package router

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/erewhon/llm-router-go/internal/config"
)

// Two local seats behind one role, each with its own request_defaults, plus
// an alias override on the first — the GLM-on-the-Sparks shape: one server,
// a fast (thinking-off) name and a thinking name.
const requestDefaultsRegistryYAML = `
nodes:
  archimedes: {host: archimedes.local, gpu: nvidia, vram_gb: 128}
  hypatia:    {host: hypatia.local,    gpu: nvidia, vram_gb: 128}

models:
  glm-spark:
    hf_repo: glm-5.3-flash
    backend: vllm
    node: archimedes
    aliases: [glm-fast, glm-think]
    capabilities: [text, tool_calling]
    request_defaults:
      temperature: 1.0
      top_p: 0.95
      chat_template_kwargs: {enable_thinking: false, reasoning_effort: max}
    alias_overrides:
      glm-think:
        request_defaults:
          thinking_token_budget: 3000
          chat_template_kwargs: {enable_thinking: true}
  qwen-hypatia:
    hf_repo: Qwen/Qwen3.6-35B-A3B
    backend: vllm
    node: hypatia
    capabilities: [text, tool_calling]
    request_defaults:
      temperature: 0.6
      min_p: 0.01

roles:
  coder:
    require: {locality: local}
    candidates: [qwen-hypatia, glm-spark]
`

// bodyCapturingBackends refuses the dead hosts and records, per host, the
// JSON body each live one received.
type bodyCapturingBackends struct {
	dead map[string]bool

	mu     sync.Mutex
	bodies map[string][]map[string]any
}

func (b *bodyCapturingBackends) RoundTrip(req *http.Request) (*http.Response, error) {
	if b.dead[req.URL.Host] {
		return nil, fmt.Errorf("dial tcp %s: connect: connection refused", req.URL.Host)
	}
	var body map[string]any
	_ = json.NewDecoder(req.Body).Decode(&body)
	b.mu.Lock()
	if b.bodies == nil {
		b.bodies = map[string][]map[string]any{}
	}
	b.bodies[req.URL.Host] = append(b.bodies[req.URL.Host], body)
	b.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"x","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)),
		Request:    req,
	}, nil
}

func (b *bodyCapturingBackends) last(host string) map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n := len(b.bodies[host]); n > 0 {
		return b.bodies[host][n-1]
	}
	return nil
}

