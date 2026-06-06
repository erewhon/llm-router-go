// Command router is the Go rewrite of the LiteLLM proxy: the OpenAI-compatible
// front door for the fleet. It reads models.yaml directly and routes each
// request to the right upstream — local node backend, tool proxy, or external
// API — streaming SSE through untouched. See docs/PLAN.md (Phase 3).
package main

import (
	"context"
	"flag"
	"fmt"
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
		addr        = fs.String("addr", ":4015", "listen address (cutover runs parallel to LiteLLM:4010)")
		modelsYAML  = fs.String("models-yaml", "/etc/llm-router/models.yaml", "path to models.yaml")
		mode        = fs.String("mode", "", `mode tag filter ("big"/"default"/...); empty = all enabled models`)
		logLevel    = fs.String("log-level", "info", "log level: debug, info, warn, error")
		logFormat   = fs.String("log-format", "json", "log format: json or text")
		shutdownTo  = fs.Duration("shutdown-timeout", 5*time.Second, "graceful shutdown deadline")
		postgresDSN = fs.String("postgres-dsn", "", `Postgres DSN for request logging (e.g. "postgres://user:pw@host/db"); empty disables`)

		// /.well-known/opencode (3b.iv). Empty -wellknown-provider-id disables.
		wellKnownProviderID   = fs.String("wellknown-provider-id", "", `provider key under "provider" in /.well-known/opencode (e.g. "llm"); empty disables the endpoint`)
		wellKnownProviderName = fs.String("wellknown-provider-name", "LLM Router", "human label OpenCode shows for the provider")
		wellKnownBaseURL      = fs.String("wellknown-base-url", "", "public OpenAI-compatible URL OpenCode hits (e.g. https://llm.bcc.sh/v1)")
		wellKnownAPIKey       = fs.String("wellknown-api-key", "", "bearer OpenCode should send; empty omits the apiKey field")

		// API key auth. Empty list disables auth — anyone reachable can call
		// /v1/*. /health, /metrics, /.well-known/opencode are always exempt.
		apiKeys = fs.String("api-keys", "", `comma-separated bearer tokens accepted on /v1/*; empty disables auth (NOT recommended on shared networks)`)

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
			logger.Error("reqlog postgres failed", "err", err, "dsn", reqlog.RedactDSN(*postgresDSN))
			return 1
		}
		sink = ps
		defer ps.Close()
		logger.Info("reqlog enabled", "dsn", reqlog.RedactDSN(*postgresDSN))
	}
	routerOpts = append(routerOpts, router.WithSink(sink))

	rt := router.New(registry, logger, routerOpts...)

	authKeys := splitCSV(*apiKeys)
	if len(authKeys) == 0 {
		logger.Warn("API key auth DISABLED — anyone reachable on :4010 can call the proxy; set --api-keys to enable")
	} else {
		logger.Info("API key auth enabled", "keys", len(authKeys))
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

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
