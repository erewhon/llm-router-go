package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"testing"
)

// capture runs fn with os.Stdout and os.Stderr redirected into pipes and
// returns what was written to each. The bodies print usage the way the flag
// package does — straight to the process's stderr — so the smoke tests have
// to observe the real file descriptors rather than an injected writer.
func capture(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	pipe := func(target **os.File) (<-chan string, func()) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		saved := *target
		*target = w
		ch := make(chan string, 1)
		go func() {
			var buf bytes.Buffer
			_, _ = io.Copy(&buf, r)
			ch <- buf.String()
		}()
		return ch, func() { _ = w.Close(); *target = saved }
	}
	outCh, restoreOut := pipe(&os.Stdout)
	errCh, restoreErr := pipe(&os.Stderr)
	fn()
	restoreOut()
	restoreErr()
	return <-outCh, <-errCh
}

// Every entry answers --help by printing its usage to stderr and returning
// flag.ErrHelp: the FlagSet is per call, so the wrapper (or a host program)
// decides the exit status — the standalone binaries map it to 2, as they
// always have (ExitCode documents that).
func TestHelp(t *testing.T) {
	entries := []struct {
		name  string
		entry func(context.Context, []string) error
		want  string // a line that only that command's usage carries
	}{
		{"Router", Router, "Usage of router:"},
		{"NodeAgent", NodeAgent, "Usage of node-agent:"},
		{"GPUExporter", GPUExporter, "Usage of gpu-exporter:"},
		{"ToolProxy", ToolProxy, "Usage of tool-proxy:"},
		{"Say", Say, "usage: orpheus-say [flags] [text...]"},
	}
	for _, tc := range entries {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			stdout, stderr := capture(t, func() {
				err = tc.entry(context.Background(), []string{"--help"})
			})
			if !errors.Is(err, flag.ErrHelp) {
				t.Fatalf("err = %v, want flag.ErrHelp", err)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr lacks %q:\n%s", tc.want, stderr)
			}
			if stdout != "" {
				t.Errorf("--help wrote to stdout: %q", stdout)
			}
			if got := ExitCode(err); got != 2 {
				t.Errorf("ExitCode(ErrHelp) = %d, want 2 (the binaries' historical --help status)", got)
			}
		})
	}
}

// An unknown flag is a usage error: the flag package prints the message and
// usage, and the entry reports status 2 without a second diagnostic.
func TestUnknownFlagIsUsageError(t *testing.T) {
	var err error
	_, stderr := capture(t, func() {
		err = Router(context.Background(), []string{"--no-such-flag"})
	})
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 2 {
		t.Fatalf("err = %v, want *ExitError{Code: 2}", err)
	}
	if !strings.Contains(stderr, "flag provided but not defined: -no-such-flag") {
		t.Errorf("stderr lacks the flag package's message:\n%s", stderr)
	}
}

// --version prints the forwarded Version on stdout, which is the ldflags
// contract the deploy scripts rely on (-X main.version, forwarded by the
// wrapper into cli.Version).
func TestVersionIsForwarded(t *testing.T) {
	defer func(v string) { Version = v }(Version)
	Version = "smoke-1.2.3"
	for name, entry := range map[string]func(context.Context, []string) error{
		"Router": Router, "NodeAgent": NodeAgent, "ToolProxy": ToolProxy, "Say": Say,
	} {
		var err error
		stdout, _ := capture(t, func() { err = entry(context.Background(), []string{"--version"}) })
		if err != nil {
			t.Errorf("%s --version: err = %v", name, err)
		}
		if stdout != "smoke-1.2.3\n" {
			t.Errorf("%s --version printed %q", name, stdout)
		}
	}
}

func TestExitCode(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, 0},
		{flag.ErrHelp, 2},
		{&ExitError{Code: 130}, 130},
		{errors.New("gpu-exporter: boom"), 1},
	}
	for _, tc := range cases {
		if got := ExitCode(tc.err); got != tc.want {
			t.Errorf("ExitCode(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}
