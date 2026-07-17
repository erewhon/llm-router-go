package router

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
	"github.com/erewhon/llm-router-go/internal/router/reqlog"
)

const testYAML = `
nodes:
  archimedes:
    host: archimedes.local
    gpu: nvidia
    vram_gb: 128
  hypatia:
    host: hypatia.local
    gpu: nvidia
    vram_gb: 128

models:
  # local, tool_proxy:true -> routes through the tool proxy, model_id preserved
  nemotron-3-super:
    hf_repo: nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4
    backend: vllm
    node: archimedes
    api_port: 5391
    tool_proxy: true
    aliases: [thinker, research]

  # local, no tool_proxy -> routes straight to the node backend, hf_repo sent
  qwen36-hypatia:
    hf_repo: Qwen/Qwen3.6-35B-A3B-FP8#nothink
    backend: vllm
    node: hypatia
    api_port: 5391
    aliases: [coder, vision]

  # external with an env-var api_key -> api_base + Bearer <env>
  zen-glm:
    hf_repo: zen/glm-4.6
    backend: external
    api_base: https://api.zen.example/v1
    api_key: ZEN_TEST_KEY
    aliases: [glm]

  # external with a literal sk- key
  zen-lit:
    hf_repo: zen/lit
    backend: external
    api_base: https://api.lit.example/v1
    api_key: sk-literal-123

  # external with a custom auth header (raw key, no Bearer prefix)
  zen-hdr:
    hf_repo: zen/hdr
    backend: external
    api_base: https://api.hdr.example/v1
    api_key: ZEN_TEST_KEY
    api_key_header: X-Api-Key
    aliases: [hdr]

  # external pointing at the tool proxy (auto-router stub)
  auto:
    hf_repo: auto
    backend: external
    api_base: http://192.168.42.240:5392/v1

  # Anthropic Messages passthrough target (single gateway)
  anthropic-gateway:
    hf_repo: anthropic-passthrough
    backend: external
    api_base: https://api.anthropic.example/v1
    api_class: anthropic

  # disabled -> not routable, not listed
  ghost-disabled:
    hf_repo: ghost/x
    backend: vllm
    node: archimedes
    enabled: false

  # mode-tagged "big" -> excluded when mode=default
  big-only:
    hf_repo: big/model
    backend: vllm
    node: archimedes
    tags: [mode:big]

  # embedding-class model (OpenArc-style on /v1)
  qwen3-embedding:
    hf_repo: Qwen/Qwen3-Embedding-4B
    backend: external
    api_base: http://euclid.local:5404/v1
    api_class: embeddings
    aliases: [embedding]

  # rerank-class model
  qwen3-reranker:
    hf_repo: Qwen/Qwen3-Reranker-4B
    backend: external
    api_base: http://euclid.local:5404/v1
    api_class: rerank
`

