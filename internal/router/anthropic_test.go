package router

import (
	"compress/gzip"
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
	// gzipResp mimics api.anthropic.com: when the request advertises gzip, the
	// response body is gzip-compressed with a Content-Encoding: gzip header.
	gzipResp bool
	// acceptEnc records the Accept-Encoding the upstream actually received.
	acceptEnc string
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
		u.acceptEnc = r.Header.Get("Accept-Encoding")
		for k, v := range u.respHeaders {
			w.Header().Set(k, v)
		}
		if u.gzipResp && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			_, _ = io.WriteString(gz, u.respBody)
			_ = gz.Close()
			return
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

// TestAnthropic_SSE_CapturesUsage_GzipResponse is the regression for the v0.6.0
// bug where a gzip'd upstream response made the usage tee scan compressed bytes
// and log NULL tokens. Claude Code always sends Accept-Encoding: gzip, so
// api.anthropic.com returns a gzip'd SSE stream; the router must decode it
// before parsing usage. Without stripping the client's Accept-Encoding upstream
// (so the Go transport re-adds gzip and transparently decompresses), the usage
// assertions below fail with nil tokens.
func TestAnthropic_SSE_CapturesUsage_GzipResponse(t *testing.T) {
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
		gzipResp:    true,
	}
	srv := up.server(t)
	defer srv.Close()

	mem := &reqlog.MemorySink{}
	rt := newTestRouter(t, &transportRedirect{to: srv.URL, rt: http.DefaultTransport}, WithSink(mem))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip") // what Claude Code sends
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// The upstream still received a gzip-capable request: the router stripped the
	// client's header and the Go transport re-added its own (then decompressed).
	if !strings.Contains(up.acceptEnc, "gzip") {
		t.Errorf("upstream Accept-Encoding = %q, want gzip still requested", up.acceptEnc)
	}
	// The client received the full, decoded stream.
	if !strings.Contains(rec.Body.String(), "message_stop") {
		t.Errorf("client did not receive the full SSE stream")
	}
	recs := mem.Records()
	if len(recs) != 1 {
		t.Fatalf("reqlog records = %d, want 1", len(recs))
	}
	lr := recs[0]
	if lr.PromptTokens == nil || *lr.PromptTokens != 100 ||
		lr.CompletionTokens == nil || *lr.CompletionTokens != 42 ||
		lr.CacheCreationInputTokens == nil || *lr.CacheCreationInputTokens != 5 ||
		lr.CacheReadInputTokens == nil || *lr.CacheReadInputTokens != 200 {
		t.Errorf("gzip SSE usage wrong (compressed body not decoded before parse?): in=%v out=%v cc=%v cr=%v",
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

func TestAnthropicPrefixChain_IgnoresCacheControl(t *testing.T) {
	// The same conversation with the breakpoint marker on message 0 vs
	// message 1 — Claude Code moves it every request — must hash
	// identically: the cache keys on content, not marker placement.
	withMarkerOn := func(idx int) []byte {
		marker := `,"cache_control":{"type":"ephemeral"}`
		msgs := []string{
			`{"role":"user","content":[{"type":"text","text":"one"M0}]}`,
			`{"role":"assistant","content":[{"type":"text","text":"two"M1}]}`,
		}
		for i := range msgs {
			m := ""
			if i == idx {
				m = marker
			}
			msgs[i] = strings.Replace(msgs[i], "M0", m, 1)
			msgs[i] = strings.Replace(msgs[i], "M1", m, 1)
		}
		return []byte(`{"model":"m","tools":[{"name":"t"}],"system":"S","messages":[` +
			strings.Join(msgs, ",") + `]}`)
	}
	_, c0 := anthropicPrefixChain(withMarkerOn(0))
	_, c1 := anthropicPrefixChain(withMarkerOn(1))
	if c0 == "" || c0 != c1 {
		t.Errorf("marker rotation changed the chain:\n %q\n %q", c0, c1)
	}

	// Markers on a tool definition and on a system block wash out too.
	plain := `{"model":"m","tools":[{"name":"t"}],"system":[{"type":"text","text":"S"}],"messages":[{"role":"user","content":"x"}]}`
	marked := `{"model":"m","tools":[{"name":"t","cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
		`"system":[{"type":"text","text":"S","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"x"}]}`
	_, cp := anthropicPrefixChain([]byte(plain))
	_, cm := anthropicPrefixChain([]byte(marked))
	if cp == "" || cp != cm {
		t.Errorf("tools/system markers changed the chain:\n %q\n %q", cp, cm)
	}
}

func TestAnthropicPrefixChain_CanonicalizesButKeepsContent(t *testing.T) {
	// Key order is serialization noise, not content: identical chains.
	a := `{"model":"m","tools":[{"name":"t"}],"system":"S","messages":[{"role":"user","content":"x"}]}`
	b := `{"model":"m","tools":[{"name":"t"}],"system":"S","messages":[{"content":"x","role":"user"}]}`
	_, ca := anthropicPrefixChain([]byte(a))
	_, cb := anthropicPrefixChain([]byte(b))
	if ca == "" || ca != cb {
		t.Errorf("key order changed the chain:\n %q\n %q", ca, cb)
	}

	// A cache_control key nested INSIDE user data (a tool_use input) is
	// content — stripping is deliberately not recursive — so it must
	// still diverge the chain.
	base := `{"model":"m","tools":[{"name":"t"}],"system":"S","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"1","name":"t","input":{"payload":%s}}]}]}`
	_, cx := anthropicPrefixChain([]byte(strings.Replace(base, "%s", `{"cache_control":"keep-me"}`, 1)))
	_, cy := anthropicPrefixChain([]byte(strings.Replace(base, "%s", `{}`, 1)))
	if cx == "" || cx == cy {
		t.Errorf("nested cache_control in user data was stripped: chains should differ")
	}

	// Big integers survive canonicalization without float rounding: two
	// values that would collide as float64 stay distinct.
	_, ci := anthropicPrefixChain([]byte(strings.Replace(base, "%s", `9007199254740993`, 1)))
	_, cj := anthropicPrefixChain([]byte(strings.Replace(base, "%s", `9007199254740992`, 1)))
	if ci == "" || ci == cj {
		t.Errorf("adjacent big ints hashed identically: float round-trip in canonicalization")
	}
}
