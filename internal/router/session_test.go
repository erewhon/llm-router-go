package router

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/erewhon/llm-router-go/internal/config"
)

// --- deriveSessionID: the value ---------------------------------------------

func chatBody(model string, msgs ...string) []byte {
	return []byte(`{"model":"` + model + `","messages":[` + strings.Join(msgs, ",") + `]}`)
}

const (
	sysMsg   = `{"role":"system","content":"You are a careful Go reviewer."}`
	userMsg1 = `{"role":"user","content":"Review internal/router/session.go"}`
	asstMsg1 = `{"role":"assistant","content":"Looking at it now."}`
	userMsg2 = `{"role":"user","content":"Now check the tests."}`
)

func TestSessionIDIsStableAcrossTheTurnsOfOneConversation(t *testing.T) {
	// The whole point: turn 1 and turn 3 of an agent loop must land on the
	// same provider session, or the prompt cache never gets warm.
	turn1 := deriveSessionID(chatBody("kimi-k2.7-code", sysMsg, userMsg1))
	turn3 := deriveSessionID(chatBody("kimi-k2.7-code", sysMsg, userMsg1, asstMsg1, userMsg2))
	if turn1 != turn3 {
		t.Errorf("session changed between turns: %q -> %q", turn1, turn3)
	}
}

func TestSessionIDSeparatesConversations(t *testing.T) {
	a := deriveSessionID(chatBody("kimi-k2.7-code", sysMsg, userMsg1))
	b := deriveSessionID(chatBody("kimi-k2.7-code", sysMsg, `{"role":"user","content":"something else entirely"}`))
	if a == b {
		t.Error("two different conversations got the same session id")
	}
}

func TestSessionIDSeparatesModels(t *testing.T) {
	// Caches are per model; one conversation on two models is two sessions.
	a := deriveSessionID(chatBody("kimi-k2.7-code", sysMsg, userMsg1))
	b := deriveSessionID(chatBody("minimax-m3", sysMsg, userMsg1))
	if a == b {
		t.Error("the same conversation on two models shared a session id")
	}
}

func TestSessionIDIgnoresContentKeyOrder(t *testing.T) {
	// Two clients writing the same content part with keys in a different
	// order are the same conversation.
	a := deriveSessionID(chatBody("m", `{"role":"user","content":[{"type":"text","text":"hi"}]}`))
	b := deriveSessionID(chatBody("m", `{"role":"user","content":[{"text":"hi","type":"text"}]}`))
	if a != b {
		t.Errorf("key order split one conversation: %q vs %q", a, b)
	}
}

func TestSessionIDIgnoresAMovingCacheControlBreakpoint(t *testing.T) {
	// Anthropic-style clients put cache_control on the newest message, so the
	// first user message has it on turn 1 and loses it on turn 2. That must
	// not give one conversation two sessions.
	withBP := deriveSessionID(chatBody("m", sysMsg,
		`{"role":"user","content":[{"type":"text","text":"review this","cache_control":{"type":"ephemeral"}}]}`))
	withoutBP := deriveSessionID(chatBody("m", sysMsg,
		`{"role":"user","content":[{"type":"text","text":"review this"}]}`,
		asstMsg1, userMsg2))
	if withBP != withoutBP {
		t.Errorf("a moved cache_control breakpoint split the session: %q vs %q", withBP, withoutBP)
	}
}

func TestSessionIDIgnoresPerMessageFieldsOtherThanRoleAndContent(t *testing.T) {
	a := deriveSessionID(chatBody("m", sysMsg, `{"role":"user","content":"hi"}`))
	b := deriveSessionID(chatBody("m", sysMsg, `{"role":"user","content":"hi","name":"steven"}`))
	if a != b {
		t.Errorf("a per-message name field split the session: %q vs %q", a, b)
	}
}

func TestDeveloperRoleCountsAsSystem(t *testing.T) {
	// OpenAI's newer spelling of the system role must anchor a conversation
	// the same way — otherwise it would be treated as the first user message.
	withDev := deriveSessionID(chatBody("m", `{"role":"developer","content":"be terse"}`, userMsg1))
	withDevLater := deriveSessionID(chatBody("m", `{"role":"developer","content":"be terse"}`, userMsg1, asstMsg1, userMsg2))
	if withDev != withDevLater {
		t.Error("a developer-role conversation was not stable across turns")
	}
	noSys := deriveSessionID(chatBody("m", `{"role":"developer","content":"be terse"}`))
	onlyUser := deriveSessionID(chatBody("m", `{"role":"user","content":"be terse"}`))
	if noSys == onlyUser {
		t.Error("developer and user roles with the same text hashed the same — roles are not distinguished")
	}
}