func testRegistry(t *testing.T) *config.ModelRegistry {
	t.Helper()
	reg, err := config.LoadBytes([]byte(testYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	return reg
}

func newTestRouter(t *testing.T, transport http.RoundTripper, extra ...Option) *Router {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := []Option{
		WithFlushInterval(0), // tests don't need SSE flush
		WithGetenv(func(k string) string {
			if k == "ZEN_TEST_KEY" {
				return "secret-key"
			}
			return ""
		}),
	}
	if transport != nil {
		opts = append(opts, WithTransport(transport))
	}
	opts = append(opts, extra...)
	return New(testRegistry(t), logger, opts...)
}

// transportRedirect re-routes the upstream request to a test server URL,
// preserving the path the router chose.
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
	rt := newTestRouter(t, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rt.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("body missing status:ok: %s", rec.Body.String())
	}
}

func TestModels_ListsActiveExcludesDisabled(t *testing.T) {
	rt := newTestRouter(t, nil) // mode "" => all enabled
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rt.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID       string `json:"id"`
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
	got := map[string]bool{}
	for _, e := range resp.Data {
		got[e.ID] = true
	}
	for _, want := range []string{"nemotron-3-super", "qwen36-hypatia", "zen-glm", "auto", "big-only"} {
		if !got[want] {
			t.Errorf("model %q missing from /v1/models", want)
		}
	}
	if got["ghost-disabled"] {
		t.Errorf("disabled model leaked into /v1/models")
	}
	// Aliases must surface alongside canonical IDs (parity with LiteLLM):
	// nemotron-3-super → thinker, research; qwen36-hypatia → coder, vision;
	// zen-glm → glm; qwen3-embedding → embedding (the only non-chat alias).
	for _, alias := range []string{"thinker", "research", "coder", "vision", "glm", "embedding"} {
		if !got[alias] {
			t.Errorf("alias %q missing from /v1/models", alias)
		}
	}
	// Alias entries should mirror the backend/api_class of their canonical
	// model. Spot-check `coder` (alias of qwen36-hypatia, chat-class).
	for _, e := range resp.Data {
		if e.ID == "coder" {
			if e.APIClass != "chat" {
				t.Errorf("alias 'coder' api_class = %q, want chat", e.APIClass)
			}
		}
	}
}

func TestModels_ModeFilterExcludesOtherMode(t *testing.T) {
	rt := newTestRouter(t, nil, WithMode("default"))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rt.Handler().ServeHTTP(rec, req)
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	for _, e := range resp.Data {
		if e.ID == "big-only" {
			t.Errorf("mode=default should exclude mode:big model, but big-only is listed")
		}
	}
}

// ---------------------------------------------------------------------------
// /v1/chat/completions — forwarding + body rewrite
// ---------------------------------------------------------------------------

// captureUpstream returns a test server that records the path, decoded body,
// and Authorization header of the request it receives.
func captureUpstream(t *testing.T, path *string, body *map[string]any, auth *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if path != nil {
			*path = r.URL.Path
		}
		if auth != nil {
			*auth = r.Header.Get("Authorization")
		}
		if body != nil {
			_ = json.NewDecoder(r.Body).Decode(body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"hi"}}]}`))
	}))
}

func postChat(t *testing.T, rt *Router, jsonBody string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	rt.Handler().ServeHTTP(rec, req)
	return rec
}

func TestChat_LocalDirect_SendsHFRepo(t *testing.T) {
	var path string
	var body map[string]any
	up := captureUpstream(t, &path, &body, nil)
	defer up.Close()
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport})

	rec := postChat(t, rt, `{"model":"coder","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if path != "/v1/chat/completions" {
		t.Errorf("upstream path = %q", path)
	}
	// alias "coder" -> qwen36-hypatia, hf_repo with "#nothink" stripped.
	if body["model"] != "Qwen/Qwen3.6-35B-A3B-FP8" {
		t.Errorf("upstream model = %v, want bare hf_repo", body["model"])
	}
}

func TestChat_ToolProxy_PreservesModelID(t *testing.T) {
	var body map[string]any
	up := captureUpstream(t, nil, &body, nil)
	defer up.Close()
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport})

	// alias "research" -> nemotron-3-super (tool_proxy:true): the tool proxy
	// must receive the registry key, not the hf_repo.
	rec := postChat(t, rt, `{"model":"research","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if body["model"] != "nemotron-3-super" {
		t.Errorf("tool-proxy model = %v, want model_id nemotron-3-super", body["model"])
	}
}

func TestChat_External_ForwardsWithBearer(t *testing.T) {
	var body map[string]any
	var auth string
	up := captureUpstream(t, nil, &body, &auth)
	defer up.Close()
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport})

	rec := postChat(t, rt, `{"model":"glm","messages":[]}`) // alias of zen-glm
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if body["model"] != "zen/glm-4.6" {
		t.Errorf("external model = %v, want zen/glm-4.6", body["model"])
	}
	if auth != "Bearer secret-key" {
		t.Errorf("Authorization = %q, want Bearer secret-key (env-resolved)", auth)
	}
}

func TestChat_External_ForwardsWithCustomHeader(t *testing.T) {
	var apiKey, auth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey = r.Header.Get("X-Api-Key")
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer up.Close()
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport})

	// Inbound carries the router's own front-door token; with a custom
	// api_key_header it must be replaced by the upstream key, not leaked.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"hdr","messages":[]}`)) // alias of zen-hdr
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-router-master")
	rt.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// key delivered raw (no "Bearer " prefix) in the configured header...
	if apiKey != "secret-key" {
		t.Errorf("X-Api-Key = %q, want secret-key (env-resolved, raw)", apiKey)
	}
	// ...and the router's own token stripped so it never reaches the provider.
	if auth != "" {
		t.Errorf("Authorization = %q, want empty (router token must not leak upstream)", auth)
	}
}

