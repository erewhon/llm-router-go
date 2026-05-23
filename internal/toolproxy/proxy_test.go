package toolproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erewhon/llm-router-go/internal/config"
)

const testYAML = `
nodes:
  archimedes:
    host: archimedes.local
    gpu: nvidia
    vram_gb: 128
  delphi:
    host: delphi.local
    gpu: amd
    vram_gb: 64

models:
  nemotron-3-super:
    hf_repo: nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4
    backend: vllm
    node: archimedes
    api_port: 5391
    tool_proxy: true
    aliases: [thinker, research]

  qwen-nothink:
    hf_repo: Qwen/Qwen3.5-35B#nothink
    backend: vllm
    node: archimedes
    api_port: 5391
    aliases: [coder-alt]

  flux-dev:
    hf_repo: FLUX.1-dev
    backend: external
    node: delphi
    api_base: http://delphi.local:5396
    api_class: image_gen
`

func newTestProxy(t *testing.T, upstreamRewrite http.RoundTripper) *Proxy {
	t.Helper()
	reg, err := config.LoadBytes([]byte(testYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := []Option{WithFlushInterval(0)} // tests don't need SSE flush
	if upstreamRewrite != nil {
		opts = append(opts, WithTransport(upstreamRewrite))
	}
	return New(reg, logger, opts...)
}

// transportRedirect re-routes the upstream request to a different URL.
// Used so we can run a fake upstream on a test server while pretending to
// hit archimedes.local:5391.
type transportRedirect struct {
	to string
	rt http.RoundTripper
}

func (t *transportRedirect) RoundTrip(req *http.Request) (*http.Response, error) {
	target, _ := http.NewRequest(req.Method, t.to+req.URL.Path, req.Body)
	target.Header = req.Header
	target.ContentLength = req.ContentLength
	return t.rt.RoundTrip(target)
}

// ---------------------------------------------------------------------------
// /health and /v1/models
// ---------------------------------------------------------------------------

func TestHealth(t *testing.T) {
	p := newTestProxy(t, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("body missing status:ok: %s", rec.Body.String())
	}
}

func TestModels(t *testing.T) {
	p := newTestProxy(t, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID       string `json:"id"`
			Object   string `json:"object"`
			OwnedBy  string `json:"owned_by"`
			APIClass string `json:"api_class"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Object != "list" {
		t.Errorf("object = %q, want list", resp.Object)
	}
	byID := map[string]string{}
	for _, e := range resp.Data {
		byID[e.ID] = e.APIClass
	}
	if byID["nemotron-3-super"] != "chat" {
		t.Errorf("nemotron-3-super api_class = %q, want chat", byID["nemotron-3-super"])
	}
	if byID["flux-dev"] != "image_gen" {
		t.Errorf("flux-dev api_class = %q, want image_gen", byID["flux-dev"])
	}
}

// ---------------------------------------------------------------------------
// /v1/chat/completions — model resolution + body rewrite + forward
// ---------------------------------------------------------------------------

func TestChat_ForwardsAndRewritesModel(t *testing.T) {
	// Upstream captures the body so we can assert the model was rewritten
	// and the payload made it across intact.
	type captured struct {
		path string
		body map[string]any
	}
	var got captured
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer upstream.Close()

	// Redirect upstream calls (which the proxy would send to
	// archimedes.local:5391) to our test server.
	p := newTestProxy(t, &transportRedirect{to: upstream.URL, rt: http.DefaultTransport})

	// Caller sends the registry's model_id; expect upstream to receive
	// the hf_repo.
	reqBody := `{"model":"nemotron-3-super","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	p.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got.path != "/v1/chat/completions" {
		t.Errorf("upstream path = %q, want /v1/chat/completions", got.path)
	}
	if got.body["model"] != "nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4" {
		t.Errorf("model rewrite missing: got %v", got.body["model"])
	}
}

func TestChat_StripsOpenAIPrefix(t *testing.T) {
	// LiteLLM sometimes forwards `model: openai/X`; we should still resolve.
	var receivedModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		receivedModel, _ = b["model"].(string)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	p := newTestProxy(t, &transportRedirect{to: upstream.URL, rt: http.DefaultTransport})

	reqBody := `{"model":"openai/nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4","messages":[]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	p.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if receivedModel != "nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4" {
		t.Errorf("upstream got model %q, want stripped+hf_repo", receivedModel)
	}
}

func TestChat_StripsNothinkSuffix(t *testing.T) {
	// hf_repo includes "#nothink" to pick a template; the registry stores
	// it as-is but upstream wants the bare name.
	var receivedModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		receivedModel, _ = b["model"].(string)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	p := newTestProxy(t, &transportRedirect{to: upstream.URL, rt: http.DefaultTransport})

	reqBody := `{"model":"qwen-nothink","messages":[]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	p.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if receivedModel != "Qwen/Qwen3.5-35B" {
		t.Errorf("upstream got %q, want bare hf_repo (no #nothink)", receivedModel)
	}
}

func TestChat_ResolvesByAlias(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	p := newTestProxy(t, &transportRedirect{to: upstream.URL, rt: http.DefaultTransport})

	for _, alias := range []string{"thinker", "research"} {
		reqBody := `{"model":"` + alias + `","messages":[]}`
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
		p.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("alias %q: status = %d", alias, rec.Code)
		}
	}
}

func TestChat_UnknownModelIs404(t *testing.T) {
	p := newTestProxy(t, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ghost","messages":[]}`))
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestChat_ExternalModelIs404(t *testing.T) {
	// External models manage their own routing and shouldn't be in our path.
	p := newTestProxy(t, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"flux-dev","messages":[]}`))
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for external model", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "external") {
		t.Errorf("error body doesn't mention 'external': %s", rec.Body.String())
	}
}

func TestChat_BadJSON(t *testing.T) {
	p := newTestProxy(t, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{not json`))
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestChat_MissingModelField(t *testing.T) {
	p := newTestProxy(t, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"messages":[]}`))
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// SSE streaming
// ---------------------------------------------------------------------------

func TestChat_SSEStreamsThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, chunk := range []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = w.Write([]byte(chunk))
			flusher.Flush()
		}
	}))
	defer upstream.Close()
	p := newTestProxy(t, &transportRedirect{to: upstream.URL, rt: http.DefaultTransport})

	body := `{"model":"nemotron-3-super","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	p.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	got := rec.Body.String()
	for _, want := range []string{"hel", "lo", "[DONE]"} {
		if !strings.Contains(got, want) {
			t.Errorf("stream missing %q: %s", want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Upstream error
// ---------------------------------------------------------------------------

func TestChat_UpstreamUnreachable(t *testing.T) {
	// Point at a port nothing listens on.
	bad := &transportRedirect{to: "http://127.0.0.1:1", rt: http.DefaultTransport}
	p := newTestProxy(t, bad)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"nemotron-3-super","messages":[]}`)))
	p.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}
