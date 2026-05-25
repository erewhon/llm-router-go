package toolproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/toolproxy/tools"
)

// ---------------------------------------------------------------------------
// Test harness: a proxy with the calculator tool registered, and a scripted
// upstream that returns queued JSON for non-streaming (loop) calls and a fixed
// SSE body for the streaming (final re-stream) call.
// ---------------------------------------------------------------------------

func newToolProxy(t *testing.T, transport http.RoundTripper, opts ...Option) *Proxy {
	t.Helper()
	reg, err := config.LoadBytes([]byte(testYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	tr := tools.NewRegistry()
	tr.Register(tools.Calculator()) // deterministic, no network
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	base := []Option{WithFlushInterval(0), WithTools(tr)}
	if transport != nil {
		base = append(base, WithTransport(transport))
	}
	base = append(base, opts...)
	return New(reg, logger, base...)
}

type scriptedUpstream struct {
	mu     sync.Mutex
	queue  []string         // popped in order for stream:false calls
	repeat string           // if set, returned for every stream:false call (queue ignored)
	sse    string           // returned for the stream:true call
	calls  []map[string]any // captured request bodies, in order
}

func (s *scriptedUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)

		s.mu.Lock()
		s.calls = append(s.calls, body)
		isStream, _ := body["stream"].(bool)
		var resp string
		switch {
		case isStream:
			s.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			_, _ = io.WriteString(w, s.sse)
			if flusher != nil {
				flusher.Flush()
			}
			return
		case s.repeat != "":
			resp = s.repeat
		case len(s.queue) > 0:
			resp = s.queue[0]
			s.queue = s.queue[1:]
		default:
			resp = answerResp("(no more scripted responses)")
		}
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, resp)
	}
}

func (s *scriptedUpstream) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *scriptedUpstream) call(i int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[i]
}

// toolCallResp is a non-streaming response whose assistant message calls one tool.
func toolCallResp(name, args string) string {
	return fmt.Sprintf(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_abc","type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}]}`, name, args)
}

// answerResp is a non-streaming final answer with usage.
func answerResp(content string) string {
	return fmt.Sprintf(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`, content)
}

// toolCallRespContent is a tool-call response whose assistant message also
// carries content (e.g. inline <think> reasoning).
func toolCallRespContent(name, args, content string) string {
	return fmt.Sprintf(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":%q,"tool_calls":[{"id":"call_abc","type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}]}`, content, name, args)
}

func postChat(t *testing.T, p *Proxy, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	p.Handler().ServeHTTP(rec, req)
	return rec
}

// decode a non-streaming chat.completion out of a recorder.
func decodeCompletion(t *testing.T, rec *httptest.ResponseRecorder) chatCompletionOut {
	t.Helper()
	var cc chatCompletionOut
	if err := json.Unmarshal(rec.Body.Bytes(), &cc); err != nil {
		t.Fatalf("decode response: %v\nbody=%s", err, rec.Body.String())
	}
	return cc
}

// messageRole pulls role/content out of a captured message at index i.
func msgField(m map[string]any, i int, field string) any {
	msgs, _ := m["messages"].([]any)
	if i < 0 {
		i = len(msgs) + i
	}
	if i < 0 || i >= len(msgs) {
		return nil
	}
	msg, _ := msgs[i].(map[string]any)
	return msg[field]
}

// ---------------------------------------------------------------------------
// Non-streaming
// ---------------------------------------------------------------------------

func TestToolLoop_ExecutesProxyToolThenAnswers(t *testing.T) {
	up := &scriptedUpstream{queue: []string{
		toolCallResp("calculator", `{"expression":"2+2"}`),
		answerResp("The answer is 4."),
	}}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"nemotron-3-super","messages":[{"role":"user","content":"what is 2+2?"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	cc := decodeCompletion(t, rec)
	if cc.Choices[0].Message.Content != "The answer is 4." {
		t.Errorf("content = %q", cc.Choices[0].Message.Content)
	}
	if cc.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", cc.Choices[0].FinishReason)
	}
	if cc.Usage == nil || cc.Usage.TotalTokens != 3 {
		t.Errorf("usage = %+v, want total 3", cc.Usage)
	}

	if up.callCount() != 2 {
		t.Fatalf("upstream calls = %d, want 2", up.callCount())
	}
	// First call must offer the calculator tool.
	if tools, _ := up.call(0)["tools"].([]any); len(tools) == 0 {
		t.Error("first call carried no tools")
	}
	// Second call's last message must be the tool result with the computed value.
	if role := msgField(up.call(1), -1, "role"); role != "tool" {
		t.Errorf("last message role = %v, want tool", role)
	}
	if content := msgField(up.call(1), -1, "content"); content != "4" {
		t.Errorf("tool result content = %v, want 4", content)
	}
	// And the assistant turn that requested the tool must be present.
	if tc := msgField(up.call(1), -2, "tool_calls"); tc == nil {
		t.Error("assistant tool_calls turn missing from history")
	}
}

func TestToolLoop_NaturalAnswerNoTools(t *testing.T) {
	up := &scriptedUpstream{queue: []string{answerResp("hello there")}}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"nemotron-3-super","messages":[{"role":"user","content":"hi"}]}`)
	cc := decodeCompletion(t, rec)
	if cc.Choices[0].Message.Content != "hello there" || cc.Choices[0].FinishReason != "stop" {
		t.Errorf("choice = %+v", cc.Choices[0])
	}
	if up.callCount() != 1 {
		t.Errorf("upstream calls = %d, want 1", up.callCount())
	}
}

