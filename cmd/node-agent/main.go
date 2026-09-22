// Command node-agent serves the per-machine HTTP API that manages
// inference backends on a single node.
//
// The body lives in package cli (cli.NodeAgent) so the unified pitf CLI can
// mount it; this file only stamps the version and turns the returned error
// into the exit status the binary has always used.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/erewhon/llm-router-go/cli"
)

// version is overridden via -ldflags="-X main.version=$(git describe ...)".
var version = "dev"

func main() {
	cli.Version = version
	// A background context on purpose: cli.NodeAgent installs its own signal
	// handling at the point the old main did, so startup and shutdown
	// behave exactly as before.
	err := cli.NodeAgent(context.Background(), os.Args[1:])
	var exit *cli.ExitError
	if err != nil && !errors.As(err, &exit) && !errors.Is(err, flag.ErrHelp) {
		// ExitError and ErrHelp have already printed their diagnostics.
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(cli.ExitCode(err))
}