func TestChat_StripsOpenAIPrefix(t *testing.T) {
	var body map[string]any
	up := captureUpstream(t, nil, &body, nil)
	defer up.Close()
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport})

	rec := postChat(t, rt, `{"model":"openai/qwen36-hypatia","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body["model"] != "Qwen/Qwen3.6-35B-A3B-FP8" {
		t.Errorf("model = %v, want resolved hf_repo after stripping openai/", body["model"])
	}
}

func TestChat_UnknownModelIs404(t *testing.T) {
	rt := newTestRouter(t, nil)
	rec := postChat(t, rt, `{"model":"ghost","messages":[]}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestChat_DisabledModelIs404(t *testing.T) {
	rt := newTestRouter(t, nil)
	rec := postChat(t, rt, `{"model":"ghost-disabled","messages":[]}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for disabled model", rec.Code)
	}
}

func TestChat_BadJSON(t *testing.T) {
	rt := newTestRouter(t, nil)
	rec := postChat(t, rt, `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestChat_MissingModelField(t *testing.T) {
	rt := newTestRouter(t, nil)
	rec := postChat(t, rt, `{"messages":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestChat_SSEStreamsThrough(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer up.Close()
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport})

	rec := postChat(t, rt, `{"model":"coder","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
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

func TestChat_UpstreamUnreachableIs502(t *testing.T) {
	bad := &transportRedirect{to: "http://127.0.0.1:1", rt: http.DefaultTransport}
	rt := newTestRouter(t, bad)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"coder","messages":[]}`)))
	rt.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Phase 3b.i — /v1/completions, /v1/embeddings, /v1/rerank
// ---------------------------------------------------------------------------

func postTo(t *testing.T, rt *Router, path, jsonBody string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	rt.Handler().ServeHTTP(rec, req)
	return rec
}

func TestEmbeddings_RoutesWithHFRepo(t *testing.T) {
	var path string
	var body map[string]any
	up := captureUpstream(t, &path, &body, nil)
	defer up.Close()
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport})

	rec := postTo(t, rt, "/v1/embeddings",
		`{"model":"embedding","input":"hello world"}`) // alias of qwen3-embedding
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if path != "/v1/embeddings" {
		t.Errorf("upstream path = %q, want /v1/embeddings", path)
	}
	if body["model"] != "Qwen/Qwen3-Embedding-4B" {
		t.Errorf("upstream model = %v, want bare hf_repo", body["model"])
	}
	if body["input"] != "hello world" {
		t.Errorf("input field not preserved: %v", body["input"])
	}
}

func TestRerank_RoutesWithHFRepo(t *testing.T) {
	var path string
	var body map[string]any
	up := captureUpstream(t, &path, &body, nil)
	defer up.Close()
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport})

	rec := postTo(t, rt, "/v1/rerank",
		`{"model":"qwen3-reranker","query":"q","documents":["a","b"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if path != "/v1/rerank" {
		t.Errorf("upstream path = %q, want /v1/rerank", path)
	}
	if body["model"] != "Qwen/Qwen3-Reranker-4B" {
		t.Errorf("upstream model = %v, want bare hf_repo", body["model"])
	}
}

// /v1/completions on a tool_proxy:true model must bypass the tool proxy
// (the proxy doesn't serve that path) and forward the bare hf_repo direct
// to the node backend.
func TestCompletions_BypassesToolProxy(t *testing.T) {
	var path string
	var body map[string]any
	up := captureUpstream(t, &path, &body, nil)
	defer up.Close()
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport})

	rec := postTo(t, rt, "/v1/completions",
		`{"model":"research","prompt":"hello"}`) // alias of nemotron-3-super (tool_proxy:true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if path != "/v1/completions" {
		t.Errorf("upstream path = %q, want /v1/completions", path)
	}
	// hf_repo, NOT the model_id — forceDirect produces the same body shape as
	// a direct local model.
	if body["model"] != "nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4" {
		t.Errorf("upstream model = %v, want bare hf_repo (bypassed tool proxy)", body["model"])
	}
}

// Calling a non-chat endpoint with a chat model returns 400 with a clear
// class-mismatch message — and the body is NOT forwarded upstream.
func TestEmbeddings_RejectsChatModel(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
	}))
	defer up.Close()
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport})

	rec := postTo(t, rt, "/v1/embeddings", `{"model":"coder","input":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "api_class") {
		t.Errorf("error message doesn't mention api_class: %s", rec.Body.String())
	}
	if calls != 0 {
		t.Errorf("upstream received %d calls, want 0 (request must be rejected before forwarding)", calls)
	}
}

// Symmetric: calling /v1/chat/completions with an embedding-class model.
func TestChat_RejectsEmbeddingModel(t *testing.T) {
	rt := newTestRouter(t, nil)
	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"qwen3-embedding","messages":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Phase 3b.ii — reqlog sink integration
// ---------------------------------------------------------------------------

// usageUpstream is a fake upstream that responds with an OpenAI-shape body
// containing a `usage` block.
func usageUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":7,"completion_tokens":11,"total_tokens":18}}`))
	}))
}

