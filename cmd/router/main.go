// Command router is the Go rewrite of the LiteLLM proxy: the OpenAI-compatible
// front door for the fleet. It reads models.yaml directly and routes each
// request to the right upstream — local node backend, tool proxy, or external
// API — streaming SSE through untouched. See docs/PLAN.md (Phase 3).
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/httpx"
	"github.com/erewhon/llm-router-go/internal/logx"
	"github.com/erewhon/llm-router-go/internal/router"
	"github.com/erewhon/llm-router-go/internal/router/reqlog"
)

// version is overridden via -ldflags="-X main.version=$(git describe ...)".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("router", flag.ContinueOnError)
	var (
		addr         = fs.String("addr", ":4015", "listen address (cutover runs parallel to LiteLLM:4010)")
		modelsYAML   = fs.String("models-yaml", "/etc/llm-router/models.yaml", "path to models.yaml")
		mode         = fs.String("mode", "", `mode tag filter ("big"/"default"/...); empty = all enabled models`)
		logLevel     = fs.String("log-level", "info", "log level: debug, info, warn, error")
		logFormat    = fs.String("log-format", "json", "log format: json or text")
		shutdownTo   = fs.Duration("shutdown-timeout", 5*time.Second, "graceful shutdown deadline")
		postgresDSN  = fs.String("postgres-dsn", "", `Postgres DSN for request logging (e.g. "postgres://user:pw@host/db"); empty disables`)
		toolProxyURL = fs.String("tool-proxy-url", "", `address tool_proxy models route to; empty falls back to $ROUTER_TOOL_PROXY_URL, then the built-in default`)

		// /.well-known/opencode (3b.iv). Empty -wellknown-provider-id disables.
		wellKnownProviderID   = fs.String("wellknown-provider-id", "", `provider key under "provider" in /.well-known/opencode (e.g. "llm"); empty disables the endpoint`)
		wellKnownProviderName = fs.String("wellknown-provider-name", "LLM Router", "human label OpenCode shows for the provider")
		wellKnownBaseURL      = fs.String("wellknown-base-url", "", "public OpenAI-compatible URL OpenCode hits (e.g. https://llm.bcc.sh/v1)")
		wellKnownAPIKey       = fs.String("wellknown-api-key", "", "bearer OpenCode should send; empty omits the apiKey field")

		// API key auth. Empty list disables auth — anyone reachable can call
		// /v1/*. /health, /metrics, /.well-known/opencode are always exempt.
		// If --api-keys is empty, falls back to $ROUTER_API_KEYS (typical
		// pattern: load that env var from systemd's EnvironmentFile so the
		// key isn't visible in /proc/PID/cmdline).
		apiKeys = fs.String("api-keys", "", `comma-separated bearer tokens accepted on /v1/*; empty falls back to $ROUTER_API_KEYS, then disables auth`)

		// Dashboard: the status UI baked into the binary, served on its own
		// listener (separate auth boundary from /v1/*). Off by default so
		// existing deployments are unaffected; the loopback-default addr keeps
		// it safe to enable without auth on a local instance.
		dashboard     = fs.Bool("dashboard", false, "serve the status dashboard UI on --dashboard-addr")
		dashboardAddr = fs.String("dashboard-addr", "127.0.0.1:4011", "listen address for the dashboard; carries NO bearer auth, so keep it on loopback unless fronted by your own auth")
		dashboardURL  = fs.String("dashboard-public-url", "", "public OpenAI-compatible base URL shown in the dashboard's Connection card (e.g. https://llm.bcc.sh); empty derives http://localhost:<port> from --addr")

		showVer = fs.Bool("version", false, "print version and exit")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVer {
		fmt.Println(version)
		return 0
	}

	level, err := logx.ParseLevel(*logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	logger := logx.New(os.Stdout, logx.Config{
		Level:  level,
		Format: *logFormat,
		Attrs:  httpx.LogAttrsFromContext,
	}).With("svc", "router")

	registry, err := config.Load(*modelsYAML)
	if err != nil {
		logger.Error("load registry failed", "path", *modelsYAML, "err", err)
		return 1
	}

	// Env-var fallback for secrets. Standard pattern: systemd ships them
	// via EnvironmentFile so they don't appear in /proc/PID/cmdline. Flags
	// still win if set, so dev/test runs are unaffected.
	if *wellKnownAPIKey == "" {
		if v := os.Getenv("WELLKNOWN_API_KEY"); v != "" {
			*wellKnownAPIKey = v
		}
	}
	if *toolProxyURL == "" {
		*toolProxyURL = os.Getenv("ROUTER_TOOL_PROXY_URL")
	}
	if *toolProxyURL != "" {
		registry.ToolProxyAddr = *toolProxyURL
		logger.Info("tool proxy address overridden", "addr", *toolProxyURL)
	}

	routerOpts := []router.Option{
		router.WithMode(*mode),
		router.WithVersion(version),
		router.WithWellKnown(router.WellKnownConfig{
			ProviderID:   *wellKnownProviderID,
			ProviderName: *wellKnownProviderName,
			BaseURL:      *wellKnownBaseURL,
			APIKey:       *wellKnownAPIKey,
		}),
	}
	var sink reqlog.Sink = reqlog.NopSink{}
	if *postgresDSN != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		ps, err := reqlog.NewPostgres(ctx, *postgresDSN, logger.With("subsys", "reqlog"))
		cancel()
		if err != nil {
			// Soft-fail: a request-log DB outage must never take down the
			// proxy. Fall back to NopSink (sink is already NopSink) and run
			// without logging — loudly, so the gap is visible in the journal.
			logger.Warn("reqlog postgres unavailable; falling back to NopSink (requests will NOT be logged)",
				"err", err, "dsn", reqlog.RedactDSN(*postgresDSN))
		} else {
			sink = ps
			defer ps.Close()
			logger.Info("reqlog enabled", "dsn", reqlog.RedactDSN(*postgresDSN))
		}
	}
	routerOpts = append(routerOpts, router.WithSink(sink))

	rt := router.New(registry, logger, routerOpts...)

	apiKeysSrc := *apiKeys
	apiKeysFrom := "flag"
	if apiKeysSrc == "" {
		if v := os.Getenv("ROUTER_API_KEYS"); v != "" {
			apiKeysSrc = v
			apiKeysFrom = "env"
		}
	}
	authKeys := splitCSV(apiKeysSrc)
	if len(authKeys) == 0 {
		logger.Warn("API key auth DISABLED — anyone reachable on :4010 can call the proxy; set --api-keys or $ROUTER_API_KEYS to enable")
	} else {
		logger.Info("API key auth enabled", "keys", len(authKeys), "source", apiKeysFrom)
	}
	authExempt := []string{"/health", "/metrics", "/.well-known/opencode"}

	handler := httpx.Chain(
		rt.Handler(),
		httpx.RequestID,
		httpx.AccessLog(logger),
		httpx.Recover(logger),
		router.RequireBearer(authKeys, authExempt),
	)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *dashboard {
		apiBase := *dashboardURL
		if apiBase == "" {
			apiBase = deriveAPIBase(*addr)
		}
		if !isLoopbackBind(*dashboardAddr) {
			logger.Warn("dashboard listener is NOT loopback — it has no bearer auth and its /api/chat can invoke any model; front it with your own auth (oauth2-proxy) or bind 127.0.0.1",
				"addr", *dashboardAddr)
		}
		dashHandler := httpx.Chain(
			rt.DashboardHandler(router.DashboardConfig{APIBase: apiBase}),
			httpx.RequestID,
			httpx.AccessLog(logger.With("svc", "dashboard")),
			httpx.Recover(logger),
		)
		dashSrv := &http.Server{
			Addr:              *dashboardAddr,
			Handler:           dashHandler,
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			logger.Info("dashboard starting", "addr", *dashboardAddr, "public_url", apiBase)
			if err := httpx.ServeContext(ctx, dashSrv, *shutdownTo); err != nil {
				logger.Error("dashboard server stopped with error", "err", err)
				stop() // take the whole process down if the dashboard listener dies
			}
		}()
	}

	logger.Info("starting", "addr", *addr, "version", version,
		"models_yaml", *modelsYAML, "mode", *mode,
		"models", len(registry.ModelsForMode(*mode)))
	if err := httpx.ServeContext(ctx, srv, *shutdownTo); err != nil {
		logger.Error("server stopped with error", "err", err)
		return 1
	}
	logger.Info("shutdown complete")
	return 0
}

// deriveAPIBase turns a listen address (":4010", "0.0.0.0:4010",
// "192.168.42.240:4010") into the base URL a local client would hit. A
// wildcard or empty host collapses to localhost — the dashboard's Connection
// card is a copy-paste hint, and "http://0.0.0.0:4010" isn't dialable.
func deriveAPIBase(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://localhost:4010"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// isLoopbackBind reports whether addr binds only the loopback interface. An
// empty or wildcard host ("":4011, "0.0.0.0:4011") is NOT loopback — it's
// reachable from the network, so the dashboard's no-auth warning fires.
func isLoopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
