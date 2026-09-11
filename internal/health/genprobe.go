package health

// The generation probe closes the gap between "the listing is up" and "the
// seat can generate". llama-server and vLLM both answer /v1/models — and the
// node agent both reports `running` — for tens of seconds to minutes before
// the first completion would succeed: weights are still loading, the KV cache
// is still being allocated, or a worker is wedged behind a listing that keeps
// serving. During that window the router used to forward real traffic into
// 503s (2026-09-06, delphi gptoss-server restart: eight Harbor trials errored
// against `{"error":{"message":"Loading model","code":503}}`).
//
// So a seat the poller says is up is `warming`, not routable, until one
// minimal generation succeeds against its backend directly. Real traffic is
// the steady-state signal afterwards: a served request confirms the seat, and
// a 503 / connection-refused from a served request demotes it back to warming
// — not to "down" — so a reloading seat recovers on its own timescale without
// tripping the breaker or waiting for a full poll cycle.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
)

// DefaultProbeTimeout bounds one generation probe. A cold seat that has not
// produced a token in 30 s is still warming as far as routing is concerned,
// whatever the reason.
const DefaultProbeTimeout = 30 * time.Second

// Backoff between failed probes of one seat: 5 s, 10 s, 20 s, 40 s, then 60 s.
// Probes are cheap for the router but not free for a seat that is busy
// loading, and the poll interval (15 s) already bounds how often a round runs.
const (
	probeBackoffBase = 5 * time.Second
	probeBackoffCap  = 60 * time.Second
)

func probeBackoff(fails int) time.Duration {
	if fails <= 0 {
		return 0
	}
	d := probeBackoffBase << uint(fails-1)
	if d <= 0 || d > probeBackoffCap {
		return probeBackoffCap
	}
	return d
}

// GenTarget describes one generation probe: where to send it and as what.
type GenTarget struct {
	// Model is the registry id, for logs.
	Model string
	// Root is the backend's URL root — its api_base with any trailing "/v1"
	// removed, exactly as the router's BackendURL is derived — with the tool
	// proxy bypassed: the probe asks the engine itself, not the hop in front
	// of it. The class path ("/v1/chat/completions", …) is appended to it,
	// which is what makes an api_base written with or without "/v1" land on
	// the same endpoint the router forwards to.
	Root string
	// BackendModel is the name the engine knows the model by.
	BackendModel string
	APIClass     config.APIClass
	// Bearer / BearerHeader carry the resolved credential for node-pinned
	// externals that want one. Empty for the engines the agents manage.
	Bearer       string
	BearerHeader string
}

// GenProbeFunc performs one generation probe. nil means the seat generated
// (or answered from its generation path — see ProbeRejected); any other
// error means it is not ready. Injectable so tests stay hermetic.
type GenProbeFunc func(ctx context.Context, target GenTarget) error

// ProbeRejected is returned when the seat answered the probe with a 4xx: the
// generation path is alive (a loading llama-server says 503, a closed port
// refuses), it just disliked our request. The tracker counts this as warm and
// logs it, because a probe body an engine rejects is a bug to fix here, not
// a reason to keep a healthy seat out of rotation.
type ProbeRejected struct {
	Status int
	Body   string
}

func (e *ProbeRejected) Error() string {
	return fmt.Sprintf("probe rejected with %d: %s", e.Status, e.Body)
}

// errProbeStatus is a 5xx answer to a probe — the "Loading model" shape.
type errProbeStatus struct {
	Status int
	Body   string
}

func (e *errProbeStatus) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("upstream status %d", e.Status)
	}
	return fmt.Sprintf("upstream status %d: %s", e.Status, e.Body)
}

// UpstreamStatus lets isWarmingSignal read the status without importing the
// router's error type; the router's errUpstreamStatus implements the same
// method.
func (e *errProbeStatus) UpstreamStatus() int { return e.Status }

// genProbeClient has no timeout of its own: the per-probe context carries it,
// so one slow seat cannot pin the shared client's deadline for the others.
var genProbeClient = &http.Client{}

// ProbeGeneration is the real GenProbeFunc: one minimal request against the
// seat's own class endpoint, `max_tokens: 1` for chat, one short input for
// embeddings and rerank. Media classes have no probe here — generating an
// image or a clip to prove liveness is the wrong trade — and ProbeEnabled
// keeps them out by default.
func ProbeGeneration(ctx context.Context, t GenTarget) error {
	path, body, ok := genProbeRequest(t.APIClass, t.BackendModel)
	if !ok {
		return nil
	}
	url := strings.TrimSuffix(t.Root, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if t.Bearer != "" {
		if t.BearerHeader != "" {
			req.Header.Set(t.BearerHeader, t.Bearer)
		} else {
			req.Header.Set("Authorization", "Bearer "+t.Bearer)
		}
	}
	resp, err := genProbeClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	// Drain the rest (bounded) so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	switch {
	case resp.StatusCode >= 500:
		return &errProbeStatus{Status: resp.StatusCode, Body: oneLine(snippet)}
	case resp.StatusCode >= 400:
		return &ProbeRejected{Status: resp.StatusCode, Body: oneLine(snippet)}
	}
	return nil
}

// genProbeRequest returns the path and body for one class's probe, and false
// for a class with no probe.
func genProbeRequest(class config.APIClass, model string) (string, []byte, bool) {
	q := jsonString(model)
	switch class {
	case config.APIClassChat, "":
		return "/v1/chat/completions", []byte(`{"model":` + q +
			`,"messages":[{"role":"user","content":"hi"}],"max_tokens":1,"stream":false}`), true
	case config.APIClassEmbeddings:
		return "/v1/embeddings", []byte(`{"model":` + q + `,"input":"hi"}`), true
	case config.APIClassRerank:
		return "/v1/rerank", []byte(`{"model":` + q + `,"query":"hi","documents":["hi"]}`), true
	}
	return "", nil, false
}

func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func oneLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// ProbeEnabled reports whether the generation probe gates this placement.
//
// Default: on for fleet-resident chat, embeddings and rerank seats — the
// engines that answer their listing before they can generate. Off for media
// classes (a probe would render something), for the Anthropic passthrough
// (there is nothing local to warm), and for every nodeless external: those
// cost money per call, are rate-limited, and are somebody else's uptime
// problem — listing plus real traffic stays their signal. A virtual chain
// entry has no backend of its own to probe.
//
// `health.generation_probe: true|false` on the model entry overrides the
// class default in either direction.
func ProbeEnabled(m config.ModelDefinition) bool {
	if m.IsVirtual() || !m.Enabled {
		return false
	}
	if m.Health != nil && m.Health.GenerationProbe != nil {
		return *m.Health.GenerationProbe
	}
	if !m.IsLocal() {
		return false
	}
	switch m.APIClass {
	case config.APIClassChat, config.APIClassEmbeddings, config.APIClassRerank, "":
		return true
	}
	return false
}

// statusError is implemented by the router's suppressed-5xx error and by
// errProbeStatus, so the tracker can read a status without a dependency on
// either package's concrete type.
type statusError interface{ UpstreamStatus() int }

// isWarmingSignal classifies a proxy failure as "the seat is loading" rather
// than "the seat is broken": a 503, or a connection that was refused
// outright. Timeouts and other 5xx are not in this class — a seat that
// accepts a request and then dies is what the breaker is for.
func isWarmingSignal(err error) bool {
	if err == nil {
		return false
	}
	var se statusError
	if errors.As(err, &se) {
		return se.UpstreamStatus() == http.StatusServiceUnavailable
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH)
}