func TestReqlog_SuccessfulChatRecordsUsage(t *testing.T) {
	up := usageUpstream(t)
	defer up.Close()
	sink := &reqlog.MemorySink{}
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport}, WithSink(sink))

	rec := postTo(t, rt, "/v1/chat/completions",
		`{"model":"research","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	recs := sink.Records()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	if r.Path != "/v1/chat/completions" || r.Method != "POST" {
		t.Errorf("path/method wrong: %q %q", r.Path, r.Method)
	}
	if r.Model != "research" || r.BackendModel != "nemotron-3-super" || !r.ViaToolProxy {
		t.Errorf("resolution fields wrong: model=%q backend=%q via=%v",
			r.Model, r.BackendModel, r.ViaToolProxy)
	}
	if r.APIClass != "chat" {
		t.Errorf("APIClass = %q, want chat", r.APIClass)
	}
	if r.Status != 200 {
		t.Errorf("Status = %d, want 200", r.Status)
	}
	if r.LatencyMS < 0 {
		t.Errorf("LatencyMS = %d (should be >= 0)", r.LatencyMS)
	}
	if r.PromptTokens == nil || *r.PromptTokens != 7 {
		t.Errorf("PromptTokens = %v, want 7", r.PromptTokens)
	}
	if r.CompletionTokens == nil || *r.CompletionTokens != 11 {
		t.Errorf("CompletionTokens = %v, want 11", r.CompletionTokens)
	}
	if r.TotalTokens == nil || *r.TotalTokens != 18 {
		t.Errorf("TotalTokens = %v, want 18", r.TotalTokens)
	}
	if r.Error != "" {
		t.Errorf("Error = %q, want empty", r.Error)
	}
}

func TestReqlog_EmbeddingsRecordsTokens(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":3,"total_tokens":3}}`))
	}))
	defer up.Close()
	sink := &reqlog.MemorySink{}
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport}, WithSink(sink))

	postTo(t, rt, "/v1/embeddings", `{"model":"embedding","input":"hi"}`)
	recs := sink.Records()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	if r.APIClass != "embeddings" || r.Path != "/v1/embeddings" {
		t.Errorf("wrong endpoint metadata: class=%q path=%q", r.APIClass, r.Path)
	}
	if r.PromptTokens == nil || *r.PromptTokens != 3 {
		t.Errorf("PromptTokens = %v, want 3", r.PromptTokens)
	}
	if r.CompletionTokens != nil {
		t.Errorf("CompletionTokens = %v, want nil (embeddings have no completion)", r.CompletionTokens)
	}
}

