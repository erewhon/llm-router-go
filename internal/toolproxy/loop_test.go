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
// upstream that returns queued JSON for non-streaming calls and queued SSE
// bodies (falling back to a fixed one) for streaming calls.
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
	mu       sync.Mutex
	queue    []string         // popped in order for stream:false calls
	repeat   string           // if set, returned for every stream:false call (queue ignored)
	sse      string           // returned for stream:true calls once sseQueue is empty
	sseQueue []string         // popped in order for stream:true calls
	calls    []map[string]any // captured request bodies, in order
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
			body := s.sse
			if len(s.sseQueue) > 0 {
				body = s.sseQueue[0]
				s.sseQueue = s.sseQueue[1:]
			}
			s.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			_, _ = io.WriteString(w, body)
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

// emptyAnswerResp is a terminal completion carrying no content — what a
// backend returns when the model gives up, or when the engine discards a tool
// call naming a function the request never declared.
func emptyAnswerResp(completionTokens int) string {
	return fmt.Sprintf(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":null},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":%d,"total_tokens":%d}}`, completionTokens, completionTokens+1)
}

// decodeErrorMessage pulls .error.message out of an OpenAI-shaped error body.
func decodeErrorMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error body: %v\nbody=%s", err, rec.Body.String())
	}
	return env.Error.Message
}

// Path 1: tools ran and all failed (dead egress) — the model gives up and the
// proxy used to return HTTP 200 with content "".
func TestToolLoop_EmptyAnswerAfterToolFailureIs502(t *testing.T) {
	up := &scriptedUpstream{queue: []string{
		// The calculator is the only tool registered in tests; a bogus
		// expression makes it return its failure string, standing in for a
		// search that can't reach its egress.
		toolCallResp("calculator", `{"expression":"no_such_fn(1)"}`),
		emptyAnswerResp(27),
	}}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"nemotron-3-super","messages":[{"role":"user","content":"research something"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}
	msg := decodeErrorMessage(t, rec)
	if !strings.Contains(msg, "all 1 tool call(s) failed") {
		t.Errorf("error message = %q, want it to report that every tool call failed", msg)
	}
	// The failing tool must be named, so the journal/caller can see what broke.
	if !strings.Contains(msg, "calculator") {
		t.Errorf("error message = %q, want the failing tool named", msg)
	}
}

// Path 2: no tool ran at all. The backend burned tokens that never became
// content (Atlas discarding a call to an undeclared tool). Nothing failed, so
// a tool-failure-only check would miss this.
func TestToolLoop_EmptyAnswerWithNoToolsRunIs502(t *testing.T) {
	up := &scriptedUpstream{queue: []string{emptyAnswerResp(28)}}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"nemotron-3-super","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}
	msg := decodeErrorMessage(t, rec)
	if !strings.Contains(msg, "executed no tools") {
		t.Errorf("error message = %q, want it to report that no tools ran", msg)
	}
	// The token count is the tell that the backend generated and dropped output.
	if !strings.Contains(msg, "28 completion tokens") {
		t.Errorf("error message = %q, want the backend token count for diagnosis", msg)
	}
}

// Streaming must fail the same way, before any SSE byte is written.
func TestToolLoop_EmptyAnswerStreamingIs502(t *testing.T) {
	up := &scriptedUpstream{queue: []string{emptyAnswerResp(28)}}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"nemotron-3-super","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "data:") {
		t.Errorf("streamed SSE before failing; body = %s", rec.Body.String())
	}
}

// A real answer must still pass through untouched — the guard must not fire on
// short-but-valid content.
func TestToolLoop_ShortAnswerNotTreatedAsEmpty(t *testing.T) {
	up := &scriptedUpstream{queue: []string{answerResp("4")}}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"nemotron-3-super","messages":[{"role":"user","content":"2+2?"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := decodeCompletion(t, rec).Choices[0].Message.Content; got != "4" {
		t.Errorf("content = %q, want 4", got)
	}
}

// truncatedAnswerResp is a final answer the backend cut short (finish_reason
// "length") — what Atlas returns when its inter-tool prose budget is exhausted
// while tool definitions are still attached to the request.
func truncatedAnswerResp(content string) string {
	return fmt.Sprintf(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`, content)
}