func TestSessionIDWithoutMessagesIsStillDeterministic(t *testing.T) {
	// Completions and image requests have no messages. They still get a
	// valid id — deterministic, so an identical retry reuses its session.
	body := []byte(`{"model":"m","prompt":"a lighthouse at dusk"}`)
	a, b := deriveSessionID(body), deriveSessionID(body)
	if a != b {
		t.Errorf("same body, different ids: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, sessionIDPrefix) {
		t.Errorf("id %q missing the %q prefix", a, sessionIDPrefix)
	}
	if got := deriveSessionID([]byte("not json at all")); !strings.HasPrefix(got, sessionIDPrefix) {
		t.Errorf("non-JSON body produced %q, want a prefixed id rather than nothing", got)
	}
}

func TestSessionIDHasAFixedSafeShape(t *testing.T) {
	id := deriveSessionID(chatBody("m", sysMsg, userMsg1))
	if len(id) != len(sessionIDPrefix)+32 {
		t.Errorf("len(%q) = %d, want %d", id, len(id), len(sessionIDPrefix)+32)
	}
	if strings.ContainsAny(id, " \t\r\n") {
		t.Errorf("id %q contains whitespace", id)
	}
}

// --- setSessionAffinity through the router: who gets the header -------------

// sessionYAML carries the three upstream kinds by their REAL hostnames,
// because the table keys on host: opencode.ai (Zen), openrouter.ai, and a
// local node. zen-first is a chain whose first member is Zen — the shape of
// every broken go/ chain — so the failover test can check the header does not
// follow the request to OpenRouter.
const sessionYAML = `
nodes:
  delphi: {host: delphi.local, gpu: amd, vram_gb: 96}

models:
  zen/k:
    hf_repo: kimi-k2.7-code
    backend: external
    api_base: https://opencode.ai/zen/go/v1
    api_key: sk-literal-zen
  or/k:
    hf_repo: moonshotai/kimi-k2.7-code
    backend: external
    api_base: https://openrouter.ai/api/v1
    api_key: sk-literal-or
  zen-first:
    hf_repo: kimi-k2.7-code-chain
    backend: external
    fallbacks: [zen/k, or/k]
  local-seat:
    hf_repo: openai/gpt-oss-120b
    node: delphi
`

// headerCapture is a test upstream that records the X-Opencode-Session and
// X-Session-Id it receives, and answers with a fixed status.
type headerCapture struct {
	mu        sync.Mutex
	calls     int
	opencode  []string
	sessionID []string
}

func (c *headerCapture) server(t *testing.T, status int) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.calls++
		c.opencode = append(c.opencode, r.Header.Get("X-Opencode-Session"))
		c.sessionID = append(c.sessionID, r.Header.Get("X-Session-Id"))
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func newSessionRouter(t *testing.T, byHost map[string]string) *Router {
	t.Helper()
	reg, err := config.LoadBytes([]byte(sessionYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	return New(reg, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithMode("default"), WithFlushInterval(0), WithTransport(&hostTransport{byHost: byHost}))
}

func postChatWith(t *testing.T, rt *Router, model string, hdr map[string]string, msgs ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[`+strings.Join(msgs, ",")+`]}`))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)
	return rec
}

func TestZenUpstreamGetsASessionHeaderWhenTheCallerSentNone(t *testing.T) {
	// The fix for 400 MissingSessionID.
	zen := &headerCapture{}
	rt := newSessionRouter(t, map[string]string{"opencode.ai": zen.server(t, 200).URL})

	if rec := postChatWith(t, rt, "zen/k", nil, sysMsg, userMsg1); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(zen.opencode) != 1 || !strings.HasPrefix(zen.opencode[0], sessionIDPrefix) {
		t.Fatalf("Zen received X-Opencode-Session = %v, want a router-minted %q id", zen.opencode, sessionIDPrefix)
	}
}

func TestSameConversationReachesZenOnTheSameSession(t *testing.T) {
	// End to end, not just the hash: two turns of one conversation through
	// the router arrive at Zen under one session id.
	zen := &headerCapture{}
	rt := newSessionRouter(t, map[string]string{"opencode.ai": zen.server(t, 200).URL})

	postChatWith(t, rt, "zen/k", nil, sysMsg, userMsg1)
	postChatWith(t, rt, "zen/k", nil, sysMsg, userMsg1, asstMsg1, userMsg2)
	if len(zen.opencode) != 2 || zen.opencode[0] != zen.opencode[1] {
		t.Errorf("two turns reached Zen as %v, want one session id", zen.opencode)
	}
}

func TestCallerSessionHeaderIsNeverOverwritten(t *testing.T) {
	// The OpenCode app sends its own. Replacing it would split one of its
	// conversations across two provider sessions.
	zen := &headerCapture{}
	rt := newSessionRouter(t, map[string]string{"opencode.ai": zen.server(t, 200).URL})

	postChatWith(t, rt, "zen/k", map[string]string{"X-Opencode-Session": "opencode-app-abc123"}, sysMsg, userMsg1)
	if len(zen.opencode) != 1 || zen.opencode[0] != "opencode-app-abc123" {
		t.Errorf("Zen received %v, want the caller's opencode-app-abc123 unchanged", zen.opencode)
	}
}

func TestOpenRouterGetsNoSessionHeaderOfEitherKind(t *testing.T) {
	// No x-opencode-session (meaningless there), and deliberately no
	// x-session-id: a router-minted OpenRouter session can pin a conversation
	// to a non-caching provider. That needs provider ordering first.
	or := &headerCapture{}
	rt := newSessionRouter(t, map[string]string{"openrouter.ai": or.server(t, 200).URL})

	postChatWith(t, rt, "or/k", nil, sysMsg, userMsg1)
	if len(or.opencode) != 1 || or.opencode[0] != "" {
		t.Errorf("OpenRouter received X-Opencode-Session = %v, want none", or.opencode)
	}
	if or.sessionID[0] != "" {
		t.Errorf("OpenRouter received X-Session-Id = %q, want none (not minted by the router)", or.sessionID[0])
	}
}

// recordAll answers every outbound request with 200 and records the host and
// session header it saw — no host mapping to get wrong, so a local backend's
// real address (node host + api_port) is captured whatever it resolves to.
type recordAll struct {
	mu    sync.Mutex
	hosts []string
	hdrs  []string
}

func (r *recordAll) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.hosts = append(r.hosts, req.URL.Host)
	r.hdrs = append(r.hdrs, req.Header.Get("X-Opencode-Session"))
	r.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"choices":[]}`)),
		Request:    req,
	}, nil
}