func TestToolLoop_ClientToolReturnedNotExecuted(t *testing.T) {
	up := &scriptedUpstream{queue: []string{toolCallResp("get_weather", `{"city":"NYC"}`)}}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"nemotron-3-super","messages":[{"role":"user","content":"weather?"}]}`)
	cc := decodeCompletion(t, rec)
	if cc.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", cc.Choices[0].FinishReason)
	}
	if len(cc.Choices[0].Message.ToolCalls) != 1 || cc.Choices[0].Message.ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("tool_calls = %+v, want get_weather", cc.Choices[0].Message.ToolCalls)
	}
	// The proxy must NOT loop on a client-owned tool.
	if up.callCount() != 1 {
		t.Errorf("upstream calls = %d, want 1 (no execution)", up.callCount())
	}
}

func TestToolLoop_MaxRounds(t *testing.T) {
	// Always returns a proxy tool call → the loop can never terminate naturally.
	up := &scriptedUpstream{repeat: toolCallResp("calculator", `{"expression":"1+1"}`)}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport}, WithMaxToolRounds(3))

	rec := postChat(t, p, `{"model":"nemotron-3-super","messages":[{"role":"user","content":"loop"}]}`)
	cc := decodeCompletion(t, rec)
	if cc.Choices[0].Message.Content != maxRoundsMessage {
		t.Errorf("content = %q, want %q", cc.Choices[0].Message.Content, maxRoundsMessage)
	}
	if up.callCount() != 3 {
		t.Errorf("upstream calls = %d, want 3 (capped)", up.callCount())
	}
}

func TestToolLoop_MergesClientToolsProxyWins(t *testing.T) {
	up := &scriptedUpstream{queue: []string{answerResp("ok")}}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	// Client sends its own get_weather plus a colliding "calculator" — the
	// proxy's calculator must win and the duplicate must be dropped.
	body := `{"model":"nemotron-3-super","messages":[],"tools":[
		{"type":"function","function":{"name":"get_weather","description":"w","parameters":{}}},
		{"type":"function","function":{"name":"calculator","description":"client dup","parameters":{}}}
	]}`
	_ = postChat(t, p, body)

	toolsArr, _ := up.call(0)["tools"].([]any)
	names := map[string]int{}
	for _, tl := range toolsArr {
		if n := toolFunctionName(tl); n != "" {
			names[n]++
		}
	}
	if names["calculator"] != 1 {
		t.Errorf("calculator appears %d times, want exactly 1", names["calculator"])
	}
	if names["get_weather"] != 1 {
		t.Errorf("get_weather missing from merged tools: %v", names)
	}
}

func TestToolLoop_BackendErrorIs502(t *testing.T) {
	// Tools are registered, so this takes the loop path; the loop's backend
	// call fails (nothing listening) → 502.
	bad := &transportRedirect{to: "http://127.0.0.1:1", rt: http.DefaultTransport}
	p := newToolProxy(t, bad)
	rec := postChat(t, p, `{"model":"nemotron-3-super","messages":[]}`)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// nothink → passthrough (no tool injection)
// ---------------------------------------------------------------------------

func TestToolLoop_NothinkSkipsInjection(t *testing.T) {
	up := &scriptedUpstream{queue: []string{answerResp("hi")}}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"qwen-nothink","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if up.callCount() != 1 {
		t.Fatalf("upstream calls = %d, want 1 (single passthrough)", up.callCount())
	}
	// Passthrough must NOT inject proxy tools, and must rewrite the model.
	if _, present := up.call(0)["tools"]; present {
		t.Error("nothink passthrough injected a tools field")
	}
	if up.call(0)["model"] != "Qwen/Qwen3.5-35B" {
		t.Errorf("model = %v, want bare hf_repo", up.call(0)["model"])
	}
}

// ---------------------------------------------------------------------------
// Streaming
// ---------------------------------------------------------------------------

func TestToolLoop_StreamingReStreamsFinal(t *testing.T) {
	finalSSE := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"The answer\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" is 4\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	up := &scriptedUpstream{
		queue: []string{
			toolCallResp("calculator", `{"expression":"2+2"}`), // round 1: tool
			answerResp("buffered final, discarded"),            // round 2: no tools → outcomeFinal
		},
		sse: finalSSE,
	}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"nemotron-3-super","stream":true,"messages":[{"role":"user","content":"2+2?"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		t.Errorf("Content-Type = %q, want event-stream", ct)
	}
	got := rec.Body.String()
	for _, want := range []string{"The answer", "is 4", "[DONE]"} {
		if !strings.Contains(got, want) {
			t.Errorf("stream missing %q:\n%s", want, got)
		}
	}
	// Three upstream hits: two loop calls + the streamed re-generation.
	if up.callCount() != 3 {
		t.Fatalf("upstream calls = %d, want 3", up.callCount())
	}
	final := up.call(2)
	if stream, _ := final["stream"].(bool); !stream {
		t.Error("final call was not stream:true")
	}
	if _, present := final["tools"]; present {
		t.Error("final stream call still carried tools")
	}
	if role := msgField(final, -1, "role"); role != "tool" {
		t.Errorf("final call history last role = %v, want tool", role)
	}
}

func TestToolLoop_StreamingClientToolBreakout(t *testing.T) {
	up := &scriptedUpstream{queue: []string{toolCallResp("get_weather", `{"city":"NYC"}`)}}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"nemotron-3-super","stream":true,"messages":[{"role":"user","content":"weather?"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"tool_calls", "get_weather", "[DONE]", "chat.completion"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %q:\n%s", want, body)
		}
	}
	if up.callCount() != 1 {
		t.Errorf("upstream calls = %d, want 1 (no re-stream on client breakout)", up.callCount())
	}
}

// ---------------------------------------------------------------------------
// Reasoning passthrough (2d)
// ---------------------------------------------------------------------------

func TestToolLoop_NonStreamingExtractsInlineThink(t *testing.T) {
	up := &scriptedUpstream{queue: []string{
		`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"<think>let me think</think>The answer is 42."},"finish_reason":"stop"}]}`,
	}}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"nemotron-3-super","messages":[{"role":"user","content":"hi"}]}`)
	cc := decodeCompletion(t, rec)
	if cc.Choices[0].Message.Content != "The answer is 42." {
		t.Errorf("content = %q, want think-stripped answer", cc.Choices[0].Message.Content)
	}
	if cc.Choices[0].Message.ReasoningContent != "let me think" {
		t.Errorf("reasoning_content = %q, want 'let me think'", cc.Choices[0].Message.ReasoningContent)
	}
}

