package router

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/router/reqlog"
)

// chainYAML models the provider-prefix scheme: or/k3 + zen/k3 concrete
// provider entries, k3 as the bare-name virtual chain over them, and a
// non-virtual entry that is its own first candidate.
const chainYAML = `
models:
  or/k3:
    hf_repo: moonshotai/kimi-k3
    backend: external
    api_base: https://or.example/api/v1
    api_key: sk-literal-or

  zen/k3:
    hf_repo: kimi-k3
    backend: external
    api_base: https://zen.example/v1
    api_key: sk-literal-zen

  k3:
    hf_repo: k3-virtual
    backend: external
    aliases: [k3-alias]
    fallbacks: [or/k3, zen/k3]

  self-first:
    hf_repo: self/first
    backend: external
    api_base: https://primary.example/v1
    api_key: sk-literal-self
    fallbacks: [zen/k3]

  plain:
    hf_repo: plain/model
    backend: external
    api_base: https://plain.example/v1
    api_key: sk-literal-plain
`

func chainRegistry(t *testing.T) *config.ModelRegistry {
	t.Helper()
	reg, err := config.LoadBytes([]byte(chainYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	return reg
}

func newChainRouter(t *testing.T, transport http.RoundTripper, extra ...Option) *Router {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := []Option{WithFlushInterval(0)}
	if transport != nil {
		opts = append(opts, WithTransport(transport))
	}
	opts = append(opts, extra...)
	return New(chainRegistry(t), logger, opts...)
}

// hostTransport routes upstream requests to different test servers by the
// host the router resolved, so a chain's providers can behave differently.
type hostTransport struct{ byHost map[string]string }

func (h *hostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	to, ok := h.byHost[req.URL.Host]
	if !ok {
		return nil, &net_OpErrorDial{}
	}
	target, _ := http.NewRequest(req.Method, to+req.URL.Path, req.Body)
	target.Header = req.Header
	target.ContentLength = req.ContentLength
	return http.DefaultTransport.RoundTrip(target)
}

// net_OpErrorDial stands in for an unreachable host at the transport level.
type net_OpErrorDial struct{}

func (*net_OpErrorDial) Error() string   { return "dial tcp: connection refused" }
func (*net_OpErrorDial) Timeout() bool   { return false }
func (*net_OpErrorDial) Temporary() bool { return false }

// ---------------------------------------------------------------------------
// config validation
// ---------------------------------------------------------------------------

func TestChainConfig_Valid(t *testing.T) {
	reg := chainRegistry(t)
	if !reg.Models["k3"].IsVirtual() {
		t.Errorf("k3 should be virtual (fallbacks, no backend placement)")
	}
	if reg.Models["self-first"].IsVirtual() {
		t.Errorf("self-first has its own api_base; must not be virtual")
	}
}

func TestChainConfig_Rejections(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"unknown member", `
models:
  v:
    hf_repo: v
    fallbacks: [nope]
`, `fallback "nope" is not a known model`},
		{"self reference", `
models:
  v:
    hf_repo: v
    fallbacks: [v]
`, "fallback names itself"},
		{"nested chain", `
models:
  a:
    hf_repo: a
    backend: external
    api_base: https://a.example/v1
  v1:
    hf_repo: v1
    fallbacks: [a]
  v2:
    hf_repo: v2
    fallbacks: [v1]
`, "chains must be flat"},
		{"class mismatch", `
models:
  emb:
    hf_repo: emb
    backend: external
    api_base: https://e.example/v1
    api_class: embeddings
  v:
    hf_repo: v
    fallbacks: [emb]
`, `api_class "embeddings", chain requires "chat"`},
	}
	for _, c := range cases {
		_, err := config.LoadBytes([]byte(c.yaml))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", c.name, err, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// resolution
// ---------------------------------------------------------------------------

func TestChainResolvesToFirstProvider(t *testing.T) {
	rt := newChainRouter(t, nil)
	for _, name := range []string{"k3", "k3-alias"} {
		res, err := rt.resolveModel(name, false, 0, tierNone)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.ModelID != "or/k3" || res.Chain != "k3" {
			t.Errorf("%s: resolved %q chain %q, want or/k3 via chain k3", name, res.ModelID, res.Chain)
		}
		if len(res.Remaining) != 1 || res.Remaining[0].ModelID != "zen/k3" {
			t.Errorf("%s: Remaining = %+v, want [zen/k3]", name, res.Remaining)
		}
		if res.BackendModel != "moonshotai/kimi-k3" {
			t.Errorf("%s: BackendModel = %q, want the provider's hf_repo", name, res.BackendModel)
		}
	}
}

func TestChainSelfFirstEntryIsOwnPrimary(t *testing.T) {
	rt := newChainRouter(t, nil)
	res, err := rt.resolveModel("self-first", false, 0, tierNone)
	if err != nil {
		t.Fatal(err)
	}
	if res.ModelID != "self-first" || res.Chain != "self-first" {
		t.Errorf("resolved %q chain %q, want self-first as its own primary", res.ModelID, res.Chain)
	}
	if len(res.Remaining) != 1 || res.Remaining[0].ModelID != "zen/k3" {
		t.Errorf("Remaining = %+v, want [zen/k3]", res.Remaining)
	}
}

// downAvail marks a fixed set of models unroutable.
type downAvail struct{ down map[string]bool }

func (d downAvail) Routable(id string) bool   { return !d.down[id] }
func (d downAvail) Reason(id string) string   { return "marked down" }
func (downAvail) ReportFailure(string, error) {}
func (downAvail) ReportSuccess(string)        {}

func TestChainSkipsUnroutableProvider(t *testing.T) {
	rt := newChainRouter(t, nil, WithAvailability(downAvail{down: map[string]bool{"or/k3": true}}))
	res, err := rt.resolveModel("k3", false, 0, tierNone)
	if err != nil {
		t.Fatal(err)
	}
	if res.ModelID != "zen/k3" {
		t.Errorf("resolved %q, want zen/k3 (or/k3 is down)", res.ModelID)
	}
}

func TestChainAllDownIs503(t *testing.T) {
	rt := newChainRouter(t, nil, WithAvailability(downAvail{down: map[string]bool{"or/k3": true, "zen/k3": true}}))
	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"k3","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no available provider") {
		t.Errorf("body = %q, want per-provider reasons", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// runtime failover
// ---------------------------------------------------------------------------

const okBody = `{"id":"x","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

func jsonServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestChainFailsOverOn500(t *testing.T) {
	primary := jsonServer(t, 500, `{"error":{"message":"hard down"}}`)
	defer primary.Close()
	secondary := jsonServer(t, 200, okBody)
	defer secondary.Close()

	sink := &reqlog.MemorySink{}
	rt := newChainRouter(t, &hostTransport{byHost: map[string]string{
		"or.example":  primary.URL,
		"zen.example": secondary.URL,
	}}, WithSink(sink))

	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"k3","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the fallback provider; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Router-Chain"); got != "k3" {
		t.Errorf("X-Router-Chain = %q, want k3", got)
	}
	if got := rec.Header().Get("X-Router-Resolved"); got != "zen/k3" {
		t.Errorf("X-Router-Resolved = %q, want zen/k3", got)
	}

	r := sink.Records()[0]
	if r.ResolvedVia != "zen/k3" || r.FailoverFrom != "or/k3" {
		t.Errorf("record resolved=%q failover_from=%q, want zen/k3 from or/k3", r.ResolvedVia, r.FailoverFrom)
	}
	if r.Status != 200 || r.ErrorClass != "" {
		t.Errorf("record status=%d class=%q, want clean 200", r.Status, r.ErrorClass)
	}

	rows := rt.upstreamStats.stats(time.Now())
	byModel := map[string]upstreamRow{}
	for _, row := range rows {
		byModel[row.Model] = row
	}
	if byModel["or/k3"].Failed1h != 1 {
		t.Errorf("or/k3 stats = %+v, want the failed attempt counted", byModel["or/k3"])
	}
	if byModel["zen/k3"].Total1h != 1 || byModel["zen/k3"].Failed1h != 0 {
		t.Errorf("zen/k3 stats = %+v, want one clean attempt", byModel["zen/k3"])
	}
}

func TestChainFailsOverOnErrorEnvelopeIn200(t *testing.T) {
	primary := jsonServer(t, 200, `{"error":{"type":"upstream_error","message":"claude is down"}}`)
	defer primary.Close()
	secondary := jsonServer(t, 200, okBody)
	defer secondary.Close()

	rt := newChainRouter(t, &hostTransport{byHost: map[string]string{
		"or.example":  primary.URL,
		"zen.example": secondary.URL,
	}})
	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"k3","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp struct {
		Choices []struct {
			Message struct{ Content string }
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Choices) == 0 {
		t.Fatalf("body = %s, want the fallback's real completion", rec.Body.String())
	}
}

func TestChainLastProviderErrorPassesThrough(t *testing.T) {
	primary := jsonServer(t, 500, `{"error":{"message":"or down"}}`)
	defer primary.Close()
	secondary := jsonServer(t, 503, `{"error":{"message":"zen down too"}}`)
	defer secondary.Close()

	rt := newChainRouter(t, &hostTransport{byHost: map[string]string{
		"or.example":  primary.URL,
		"zen.example": secondary.URL,
	}})
	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"k3","messages":[{"role":"user","content":"hi"}]}`)
	// The LAST provider's own error must reach the client verbatim — not a
	// synthetic 502 about a suppressed body.
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the final provider's 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "zen down too") {
		t.Errorf("body = %q, want the final provider's error body", rec.Body.String())
	}
}

func TestPlainModel500StillPassesThrough(t *testing.T) {
	up := jsonServer(t, 500, `{"error":{"message":"boom"}}`)
	defer up.Close()
	rt := newChainRouter(t, &hostTransport{byHost: map[string]string{"plain.example": up.URL}})
	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"plain","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 passthrough (no chain, no retry)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("body = %q, want the upstream's own error", rec.Body.String())
	}
}

func TestChainTransportFailureFailsOver(t *testing.T) {
	secondary := jsonServer(t, 200, okBody)
	defer secondary.Close()
	// or.example missing from the map -> dial-style transport error.
	rt := newChainRouter(t, &hostTransport{byHost: map[string]string{"zen.example": secondary.URL}})
	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"k3","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 via fallback after transport failure", rec.Code)
	}
}
