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
		showVer        = fs.Bool("version", false, "print version and exit")
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

	proxy := toolproxy.New(registry, logger,
		toolproxy.WithTools(reg),
		toolproxy.WithMaxToolRounds(*maxToolRounds),
		toolproxy.WithBackendTimeout(*backendTimeout),
	)

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

	logger.Info("starting", "addr", *addr, "version", version, "models_yaml", *modelsYAML)
	if err := httpx.ServeContext(ctx, srv, *shutdownTo); err != nil {
		logger.Error("server stopped with error", "err", err)
		return 1
	}
	logger.Info("shutdown complete")
	return 0
}
