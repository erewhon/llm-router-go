// Package cli exposes the five llm-router-go binaries as importable
// entrypoints, so a unified CLI (github.com/erewhon/pitf) can mount them as
// subcommands without forking the module or reaching into internal/.
//
// Each entry is the whole body of the corresponding cmd/* program: it parses
// args with its own flag.FlagSet (never the global one), prints what the
// binary always printed, and reports the exit status it always used through
// the returned error — see ExitError and ExitCode. The cmd/* mains are thin
// wrappers over these functions, so standalone and mounted behaviour are the
// same code path.
//
// Signal handling is NOT installed here. The long-running servers (Router,
// NodeAgent, ToolProxy) and Say install SIGINT/SIGTERM handling themselves,
// derived from ctx at the same point their mains always did (after setup,
// just before serving), which preserves their shutdown semantics exactly:
// a signal during startup still terminates the process, and the graceful
// drain runs under --shutdown-timeout once serving. A host program that
// wants to stop an entry programmatically cancels ctx; GPUExporter, which
// never had signal handling, stops only through ctx.
package cli

import (
	"context"

	"github.com/erewhon/llm-router-go/cli/internal/exitcode"
	"github.com/erewhon/llm-router-go/cli/internal/gpuexportercmd"
	"github.com/erewhon/llm-router-go/cli/internal/nodeagentcmd"
	"github.com/erewhon/llm-router-go/cli/internal/routercmd"
	"github.com/erewhon/llm-router-go/cli/internal/saycmd"
	"github.com/erewhon/llm-router-go/cli/internal/toolproxycmd"
)

// Version is what every entry prints for --version and reports at startup.
// The cmd/* wrappers set it from their ldflags-stamped main.version before
// calling an entry; a host program sets it to its own build stamp.
var Version = "dev"

// ExitError is returned by an entry that wants a specific non-zero exit
// status (2 for a usage error, 1 for a failure, 130 when Say is
// interrupted, the --validate scripting contract). The entry has already
// printed its diagnostic exactly as the standalone binary does, so a wrapper
// should exit with Code and not print Error() again. Use errors.As.
type ExitError = exitcode.Error

// ExitCode is the process exit status the standalone binaries report for an
// entry's result: 0 for nil, ExitError.Code, 2 for flag.ErrHelp (what every
// binary has always exited with after printing usage for --help), and 1 for
// any other error — which is the only kind a wrapper needs to print.
func ExitCode(err error) int { return exitcode.Code(err) }

// Router is the body of cmd/router: the OpenAI-compatible front door.
func Router(ctx context.Context, args []string) error {
	routercmd.Version = Version
	return routercmd.Run(ctx, args)
}

// NodeAgent is the body of cmd/node-agent: the per-machine backend manager.
func NodeAgent(ctx context.Context, args []string) error {
	nodeagentcmd.Version = Version
	return nodeagentcmd.Run(ctx, args)
}

// GPUExporter is the body of cmd/gpu-exporter: the root fdinfo→/run bridge
// for Intel xe GPUs. It takes no flags and runs until ctx is cancelled.
func GPUExporter(ctx context.Context, args []string) error {
	return gpuexportercmd.Run(ctx, args)
}

// ToolProxy is the body of cmd/tool-proxy: chat completions with the
// tool-execution loop and the auto-router.
func ToolProxy(ctx context.Context, args []string) error {
	toolproxycmd.Version = Version
	return toolproxycmd.Run(ctx, args)
}

// Say is the body of cmd/orpheus-say: the Orpheus text-to-speech client.
func Say(ctx context.Context, args []string) error {
	saycmd.Version = Version
	return saycmd.Run(ctx, args)
}