func TestToolLoop_PrefersBackendReasoning(t *testing.T) {
	// Both an inline <think> tag and a structured reasoning_content are present;
	// the backend's structured field wins.
	up := &scriptedUpstream{queue: []string{
		`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"<think>tag reasoning</think>answer","reasoning_content":"backend reasoning"},"finish_reason":"stop"}]}`,
	}}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"nemotron-3-super","messages":[]}`)
	cc := decodeCompletion(t, rec)
	if cc.Choices[0].Message.ReasoningContent != "backend reasoning" {
		t.Errorf("reasoning_content = %q, want backend value", cc.Choices[0].Message.ReasoningContent)
	}
	if cc.Choices[0].Message.Content != "answer" {
		t.Errorf("content = %q, want 'answer'", cc.Choices[0].Message.Content)
	}
}

func TestToolLoop_StripsThinkFromHistory(t *testing.T) {
	// The assistant turn recorded in history must have its <think> removed so
	// the model doesn't re-read its own reasoning on the next round.
	up := &scriptedUpstream{queue: []string{
		toolCallRespContent("calculator", `{"expression":"2+2"}`, "<think>compute it</think>"),
		answerResp("done"),
	}}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	_ = postChat(t, p, `{"model":"nemotron-3-super","messages":[{"role":"user","content":"2+2?"}]}`)
	if up.callCount() != 2 {
		t.Fatalf("upstream calls = %d, want 2", up.callCount())
	}
	if c := msgField(up.call(1), -2, "content"); c != "" {
		t.Errorf("assistant history content = %v, want empty (think stripped)", c)
	}
}

func TestToolLoop_StreamingForwardsBackendReasoning(t *testing.T) {
	// The relayed final stream carries reasoning_content deltas straight to the
	// client — structured reasoning passthrough needs no reframing.
	finalSSE := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking out loud\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"the answer\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	up := &scriptedUpstream{
		queue: []string{answerResp("triggers outcomeFinal")},
		sse:   finalSSE,
	}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"nemotron-3-super","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := rec.Body.String()
	for _, want := range []string{"reasoning_content", "thinking out loud", "the answer", "[DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %q:\n%s", want, body)
		}
	}
}

func TestToolLoop_StreamingBackendErrorEmitsSSE(t *testing.T) {
	bad := &transportRedirect{to: "http://127.0.0.1:1", rt: http.DefaultTransport}
	p := newToolProxy(t, bad)
	rec := postChat(t, p, `{"model":"nemotron-3-super","stream":true,"messages":[]}`)
	// Streaming errors are reported in-band as an SSE error chunk + [DONE],
	// not an HTTP error status (the stream has already begun semantically).
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (in-band SSE error)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Error:") || !strings.Contains(body, "[DONE]") {
		t.Errorf("expected SSE error + DONE, got:\n%s", body)
	}
}