func TestToolLoop_TruncatedFinalAnswerRegeneratedWithoutTools(t *testing.T) {
	up := &scriptedUpstream{queue: []string{
		toolCallResp("calculator", `{"expression":"2+2"}`),
		truncatedAnswerResp(`{"answer":"cut off mid-`), // capped while tools attached
		answerResp(`{"answer":"complete","sources":["a"]}`),
	}}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"nemotron-3-super","messages":[{"role":"user","content":"research 2+2"}]}`)
	cc := decodeCompletion(t, rec)

	if got, want := cc.Choices[0].Message.Content, `{"answer":"complete","sources":["a"]}`; got != want {
		t.Errorf("content = %q, want the regenerated answer %q", got, want)
	}
	if cc.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop after successful regeneration", cc.Choices[0].FinishReason)
	}
	if up.callCount() != 3 {
		t.Fatalf("upstream calls = %d, want 3 (tool call, truncated answer, regeneration)", up.callCount())
	}
	// The regeneration must drop the tool definitions — that's what lifts the
	// backend's inter-tool prose cap.
	if tools, ok := up.call(2)["tools"]; ok {
		t.Errorf("regeneration carried tools = %v, want them dropped", tools)
	}
	if tc, ok := up.call(2)["tool_choice"]; ok {
		t.Errorf("regeneration carried tool_choice = %v, want it dropped", tc)
	}
	// It must still carry the executed-tool history, or the model loses its research.
	if role := msgField(up.call(2), -1, "role"); role != "tool" {
		t.Errorf("regeneration last message role = %v, want tool", role)
	}
}

func TestToolLoop_TruncatedFinalKeptWhenRegenerationFails(t *testing.T) {
	up := &scriptedUpstream{queue: []string{
		truncatedAnswerResp("partial answer"),
		`{"choices":[]}`, // regeneration comes back unusable
	}}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"nemotron-3-super","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (degraded, not failed)", rec.Code)
	}
	cc := decodeCompletion(t, rec)
	if cc.Choices[0].Message.Content != "partial answer" {
		t.Errorf("content = %q, want the truncated answer preserved", cc.Choices[0].Message.Content)
	}
	// Truncation must stay visible rather than being relabelled "stop".
	if cc.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %q, want length", cc.Choices[0].FinishReason)
	}
}

// reasoningOnlyLengthResp is what a reasoning model returns when the client's
// max_tokens runs out while it is still thinking: reasoning_content only,
// empty content, finish_reason "length" (observed from Nemotron Lightning on
// llama.cpp — its reasoning run alone exceeds any small completion budget).
func reasoningOnlyLengthResp(reasoning string, completionTokens int) string {
	return fmt.Sprintf(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"","reasoning_content":%q},"finish_reason":"length"}],"usage":{"prompt_tokens":19,"completion_tokens":%d,"total_tokens":%d}}`, reasoning, completionTokens, completionTokens+19)
}

// An empty answer whose finish_reason is "length" with reasoning attached is
// the client's own max_tokens cap, not a proxy failure — the backend itself
// answers 200 for it. Returning 502 here made every small-max_tokens plain
// completion on the lightning route look like an outage.
func TestToolLoop_ReasoningOnlyLengthCapPassedThrough(t *testing.T) {
	up := &scriptedUpstream{queue: []string{
		reasoningOnlyLengthResp("thinking about how to answer…", 30),
		reasoningOnlyLengthResp("thinking again, still over budget…", 30), // regeneration without tools
	}}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"nemotron-3-super","max_tokens":30,"messages":[{"role":"user","content":"Say ok."}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	cc := decodeCompletion(t, rec)
	if cc.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %q, want length so the client sees its cap", cc.Choices[0].FinishReason)
	}
	if got := cc.Choices[0].Message.Content; got != "" {
		t.Errorf("content = %q, want empty passthrough", got)
	}
	if cc.Choices[0].Message.ReasoningContent == "" {
		t.Error("reasoning_content missing — the truncated thinking must reach the client")
	}
	if up.callCount() != 2 {
		t.Errorf("upstream calls = %d, want 2 (loop round + regeneration)", up.callCount())
	}
}

// Streaming must pass the same cap through instead of failing before SSE.
func TestToolLoop_ReasoningOnlyLengthCapStreamingNot502(t *testing.T) {
	finalSSE := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking…\"},\"finish_reason\":\"length\"}]}\n\n" +
		"data: [DONE]\n\n"
	up := &scriptedUpstream{
		queue: []string{reasoningOnlyLengthResp("thinking…", 30)},
		sse:   finalSSE,
	}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"nemotron-3-super","stream":true,"max_tokens":30,"messages":[{"role":"user","content":"Say ok."}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "data:") {
		t.Errorf("no SSE relayed; body = %s", rec.Body.String())
	}
}