func TestLocalBackendGetsNoSessionHeader(t *testing.T) {
	reg, err := config.LoadBytes([]byte(sessionYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	rec := &recordAll{}
	rt := New(reg, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithMode("default"), WithFlushInterval(0), WithTransport(rec))

	if got := postChatWith(t, rt, "local-seat", nil, sysMsg, userMsg1); got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", got.Code, got.Body.String())
	}
	if len(rec.hosts) != 1 {
		t.Fatalf("outbound requests = %d, want 1", len(rec.hosts))
	}
	if !strings.HasPrefix(rec.hosts[0], "delphi.local") {
		t.Fatalf("request went to %q, want the local node — the test is not exercising a local backend", rec.hosts[0])
	}
	if rec.hdrs[0] != "" {
		t.Errorf("local backend %s received X-Opencode-Session = %q, want none", rec.hosts[0], rec.hdrs[0])
	}
}

func TestSessionHeaderDoesNotFollowAFailoverToOpenRouter(t *testing.T) {
	// The chain shape that broke coder-hard: Zen first, OpenRouter behind it.
	// Zen fails with a 5xx, the chain falls to OpenRouter — and the header
	// the router added for Zen must not ride along. This is what writing to
	// the per-attempt outbound copy, rather than the inbound request, buys.
	zen, or := &headerCapture{}, &headerCapture{}
	rt := newSessionRouter(t, map[string]string{
		"opencode.ai":   zen.server(t, http.StatusBadGateway).URL,
		"openrouter.ai": or.server(t, 200).URL,
	})

	rec := postChatWith(t, rt, "zen-first", nil, sysMsg, userMsg1)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failing over (body: %s)", rec.Code, rec.Body.String())
	}
	if zen.calls != 1 || !strings.HasPrefix(zen.opencode[0], sessionIDPrefix) {
		t.Fatalf("Zen attempt: calls=%d header=%v, want one call carrying a session id", zen.calls, zen.opencode)
	}
	if or.calls != 1 {
		t.Fatalf("OpenRouter calls = %d, want 1 (the failover)", or.calls)
	}
	if or.opencode[0] != "" {
		t.Errorf("OpenRouter received X-Opencode-Session = %q — the Zen header leaked across the failover", or.opencode[0])
	}
}