func TestReqlog_ClassMismatchRecorded(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer up.Close()
	sink := &reqlog.MemorySink{}
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport}, WithSink(sink))

	postTo(t, rt, "/v1/embeddings", `{"model":"coder","input":"x"}`)
	if calls != 0 {
		t.Errorf("upstream got %d calls, want 0", calls)
	}
	recs := sink.Records()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	if r.Status != 400 {
		t.Errorf("Status = %d, want 400", r.Status)
	}
	if r.BackendURL == "" {
		t.Errorf("BackendURL should be populated (resolution succeeded before class check)")
	}
	if r.APIClass != "chat" {
		t.Errorf("APIClass = %q (the model's own class) want chat", r.APIClass)
	}
	if !strings.Contains(r.Error, "api_class") {
		t.Errorf("Error doesn't mention api_class: %q", r.Error)
	}
}

func TestReqlog_UnknownModelRecorded(t *testing.T) {
	sink := &reqlog.MemorySink{}
	rt := newTestRouter(t, nil, WithSink(sink))
	postTo(t, rt, "/v1/chat/completions", `{"model":"ghost","messages":[]}`)
	recs := sink.Records()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	if r.Status != 404 || r.Model != "ghost" || r.BackendURL != "" || r.Error == "" {
		t.Errorf("404 record wrong: %+v", r)
	}
}

func TestReqlog_BadJSONRecorded(t *testing.T) {
	sink := &reqlog.MemorySink{}
	rt := newTestRouter(t, nil, WithSink(sink))
	postTo(t, rt, "/v1/chat/completions", `{not json`)
	recs := sink.Records()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if recs[0].Status != 400 || recs[0].Model != "" {
		t.Errorf("bad-json record wrong: status=%d model=%q", recs[0].Status, recs[0].Model)
	}
}

