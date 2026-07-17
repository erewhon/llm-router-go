package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erewhon/llm-router-go/internal/router/reqlog"
)

// anthropicUpstream is a mock api.anthropic.com that records the request it
// receives (path, raw body, key headers) and returns a canned response.
type anthropicUpstream struct {
	path        string
	body        []byte
	authHeader  string
	apiKey      string
	betaHeader  string
	xForwarded  string
	respHeaders map[string]string
	respBody    string
}

func (u *anthropicUpstream) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.path = r.URL.Path
		u.body, _ = io.ReadAll(r.Body)
		u.authHeader = r.Header.Get("Authorization")
		u.apiKey = r.Header.Get("x-api-key")
		u.betaHeader = r.Header.Get("anthropic-beta")
		u.xForwarded = r.Header.Get("X-Forwarded-For")
		for k, v := range u.respHeaders {
			w.Header().Set(k, v)
		}
		_, _ = io.WriteString(w, u.respBody)
	}))
}

func TestAnthropic_VerbatimPassthrough_ForwardsClientCreds(t *testing.T) {
	up := &anthropicUpstream{
		respHeaders: map[string]string{"Content-Type": "application/json"},
		respBody: `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],
		            "usage":{"input_tokens":25,"cache_creation_input_tokens":10,"cache_read_input_tokens":90,"output_tokens":7}}`,
	}
	srv := up.server(t)
	defer srv.Close()

	mem := &reqlog.MemorySink{}
	rt := newTestRouter(t, &transportRedirect{to: srv.URL, rt: http.DefaultTransport}, WithSink(mem))

	// A request body with tools + system + messages so the prefix chain has ≥3 links.
	reqBody := `{"model":"claude-sonnet-4-5","tools":[{"name":"t"}],"system":"be brief","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-ant-oat-client-token") // Claude Code Max OAuth
	req.Header.Set("x-api-key", "sk-ant-client-key")
	req.Header.Set("anthropic-beta", "prompt-caching-2024-07-31")
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// Path + byte-identical body reached the upstream.
	if up.path != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", up.path)
	}
	if string(up.body) != reqBody {
		t.Errorf("upstream body not byte-identical:\n got %q\nwant %q", up.body, reqBody)
	}
	// Client credentials forwarded untouched; router injected none of its own.
	if up.authHeader != "Bearer sk-ant-oat-client-token" {
		t.Errorf("Authorization = %q, want the client's own token forwarded", up.authHeader)
	}
	if up.apiKey != "sk-ant-client-key" {
		t.Errorf("x-api-key = %q, want the client's own key forwarded", up.apiKey)
	}
	if up.betaHeader != "prompt-caching-2024-07-31" {
		t.Errorf("anthropic-beta = %q, want forwarded", up.betaHeader)
	}
	if up.xForwarded != "" {
		t.Errorf("X-Forwarded-For = %q, want empty (transparent passthrough)", up.xForwarded)
	}

	// reqlog: one record, anthropic class, cache-token splits, prefix chain.
	recs := mem.Records()
	if len(recs) != 1 {
		t.Fatalf("reqlog records = %d, want 1", len(recs))
	}
	lr := recs[0]
	if lr.Model != "claude-sonnet-4-5" || lr.APIClass != "anthropic" || lr.ResolvedVia != "anthropic-gateway" {
		t.Errorf("record identity wrong: model=%q class=%q via=%q", lr.Model, lr.APIClass, lr.ResolvedVia)
	}
	if lr.PromptTokens == nil || *lr.PromptTokens != 25 ||
		lr.CompletionTokens == nil || *lr.CompletionTokens != 7 ||
		lr.CacheCreationInputTokens == nil || *lr.CacheCreationInputTokens != 10 ||
		lr.CacheReadInputTokens == nil || *lr.CacheReadInputTokens != 90 {
		t.Errorf("usage wrong: in=%v out=%v cc=%v cr=%v",
			lr.PromptTokens, lr.CompletionTokens, lr.CacheCreationInputTokens, lr.CacheReadInputTokens)
	}
	if strings.Count(lr.PrefixHashChain, ",") != 2 { // tools, system, one message => 3 links
		t.Errorf("prefix hash chain = %q, want 3 comma-separated links", lr.PrefixHashChain)
	}
}

func TestAnthropic_SSE_CapturesUsageFromEvents(t *testing.T) {
	// Anthropic stream: input/cache tokens on message_start, output on message_delta.
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_creation_input_tokens":5,"cache_read_input_tokens":200,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}` + "\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	up := &anthropicUpstream{
		respHeaders: map[string]string{"Content-Type": "text/event-stream"},
		respBody:    sse,
	}
	srv := up.server(t)
	defer srv.Close()

	mem := &reqlog.MemorySink{}
	rt := newTestRouter(t, &transportRedirect{to: srv.URL, rt: http.DefaultTransport}, WithSink(mem))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// Client still received the full stream unchanged.
	if !strings.Contains(rec.Body.String(), "message_stop") {
		t.Errorf("client did not receive the full SSE stream")
	}
	recs := mem.Records()
	if len(recs) != 1 {
		t.Fatalf("reqlog records = %d, want 1", len(recs))
	}
	lr := recs[0]
	if !lr.Stream {
		t.Errorf("Stream = false, want true for an SSE response")
	}
	if lr.PromptTokens == nil || *lr.PromptTokens != 100 ||
		lr.CompletionTokens == nil || *lr.CompletionTokens != 42 || // final message_delta wins
		lr.CacheCreationInputTokens == nil || *lr.CacheCreationInputTokens != 5 ||
		lr.CacheReadInputTokens == nil || *lr.CacheReadInputTokens != 200 {
		t.Errorf("SSE usage wrong: in=%v out=%v cc=%v cr=%v",
			lr.PromptTokens, lr.CompletionTokens, lr.CacheCreationInputTokens, lr.CacheReadInputTokens)
	}
}

func TestAnthropicPrefixChain_DeterministicAndDiverges(t *testing.T) {
	base := `{"model":"claude-sonnet-4-5","tools":[{"name":"t"}],"system":"S","messages":[%s]}`
	msgA := `{"role":"user","content":"one"}`
	msgB := `{"role":"user","content":"two"}`

	// Same request twice → identical model + chain (determinism).
	m1, c1 := anthropicPrefixChain([]byte(strings.Replace(base, "%s", msgA, 1)))
	_, c2 := anthropicPrefixChain([]byte(strings.Replace(base, "%s", msgA, 1)))
	if m1 != "claude-sonnet-4-5" || c1 == "" {
		t.Fatalf("unexpected model/chain: model=%q chain=%q", m1, c1)
	}
	if c1 != c2 {
		t.Errorf("chain not deterministic:\n %q\n %q", c1, c2)
	}

	// Differ only in the last message → shared prefix (tools, system), last link diverges.
	_, cA := anthropicPrefixChain([]byte(strings.Replace(base, "%s", msgA, 1)))
	_, cB := anthropicPrefixChain([]byte(strings.Replace(base, "%s", msgB, 1)))
	la, lb := strings.Split(cA, ","), strings.Split(cB, ",")
	if len(la) != 3 || len(lb) != 3 {
		t.Fatalf("expected 3 links each, got %d and %d", len(la), len(lb))
	}
	if la[0] != lb[0] || la[1] != lb[1] {
		t.Errorf("prefix (tools, system) should match:\n A=%v\n B=%v", la, lb)
	}
	if la[2] == lb[2] {
		t.Errorf("final link should diverge when the last message differs")
	}
}
