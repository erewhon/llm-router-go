// Command orpheus-say is a tiny command-line text-to-speech client for Orpheus.
//
// It sends text to an OpenAI-compatible /v1/audio/speech endpoint — the fleet's
// orpheus-fastapi, or a local mlx-audio server on a Mac — and plays the returned
// WAV on the system's default audio output. Works on macOS (afplay) and Linux
// (paplay/ffplay/play/aplay).
//
//	orpheus-say "Hello there"
//	echo "the sky is blue" | orpheus-say
//	orpheus-say --voice leo --url http://localhost:8000 "point me anywhere"
//	orpheus-say --out /tmp/a.wav "save instead of play"
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// version is overridden via -ldflags="-X main.version=...".
var version = "dev"

const (
	// defaultURL is the fleet orpheus-fastapi (hypatia). Override with --url or
	// $ORPHEUS_URL — e.g. a local `mlx_audio.server` on a Mac (:8000).
	defaultURL   = "http://192.168.42.52:5397"
	defaultVoice = "tara"
	defaultModel = "orpheus"
	// maxChunkBytes bounds a single synth request; longer sentences are split.
	maxChunkBytes = 400
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	fs := flag.NewFlagSet("orpheus-say", flag.ContinueOnError)
	var (
		url     = fs.String("url", envOr("ORPHEUS_URL", defaultURL), "Orpheus base URL serving /v1/audio/speech ($ORPHEUS_URL)")
		voice   = fs.String("voice", envOr("ORPHEUS_VOICE", defaultVoice), "voice ($ORPHEUS_VOICE)")
		model   = fs.String("model", envOr("ORPHEUS_MODEL", defaultModel), "model name ($ORPHEUS_MODEL)")
		out     = fs.String("out", "", "write WAV to this file instead of playing")
		player  = fs.String("player", os.Getenv("ORPHEUS_PLAYER"), "audio player command; the file path is appended ($ORPHEUS_PLAYER)")
		noChunk = fs.Bool("no-chunk", false, "send all text in one request instead of splitting into sentences")
		timeout = fs.Duration("timeout", 600*time.Second, "per-request HTTP timeout")
		showVer = fs.Bool("version", false, "print version and exit")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: orpheus-say [flags] [text...]

Speak text with Orpheus. Text is taken from the arguments, or from stdin if
none are given (or if the only argument is "-").

Examples:
  orpheus-say "Hello there"
  echo "the sky is blue" | orpheus-say
  orpheus-say --voice leo --out /tmp/a.wav "save, don't play"
  ORPHEUS_URL=http://localhost:8000 orpheus-say "use a local mlx server"

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVer {
		fmt.Println(version)
		return 0
	}

	text, err := gatherText(fs.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "orpheus-say:", err)
		return 2
	}
	if strings.TrimSpace(text) == "" {
		fmt.Fprintln(os.Stderr, "orpheus-say: no text (pass arguments or pipe text on stdin)")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c := &client{
		base:  normalizeBase(*url),
		voice: *voice,
		model: *model,
		http:  &http.Client{Timeout: *timeout},
	}

	// --out: synth the whole text once and write it, no playback.
	if *out != "" {
		wav, err := c.synth(ctx, text)
		if err != nil {
			fmt.Fprintln(os.Stderr, "orpheus-say:", err)
			return 1
		}
		if err := os.WriteFile(*out, wav, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "orpheus-say:", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", *out, len(wav))
		return 0
	}

	// Resolve the player up front so we fail fast if none is available.
	play, err := resolvePlayer(*player)
	if err != nil {
		fmt.Fprintln(os.Stderr, "orpheus-say:", err)
		return 1
	}

	chunks := []string{strings.TrimSpace(text)}
	if !*noChunk {
		if s := splitSentences(text); len(s) > 0 {
			chunks = s
		}
	}

	tmp, err := os.MkdirTemp("", "orpheus-say-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "orpheus-say:", err)
		return 1
	}
	defer os.RemoveAll(tmp)

	// Pipeline: a producer synthesizes chunk i+1 while the consumer plays chunk
	// i, so audio starts after just the first chunk's synth latency and then
	// flows continuously. Chunks are kept strictly in order.
	type item struct {
		path string
		err  error
	}
	ch := make(chan item, 1)
	go func() {
		defer close(ch)
		for i, chunk := range chunks {
			wav, err := c.synth(ctx, chunk)
			if err != nil {
				ch <- item{err: err}
				return
			}
			p := filepath.Join(tmp, fmt.Sprintf("%04d.wav", i))
			if err := os.WriteFile(p, wav, 0o644); err != nil {
				ch <- item{err: err}
				return
			}
			select {
			case ch <- item{path: p}:
			case <-ctx.Done():
				return
			}
		}
	}()

	for it := range ch {
		if it.err != nil {
			if ctx.Err() != nil {
				return 130
			}
			fmt.Fprintln(os.Stderr, "orpheus-say:", it.err)
			return 1
		}
		if err := play(ctx, it.path); err != nil {
			if ctx.Err() != nil {
				return 130
			}
			fmt.Fprintln(os.Stderr, "orpheus-say: play:", err)
			return 1
		}
		_ = os.Remove(it.path)
	}
	return 0
}

// client speaks the OpenAI /v1/audio/speech protocol.
type client struct {
	base  string // base URL with any /v1 suffix stripped
	voice string
	model string
	http  *http.Client
}

func (c *client) synth(ctx context.Context, text string) ([]byte, error) {
	body, err := json.Marshal(map[string]string{
		"model": c.model,
		"input": text,
		"voice": c.voice,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/audio/speech", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request to %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		snippet := strings.TrimSpace(string(data))
		if len(snippet) > 200 {
			snippet = snippet[:200] + "…"
		}
		return nil, fmt.Errorf("orpheus %s returned %s: %s", c.base, resp.Status, snippet)
	}
	return data, nil
}

// normalizeBase strips a trailing slash and an optional /v1 suffix so the caller
// can pass either "host:5397" or "host:5397/v1".
func normalizeBase(u string) string {
	u = strings.TrimRight(u, "/")
	u = strings.TrimSuffix(u, "/v1")
	return strings.TrimRight(u, "/")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// gatherText returns the text to speak: the joined arguments, or stdin when
// there are no arguments (or the only argument is "-").
func gatherText(args []string) (string, error) {
	explicitStdin := len(args) == 1 && args[0] == "-"
	if len(args) > 0 && !explicitStdin {
		return strings.Join(args, " "), nil
	}
	if !explicitStdin {
		if fi, _ := os.Stdin.Stat(); fi != nil && fi.Mode()&os.ModeCharDevice != 0 {
			return "", errors.New("no text: pass arguments or pipe text on stdin")
		}
	}
	data, err := io.ReadAll(os.Stdin)
	return string(data), err
}

// splitSentences breaks text into speakable chunks on sentence terminators and
// newlines, collapses internal whitespace, and hard-splits any chunk longer
// than maxChunkBytes on a space boundary.
func splitSentences(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var raw []string
	var b strings.Builder
	rs := []rune(text)
	for i, r := range rs {
		b.WriteRune(r)
		switch {
		case r == '\n':
			raw = append(raw, b.String())
			b.Reset()
		case r == '.' || r == '!' || r == '?':
			if i+1 >= len(rs) || isSpace(rs[i+1]) {
				raw = append(raw, b.String())
				b.Reset()
			}
		}
	}
	if b.Len() > 0 {
		raw = append(raw, b.String())
	}

	var out []string
	for _, s := range raw {
		s = strings.Join(strings.Fields(s), " ") // collapse whitespace/newlines
		if s == "" {
			continue
		}
		for len(s) > maxChunkBytes {
			cut := strings.LastIndex(s[:maxChunkBytes], " ")
			if cut <= 0 {
				cut = maxChunkBytes
				for cut > 1 && !utf8.RuneStart(s[cut]) { // don't split a rune
					cut--
				}
			}
			if head := strings.TrimSpace(s[:cut]); head != "" {
				out = append(out, head)
			}
			s = strings.TrimSpace(s[cut:])
		}
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }

// resolvePlayer returns a function that plays a WAV file on the default output.
// It honors an explicit override, else picks the first available OS player.
func resolvePlayer(override string) (func(context.Context, string) error, error) {
	if strings.TrimSpace(override) != "" {
		fields := strings.Fields(override)
		return func(ctx context.Context, file string) error {
			return runPlayer(ctx, fields[0], append(append([]string{}, fields[1:]...), file)...)
		}, nil
	}

	type cand struct {
		name string
		args []string // fixed args before the file path
	}
	cands := []cand{
		{"paplay", nil},
		{"ffplay", []string{"-nodisp", "-autoexit", "-loglevel", "quiet"}},
		{"play", []string{"-q"}}, // sox
		{"aplay", []string{"-q"}},
	}
	if runtime.GOOS == "darwin" {
		cands = []cand{{"afplay", nil}}
	}
	for _, cd := range cands {
		if path, err := exec.LookPath(cd.name); err == nil {
			cd := cd
			return func(ctx context.Context, file string) error {
				return runPlayer(ctx, path, append(append([]string{}, cd.args...), file)...)
			}, nil
		}
	}
	names := make([]string, len(cands))
	for i, cd := range cands {
		names[i] = cd.name
	}
	return nil, fmt.Errorf("no audio player found (looked for: %s); install one, set --player/$ORPHEUS_PLAYER, or use --out FILE",
		strings.Join(names, ", "))
}

func runPlayer(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stderr // keep our stdout clean; players occasionally print
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
