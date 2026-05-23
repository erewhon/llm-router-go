// Command node-agent serves the per-machine HTTP API that manages
// inference backends on a single node. See docs/PLAN.md Phase 1.
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
	"github.com/erewhon/llm-router-go/internal/nodeagent"
)

// version is overridden via -ldflags="-X main.version=$(git describe ...)".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("node-agent", flag.ContinueOnError)
	var (
		addr       = fs.String("addr", ":8100", "listen address")
		modelsYAML = fs.String("models-yaml", "/etc/llm-router/models.yaml", "path to models.yaml")
		nodeName   = fs.String("node", "", "node name in models.yaml (defaults to hostname's first label)")
		logLevel   = fs.String("log-level", "info", "log level: debug, info, warn, error")
		logFormat  = fs.String("log-format", "json", "log format: json or text")
		shutdownTo = fs.Duration("shutdown-timeout", 5*time.Second, "graceful shutdown deadline")
		showVer    = fs.Bool("version", false, "print version and exit")
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
	}).With("svc", "node-agent")

	if *nodeName == "" {
		hn, err := os.Hostname()
		if err != nil {
			logger.Error("hostname lookup failed", "err", err)
			return 1
		}
		*nodeName = strings.SplitN(hn, ".", 2)[0]
	}

	registry, err := config.Load(*modelsYAML)
	if err != nil {
		logger.Error("load registry failed", "path", *modelsYAML, "err", err)
		return 1
	}

	agent, err := nodeagent.New(registry, *nodeName, logger, version)
	if err != nil {
		logger.Error("agent init failed", "node", *nodeName, "err", err)
		return 1
	}

	handler := httpx.Chain(
		agent.Handler(),
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

	logger.Info("starting", "addr", *addr, "node", *nodeName, "version", version,
		"models_yaml", *modelsYAML)

	if err := httpx.ServeContext(ctx, srv, *shutdownTo); err != nil {
		logger.Error("server stopped with error", "err", err)
		return 1
	}
	logger.Info("shutdown complete")
	return 0
}