func newRequestDefaultsRouter(t *testing.T, tp http.RoundTripper) *Router {
	t.Helper()
	reg, err := config.LoadBytes([]byte(requestDefaultsRegistryYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	return New(reg, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithMode("default"), WithFlushInterval(0), WithTransport(tp))
}

func kwargs(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	kw, ok := body["chat_template_kwargs"].(map[string]any)
	if !ok {
		t.Fatalf("forwarded body has no chat_template_kwargs object: %#v", body)
	}
	return kw
}

func TestRequestDefaultsFillTheForwardedBody(t *testing.T) {
	tp := &bodyCapturingBackends{}
	rt := newRequestDefaultsRouter(t, tp)

	rec := postTo(t, rt, "/v1/chat/completions", `{"model":"glm-spark","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
	}
	got := tp.last("archimedes.local:5391")
	if got["temperature"] != 1.0 || got["top_p"] != 0.95 {
		t.Errorf("sampling defaults not filled: %#v", got)
	}
	if kw := kwargs(t, got); kw["enable_thinking"] != false || kw["reasoning_effort"] != "max" {
		t.Errorf("chat_template_kwargs = %#v, want the model's defaults", kw)
	}
	if _, present := got["thinking_token_budget"]; present {
		t.Error("alias-level default reached a request that named the model directly")
	}
	if got["model"] != "glm-5.3-flash" {
		t.Errorf("model = %v, want the backend name rewritten after the merge", got["model"])
	}
}

func TestCallerFieldsWinOverRequestDefaults(t *testing.T) {
	tp := &bodyCapturingBackends{}
	rt := newRequestDefaultsRouter(t, tp)

	postTo(t, rt, "/v1/chat/completions",
		`{"model":"glm-spark","temperature":0.2,"chat_template_kwargs":{"reasoning_effort":"low"},"messages":[]}`)
	got := tp.last("archimedes.local:5391")
	if got["temperature"] != 0.2 {
		t.Errorf("temperature = %v, want the caller's 0.2", got["temperature"])
	}
	if got["top_p"] != 0.95 {
		t.Errorf("top_p = %v, want the default to fill the field the caller skipped", got["top_p"])
	}
	kw := kwargs(t, got)
	if kw["reasoning_effort"] != "low" {
		t.Errorf("nested caller key lost: %#v", kw)
	}
	if kw["enable_thinking"] != false {
		t.Errorf("nested default not filled beside the caller's key: %#v", kw)
	}
}

func TestAliasOverrideRequestDefaultsApplyByName(t *testing.T) {
	tp := &bodyCapturingBackends{}
	rt := newRequestDefaultsRouter(t, tp)

	postTo(t, rt, "/v1/chat/completions", `{"model":"glm-think","messages":[]}`)
	got := tp.last("archimedes.local:5391")
	if got["thinking_token_budget"] != float64(3000) {
		t.Errorf("alias default missing: %#v", got)
	}
	kw := kwargs(t, got)
	if kw["enable_thinking"] != true {
		t.Errorf("alias must flip enable_thinking: %#v", kw)
	}
	if kw["reasoning_effort"] != "max" || got["top_p"] != 0.95 {
		t.Errorf("model-level defaults must survive under the alias: %#v", got)
	}

	// The other alias carries only the model's defaults.
	postTo(t, rt, "/v1/chat/completions", `{"model":"glm-fast","messages":[]}`)
	got = tp.last("archimedes.local:5391")
	if _, present := got["thinking_token_budget"]; present {
		t.Error("glm-think's override leaked onto glm-fast")
	}
	if kwargs(t, got)["enable_thinking"] != false {
		t.Error("glm-fast should think = false from the model defaults")
	}
}

func TestRequestDefaultsFollowTheSeatOnFailover(t *testing.T) {
	// The role prefers hypatia (temperature 0.6, min_p); it is dead, so the
	// request fails over to the Spark seat, which must receive ITS defaults
	// and nothing of hypatia's.
	tp := &bodyCapturingBackends{dead: map[string]bool{"hypatia.local:5391": true}}
	rt := newRequestDefaultsRouter(t, tp)

	rec := postTo(t, rt, "/v1/chat/completions", `{"model":"coder","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover (body %s)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Router-Resolved") != "glm-spark" {
		t.Fatalf("resolved = %q, want glm-spark", rec.Header().Get("X-Router-Resolved"))
	}
	got := tp.last("archimedes.local:5391")
	if got["temperature"] != 1.0 || got["top_p"] != 0.95 {
		t.Errorf("second seat's defaults not applied: %#v", got)
	}
	if _, leaked := got["min_p"]; leaked {
		t.Errorf("first seat's min_p leaked into the failover body: %#v", got)
	}
	if got["model"] != "glm-5.3-flash" {
		t.Errorf("model = %v after failover", got["model"])
	}
}

func TestRequestDefaultsOnANonChatEndpoint(t *testing.T) {
	// /v1/completions goes through the same handler with forceDirect; the
	// entry's defaults apply there too (the engine accepts the same sampling
	// fields), and "prompt" stays the caller's.
	tp := &bodyCapturingBackends{}
	rt := newRequestDefaultsRouter(t, tp)

	postTo(t, rt, "/v1/completions", `{"model":"glm-spark","prompt":"def f():"}`)
	got := tp.last("archimedes.local:5391")
	if got["prompt"] != "def f():" || got["top_p"] != 0.95 {
		t.Errorf("completions body = %#v", got)
	}
}