// SSE-tail usage extraction: a streamed response includes `usage` in the last
// data event before [DONE]; the rolling 64KB tail must capture it.
func TestReqlog_SSECapturesUsageFromTail(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		for _, chunk := range []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = w.Write([]byte(chunk))
			f.Flush()
		}
	}))
	defer up.Close()
	sink := &reqlog.MemorySink{}
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport}, WithSink(sink))

	postTo(t, rt, "/v1/chat/completions",
		`{"model":"coder","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	recs := sink.Records()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	if !r.Stream {
		t.Errorf("Stream = false, want true")
	}
	if r.PromptTokens == nil || *r.PromptTokens != 5 ||
		r.CompletionTokens == nil || *r.CompletionTokens != 2 ||
		r.TotalTokens == nil || *r.TotalTokens != 7 {
		t.Errorf("SSE usage extraction wrong: pt=%v ct=%v tt=%v",
			r.PromptTokens, r.CompletionTokens, r.TotalTokens)
	}
}

func TestReqlog_NilSinkBecomesNop(t *testing.T) {
	// WithSink(nil) shouldn't panic — it should set NopSink and proceed.
	rt := newTestRouter(t, nil, WithSink(nil))
	rec := postTo(t, rt, "/v1/chat/completions", `{"model":"coder","messages":[]}`)
	_ = rec // resolution may or may not succeed; the assertion is no panic.
}

// ---------------------------------------------------------------------------
// Phase 3b.iii — /metrics and richer /health
// ---------------------------------------------------------------------------

func TestHealth_RichFields(t *testing.T) {
	rt := newTestRouter(t, nil, WithVersion("test-1.2.3"))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rt.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Status        string         `json:"status"`
		Version       string         `json:"version"`
		Mode          string         `json:"mode"`
		UptimeSec     float64        `json:"uptime_seconds"`
		Models        int            `json:"models"`
		ModelsByClass map[string]int `json:"models_by_class"`
		Streaming     bool           `json:"streaming"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ok" || resp.Version != "test-1.2.3" || !resp.Streaming {
		t.Errorf("scalar fields wrong: %+v", resp)
	}
	if resp.Models < 5 {
		t.Errorf("Models = %d, expected several active", resp.Models)
	}
	if resp.ModelsByClass["chat"] < 3 {
		t.Errorf("models_by_class[chat] = %d, want >= 3 (testYAML has multiple chat models)",
			resp.ModelsByClass["chat"])
	}
	if resp.ModelsByClass["embeddings"] != 1 || resp.ModelsByClass["rerank"] != 1 {
		t.Errorf("models_by_class wrong: %+v", resp.ModelsByClass)
	}
	if resp.UptimeSec < 0 {
		t.Errorf("UptimeSec = %v, want >= 0", resp.UptimeSec)
	}
}

func TestMetrics_ServesPrometheus(t *testing.T) {
	rt := newTestRouter(t, nil, WithVersion("test-vx"))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rt.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`router_build_info{version="test-vx"} 1`,
		"router_uptime_seconds ",
		`router_models_active{api_class="chat"}`,
		`router_models_active{api_class="embeddings"}`,
		"go_goroutines", // Go collector
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics body missing %q", want)
		}
	}
}

func TestMetrics_RequestsAndTokensObserved(t *testing.T) {
	up := usageUpstream(t) // returns usage {7,11,18}
	defer up.Close()
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport})

	// One successful chat (counts requests_total + duration + tokens).
	postTo(t, rt, "/v1/chat/completions", `{"model":"research","messages":[]}`)
	// One 404 (counts under model="unresolved", api_class="unknown").
	postTo(t, rt, "/v1/chat/completions", `{"model":"ghost","messages":[]}`)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rt.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()

	for _, want := range []string{
		// research alias -> nemotron-3-super; chat path; status 200
		`router_requests_total{api_class="chat",model="nemotron-3-super",path="/v1/chat/completions",status="200"} 1`,
		// unresolved 404
		`router_requests_total{api_class="unknown",model="unresolved",path="/v1/chat/completions",status="404"} 1`,
		// tokens 7 prompt, 11 completion from the upstream usage block
		`router_upstream_tokens_total{api_class="chat",kind="prompt",model="nemotron-3-super"} 7`,
		`router_upstream_tokens_total{api_class="chat",kind="completion",model="nemotron-3-super"} 11`,
		// duration histogram exists for chat path
		`router_request_duration_seconds_bucket{api_class="chat",path="/v1/chat/completions"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics body missing %q\n--- body excerpt ---\n%s", want, snippet(body, want))
		}
	}
}

// snippet returns ~3 lines of context around the first occurrence of needle's
// metric name (everything up to the first '{'), or the first 800 bytes if not
// found. Helps when an assertion fails because the value is off by a number.
func snippet(haystack, needle string) string {
	name := needle
	if i := strings.Index(needle, "{"); i > 0 {
		name = needle[:i]
	}
	idx := strings.Index(haystack, name)
	if idx < 0 {
		if len(haystack) > 800 {
			return haystack[:800] + "..."
		}
		return haystack
	}
	start := idx - 200
	if start < 0 {
		start = 0
	}
	end := idx + 800
	if end > len(haystack) {
		end = len(haystack)
	}
	return haystack[start:end]
}