// When the regenerated final is genuinely empty (no reasoning, no "length"),
// the 502 must describe the regenerated backend message — regenerateFinal used
// to return a zero lastRaw, so the error claimed finish_reason="" / 0 tokens
// no matter what the backend actually said.
func TestToolLoop_EmptyRegenerationReports502WithRealDiagnostics(t *testing.T) {
	up := &scriptedUpstream{queue: []string{
		truncatedAnswerResp(""), // loop final: empty, capped → triggers regeneration
		emptyAnswerResp(28),     // regeneration: empty again, finish_reason "stop"
	}}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"nemotron-3-super","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}
	msg := decodeErrorMessage(t, rec)
	if !strings.Contains(msg, `finish_reason="stop"`) {
		t.Errorf("error message = %q, want the regenerated finish_reason \"stop\"", msg)
	}
	if !strings.Contains(msg, "28 completion tokens") {
		t.Errorf("error message = %q, want the regenerated token count", msg)
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

// The loop forces stream=false for its internal rounds, but the client's
// stream_options survives in the same body map. That pair is invalid per the
// OpenAI schema: vLLM 400s the whole request ("Stream options can only be
// defined when `stream=True`"), while Atlas silently tolerated it. Regression
// for the dashboard quick-chat breaking when archimedes moved to vLLM.
func TestToolLoop_DropsStreamOptionsOnNonStreamingRounds(t *testing.T) {
	finalSSE := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	up := &scriptedUpstream{
		queue: []string{
			toolCallResp("calculator", `{"expression":"2+2"}`),
			answerResp("buffered final, discarded"),
		},
		sse: finalSSE,
	}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"nemotron-3-super","stream":true,`+
		`"stream_options":{"include_usage":true},`+
		`"messages":[{"role":"user","content":"2+2?"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if up.callCount() == 0 {
		t.Fatal("no upstream calls captured")
	}
	for i := 0; i < up.callCount(); i++ {
		call := up.call(i)
		isStream, _ := call["stream"].(bool)
		_, hasOpts := call["stream_options"]
		if !isStream && hasOpts {
			t.Errorf("call %d: stream=false but stream_options present — vLLM rejects this", i)
		}
	}
}

func TestToolLoop_StreamingToolRoundThenLiveAnswer(t *testing.T) {
	up := &scriptedUpstream{sseQueue: []string{
		sseToolCall("calculator", `{"expression":"2+2"}`), // round 1: tool
		sseAnswer("The answer", " is 4"),                  // round 2: streamed live
	}}
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
	for _, want := range []string{"The answer", " is 4", "[DONE]"} {
		if !strings.Contains(got, want) {
			t.Errorf("stream missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "calculator") {
		t.Errorf("proxy tool call leaked to the client:\n%s", got)
	}
	// Two upstream hits, both streamed, both with tools: no regeneration.
	if up.callCount() != 2 {
		t.Fatalf("upstream calls = %d, want 2", up.callCount())
	}
	for i := 0; i < 2; i++ {
		c := up.call(i)
		if stream, _ := c["stream"].(bool); !stream {
			t.Errorf("call %d was not stream:true", i)
		}
		if _, present := c["tools"]; !present {
			t.Errorf("call %d dropped the tools", i)
		}
		so, _ := c["stream_options"].(map[string]any)
		if so["include_usage"] != true {
			t.Errorf("call %d stream_options = %v, want include_usage", i, so)
		}
	}
	if role := msgField(up.call(1), -1, "role"); role != "tool" {
		t.Errorf("round 2 history last role = %v, want tool", role)
	}
	// Usage summed over both rounds (1+1 prompt, 5+2 completion).
	if !strings.Contains(got, `"prompt_tokens":2`) || !strings.Contains(got, `"completion_tokens":7`) {
		t.Errorf("usage not summed across rounds:\n%s", got)
	}
}

// A plain answer is one streamed backend call, relayed as it arrives — the
// old driver generated it twice.
func TestToolLoop_StreamingPlainAnswerIsOneCall(t *testing.T) {
	up := &scriptedUpstream{sseQueue: []string{sseAnswer("hello", " world")}}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"nemotron-3-super","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if up.callCount() != 1 {
		t.Fatalf("upstream calls = %d, want 1", up.callCount())
	}
	got := rec.Body.String()
	if !strings.Contains(got, `"content":"hello"`) || !strings.Contains(got, `"content":" world"`) {
		t.Errorf("deltas not relayed one by one:\n%s", got)
	}
	if !strings.Contains(got, `"role":"assistant"`) {
		t.Errorf("first delta should carry the role:\n%s", got)
	}
	// llama.cpp's timings ride on the terminal chunk and must survive.
	if !strings.Contains(got, `"timings"`) || !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Errorf("terminal chunk (finish_reason + timings) not replayed:\n%s", got)
	}
}

// A model without a tool parser writes <tool_call> in content. The tag must
// never reach the client, even when it is split across deltas.
func TestToolLoop_StreamingTextualToolCallNotLeaked(t *testing.T) {
	tc := `<tool_call>{"name":"calculator","arguments":{"expression":"3*3"}}</tool_call>`
	up := &scriptedUpstream{sseQueue: []string{
		sseAnswer("Let me compute. <tool", "_call>"+tc[len("<tool_call>"):]),
		sseAnswer("It is 9."),
	}}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"nemotron-3-super","stream":true,"messages":[{"role":"user","content":"3*3?"}]}`)
	if got := streamedContent(t, rec.Body.String()); got != "Let me compute. It is 9." {
		t.Errorf("streamed content = %q, want the prose without the tool call", got)
	}
	if up.callCount() != 2 {
		t.Fatalf("upstream calls = %d, want 2 (textual call executed)", up.callCount())
	}
	if role := msgField(up.call(1), -1, "role"); role != "tool" {
		t.Errorf("textual call was not executed; last role = %v", role)
	}
}

// "<tool_call>" that is not a parseable call is prose and goes out after all.
func TestToolLoop_StreamingLiteralTagInProseFlushed(t *testing.T) {
	up := &scriptedUpstream{sseQueue: []string{sseAnswer("Models emit <tool_call> tags", " when unparsed.")}}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"nemotron-3-super","stream":true,"messages":[{"role":"user","content":"?"}]}`)
	if got := streamedContent(t, rec.Body.String()); got != "Models emit <tool_call> tags when unparsed." {
		t.Errorf("streamed content = %q, want the prose verbatim", got)
	}
	if up.callCount() != 1 {
		t.Errorf("upstream calls = %d, want 1", up.callCount())
	}
}

func TestToolLoop_StreamingClientToolBreakout(t *testing.T) {
	up := &scriptedUpstream{sseQueue: []string{sseToolCall("get_weather", `{"city":"NYC"}`)}}
	server := httptest.NewServer(up.handler())
	defer server.Close()
	p := newToolProxy(t, &transportRedirect{to: server.URL, rt: http.DefaultTransport})

	rec := postChat(t, p, `{"model":"nemotron-3-super","stream":true,"messages":[{"role":"user","content":"weather?"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"tool_calls", "get_weather", "NYC", "[DONE]", "chat.completion"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %q:\n%s", want, body)
		}
	}
	if up.callCount() != 1 {
		t.Errorf("upstream calls = %d, want 1 (no re-stream on client breakout)", up.callCount())
	}
}

// streamedContent concatenates every content delta in an SSE body.
func streamedContent(t *testing.T, body string) string {
	t.Helper()
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var ch struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			t.Fatalf("bad SSE line %q: %v", data, err)
		}
		for _, c := range ch.Choices {
			b.WriteString(c.Delta.Content)
		}
	}
	return b.String()
}

// sseAnswer is a streamed answer: one content delta per part, then a terminal
// chunk with finish_reason and llama.cpp-style timings, a usage chunk, [DONE].
func sseAnswer(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		c, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": p}, "finish_reason": nil}}})
		b.WriteString("data: " + string(c) + "\n\n")
	}
	b.WriteString(`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"timings":{"predicted_per_second":12.5}}` + "\n\n")
	b.WriteString(`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// sseToolCall streams one structured tool call, arguments split in two.
func sseToolCall(name, args string) string {
	half := len(args) / 2
	c1, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
		map[string]any{"index": 0, "id": "call_abc", "type": "function", "function": map[string]any{"name": name, "arguments": args[:half]}}}}}}})
	c2, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
		map[string]any{"index": 0, "function": map[string]any{"arguments": args[half:]}}}}}}})
	return "data: " + string(c1) + "\n\n" + "data: " + string(c2) + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":5,"total_tokens":6}}` + "\n\n" +
		"data: [DONE]\n\n"
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
