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
	"github.com/erewhon/llm-router-go/internal/nodeagent/backends/sglang"
	"github.com/erewhon/llm-router-go/internal/nodeagent/gpu"
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
		probeHost  = fs.String("probe-host", "localhost", "host name backend probes target")
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

	nodeDef := registry.Nodes[*nodeName]

	opts := []nodeagent.Option{
		// SGLang on the Sparks (and the legacy vLLM image) both advertise
		// over the same OpenAI-shaped /v1/models + Prometheus /metrics
		// protocol, so one driver covers BackendVLLM today. llama.cpp's
		// llama-server (e.g. hekaton's CPU-served MiniMax) speaks the same
		// shape, so the same driver probes it.
		nodeagent.WithBackend(config.BackendVLLM, sglang.New(*probeHost)),
	}

	// GPU snapshot for /health, installed only on nodes that actually have a
	// GPU. A CPU-only node (gpu: none) gets no reader, so /health cleanly
	// omits the gpu_* fields instead of erroring on every probe. When such a
	// node gains a card, flip its models.yaml gpu: to the real vendor.
	//
	// The vendor comes from the registry; the per-node vram_gb is a fallback
	// when xpu-smi discovery can't determine total VRAM on Arc.
	//
	// Cached: the GPU probe is the one slow part of /health (Intel xpu-smi can
	// take ~1.5s, right at the dashboard's 1.5s probe timeout). The cache runs
	// it at most once per TTL and serves the last snapshot otherwise, so
	// /health stays fast. Mirrors the Python agent's get_gpu_info_cached fix.
	if nodeDef.GPU != "" && nodeDef.GPU != config.GpuNone {
		opts = append(opts, nodeagent.WithGPUReader(gpu.Cached(gpu.NewReader(nodeDef.GPU, gpu.ReaderOptions{
			FallbackTotalVRAMGB: nodeDef.VRAMGB,
		}), 5*time.Second)))
	}

	agent, err := nodeagent.New(registry, *nodeName, logger, version, opts...)
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
