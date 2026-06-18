// Command tool-proxy is the Go rewrite of the Python tool proxy. It serves
// OpenAI chat-completions, resolving models to local backends and running the
// tool-execution loop (web_search, fetch_url, calculator, tavily_search) when
// proxy tools apply. The auto-router and full reasoning passthrough land in
// later phases (see docs/PLAN.md).
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/httpx"
	"github.com/erewhon/llm-router-go/internal/logx"
	"github.com/erewhon/llm-router-go/internal/toolproxy"
	"github.com/erewhon/llm-router-go/internal/toolproxy/egress"
	"github.com/erewhon/llm-router-go/internal/toolproxy/tools"
)

// version is overridden via -ldflags="-X main.version=$(git describe ...)".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("tool-proxy", flag.ContinueOnError)
	var (
		addr           = fs.String("addr", ":5393", "listen address")
		modelsYAML     = fs.String("models-yaml", "/etc/llm-router/models.yaml", "path to models.yaml")
		logLevel       = fs.String("log-level", "info", "log level: debug, info, warn, error")
		logFormat      = fs.String("log-format", "json", "log format: json or text")
		shutdownTo     = fs.Duration("shutdown-timeout", 5*time.Second, "graceful shutdown deadline")
		socksProxy     = fs.String("proxy", "", "SOCKS5 proxy for outbound tool requests (e.g. socks5://host:1080)")
		tavilyKey      = fs.String("tavily-key", "", "Tavily API key (falls back to TAVILY_API_KEY env)")
		maxToolRounds  = fs.Int("max-tool-rounds", 5, "max tool-execution rounds before returning")
		backendTimeout = fs.Duration("backend-timeout", 600*time.Second, "per-call timeout for backend chat-completions in the tool loop")
		toolTimeout    = fs.Duration("tool-timeout", 30*time.Second, "timeout for outbound tool HTTP requests (web search, fetch_url, tavily)")
		embedURL       = fs.String("embed-url", "http://192.168.42.240:5404", "embedding backend URL for the auto-router")
		embedModel     = fs.String("embed-model", "qwen3-embedding-4b", "embedding model served by --embed-url")
		embedTimeout   = fs.Duration("embed-timeout", 5*time.Second, "timeout for auto-router embedding requests")
		litellmURL     = fs.String("litellm-url", "http://euclid.local:4010", "LiteLLM URL the auto-router redirects resolved aliases to")
		litellmKey     = fs.String("litellm-key", "sk-litellm-master", "LiteLLM bearer key (falls back to LITELLM_KEY env)")

		// Per-request VPN egress selection (X-Egress header). See
		// docs/tool-proxy-egress.md. Inert unless a request sends the header.
		egressEnabled    = fs.Bool("egress-enabled", true, "honour the X-Egress header to pick a Mullvad exit per request (needs --proxy)")
		mullvadRelaysURL = fs.String("mullvad-relays-url", egress.DefaultRelaysURL, "Mullvad relay list URL (carries socks_name/socks_port)")
		egressCacheTTL   = fs.Duration("egress-cache-ttl", time.Hour, "how long to cache the Mullvad relay list")
		egressDefault    = fs.String("egress-default", "", "egress spec when a request sends no X-Egress (empty = current default exit)")
		egressMaxTries   = fs.Int("egress-max-tries", 3, "max relays to try per request before failing (failover on a dead relay)")

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
	}).With("svc", "tool-proxy")

	registry, err := config.Load(*modelsYAML)
	if err != nil {
		logger.Error("load registry failed", "path", *modelsYAML, "err", err)
		return 1
	}

	// Outbound tool HTTP client — shared by every network-touching tool so the
	// VPN container sees one connection pool. The auto-router (2c) will reuse it.
	toolClient, err := tools.NewHTTPClient(tools.HTTPClientConfig{
		SOCKS5:  *socksProxy,
		Timeout: *toolTimeout,
	})
	if err != nil {
		logger.Error("build tool http client failed", "proxy", *socksProxy, "err", err)
		return 1
	}

	reg := tools.NewRegistry()
	reg.Register(tools.Calculator())
	reg.Register(tools.WebSearch(toolClient))
	reg.Register(tools.FetchURL(toolClient))
	// Tavily only when a key is available — otherwise the model would see a
	// tool it can't use (the tool itself also guards on an empty key).
	tavily := *tavilyKey
	if tavily == "" {
		tavily = os.Getenv("TAVILY_API_KEY")
	}
	if tavily != "" {
		reg.Register(tools.Tavily(toolClient, tavily))
	}
	logger.Info("tools registered", "tools", reg.Names(), "proxy", *socksProxy)

	// Auto-router. Its embedding client talks DIRECTLY to the LAN embedder —
	// it must not route through the web tools' SOCKS5 VPN proxy. activeAliases
	// (enabled model ids + their aliases) restrict which categories the router
	// may select, so it never picks an alias whose model is disabled.
	activeAliases := map[string]bool{}
	for id, m := range registry.Models {
		if !m.Enabled {
			continue
		}
		activeAliases[id] = true
		for _, a := range m.Aliases {
			activeAliases[a] = true
		}
	}
	litellmBearer := *litellmKey
	if litellmBearer == "" {
		litellmBearer = os.Getenv("LITELLM_KEY")
	}
	embedClient := &http.Client{Timeout: *embedTimeout}
	autoRouter := toolproxy.NewAutoRouter(*embedURL, *embedModel, embedClient, logger, activeAliases)

	proxyOpts := []toolproxy.Option{
		toolproxy.WithTools(reg),
		toolproxy.WithMaxToolRounds(*maxToolRounds),
		toolproxy.WithBackendTimeout(*backendTimeout),
		toolproxy.WithAutoRouter(autoRouter),
		toolproxy.WithLiteLLM(*litellmURL, litellmBearer),
	}

	// Per-request VPN egress: needs the SOCKS5 tunnel as the "forward" dialer.
	// Inert until a request sends X-Egress (default exit = toolClient). The
	// relay catalogue is fetched DIRECT (the public Mullvad API), not via VPN.
	if *egressEnabled && *socksProxy != "" {
		forward, derr := tools.NewSOCKS5Dialer(*socksProxy)
		if derr != nil {
			logger.Error("egress: build forward dialer failed", "proxy", *socksProxy, "err", derr)
			return 1
		}
		cat := egress.NewCatalogue(egress.CatalogueConfig{
			URL:    *mullvadRelaysURL,
			TTL:    *egressCacheTTL,
			Logger: logger.With("subsys", "egress"),
		})
		sel := egress.NewSelector(egress.Config{
			Forward:       forward,
			BaseClient:    toolClient,
			Catalogue:     cat,
			DefaultSpec:   *egressDefault,
			ClientTimeout: *toolTimeout,
			MaxTries:      *egressMaxTries,
			Logger:        logger.With("subsys", "egress"),
		})
		proxyOpts = append(proxyOpts, toolproxy.WithEgress(sel))
		logger.Info("egress selection enabled", "relays_url", *mullvadRelaysURL,
			"cache_ttl", egressCacheTTL.String(), "default", *egressDefault, "max_tries", *egressMaxTries)
	} else if *egressEnabled {
		logger.Warn("egress selection requested but --proxy is empty; X-Egress will be ignored")
	}

	proxy := toolproxy.New(registry, logger, proxyOpts...)

	handler := httpx.Chain(
		proxy.Handler(),
		httpx.RequestID,
		httpx.AccessLog(logger),
		httpx.Recover(logger),
	)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Background: compute auto-router category embeddings, retrying until the
	// embedder answers. Exits when ctx is cancelled on shutdown.
	go autoRouter.RunInit(ctx)

	logger.Info("starting", "addr", *addr, "version", version, "models_yaml", *modelsYAML,
		"embed_url", *embedURL, "litellm_url", *litellmURL)
	if err := httpx.ServeContext(ctx, srv, *shutdownTo); err != nil {
		logger.Error("server stopped with error", "err", err)
		return 1
	}
	logger.Info("shutdown complete")
	return 0
}
