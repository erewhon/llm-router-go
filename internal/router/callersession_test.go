package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erewhon/llm-router-go/internal/router/reqlog"
)

func TestCallerSessionID(t *testing.T) {
	const cc = "69c5b200-84eb-4610-a881-83fc7cf86b1d"
	jsonUID := `{"device_id":"6a02","account_uuid":"","session_id":"` + cc + `"}`
	cases := []struct {
		name    string
		headers map[string]string
		userID  string
		want    string
	}{
		{"claude code header", map[string]string{"X-Claude-Code-Session-Id": cc}, "", cc},
		{"opencode header", map[string]string{"x-session-id": "ses_f30a02a45ffeiIxjTTvV5rZ3WK", "x-session-affinity": "ses_f30a02a45ffeiIxjTTvV5rZ3WK"}, "", "ses_f30a02a45ffeiIxjTTvV5rZ3WK"},
		{"zen-style header", map[string]string{"x-opencode-session": "ses_zen"}, "", "ses_zen"},
		{"affinity only", map[string]string{"x-session-affinity": "ses_aff"}, "", "ses_aff"},
		{"named header beats generic", map[string]string{"X-Session-Id": "generic", "X-Claude-Code-Session-Id": cc}, "", cc},
		{"header beats metadata", map[string]string{"X-Claude-Code-Session-Id": cc}, `{"session_id":"other"}`, cc},
		{"metadata json string", nil, jsonUID, cc},
		{"metadata legacy suffix", nil, "user_abc_account_def_session_" + cc, cc},
		{"metadata without session", nil, "user_abc", ""},
		{"metadata malformed json", nil, `{"session_id":`, ""},
		{"router-minted value dropped", map[string]string{"X-Opencode-Session": "llmr-0123abcd"}, "", ""},
		{"router-minted falls through", map[string]string{"X-Opencode-Session": "llmr-0123abcd", "X-Session-Affinity": "ses_real"}, "", "ses_real"},
		{"whitespace trimmed", map[string]string{"X-Session-Id": "  ses_pad  "}, "", "ses_pad"},
		{"nothing", nil, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range c.headers {
				h.Set(k, v)
			}
			if got := callerSessionID(h, c.userID); got != c.want {
				t.Errorf("callerSessionID = %q, want %q", got, c.want)
			}
		})
	}
}

func TestCallerSessionID_CapsLength(t *testing.T) {
	h := http.Header{}
	h.Set("X-Session-Id", strings.Repeat("a", 500))
	if got := callerSessionID(h, ""); len(got) != maxCallerSessionID {
		t.Errorf("len = %d, want %d", len(got), maxCallerSessionID)
	}
}

func TestAnthropicUserID_ReadsOnlyMetadata(t *testing.T) {
	body := `{"model":"m","metadata":{"user_id":"user_x_session_abc"},"messages":[{"role":"user","content":"secret"}]}`
	if got := anthropicUserID([]byte(body)); got != "user_x_session_abc" {
		t.Errorf("anthropicUserID = %q", got)
	}
	if got := anthropicUserID([]byte(`not json`)); got != "" {
		t.Errorf("anthropicUserID(garbage) = %q, want empty", got)
	}
}

// The passthrough logs the Claude Code session id from the header, and from
// metadata.user_id when an older client sent no header.
func TestAnthropic_LogsCallerSessionID(t *testing.T) {
	const cc = "69c5b200-84eb-4610-a881-83fc7cf86b1d"
	for _, tc := range []struct {
		name   string
		header string
		body   string
	}{
		{"header", cc, `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`},
		{"metadata", "", `{"model":"claude-sonnet-4-5","metadata":{"user_id":"{\"device_id\":\"d\",\"session_id\":\"` + cc + `\"}"},"messages":[{"role":"user","content":"hi"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &anthropicUpstream{
				respHeaders: map[string]string{"Content-Type": "application/json"},
				respBody:    `{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`,
			}
			srv := up.server(t)
			defer srv.Close()
			mem := &reqlog.MemorySink{}
			rt := newTestRouter(t, &transportRedirect{to: srv.URL, rt: http.DefaultTransport}, WithSink(mem))

			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-api-key", "sk-ant-client-key")
			if tc.header != "" {
				req.Header.Set("X-Claude-Code-Session-Id", tc.header)
			}
			rec := httptest.NewRecorder()
			rt.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if string(up.body) != tc.body {
				t.Errorf("upstream body changed")
			}
			recs := mem.Records()
			if len(recs) != 1 || recs[0].SessionID != cc {
				t.Fatalf("records = %+v, want one with SessionID %q", recs, cc)
			}
		})
	}
}

// An OpenAI-shaped request through the main proxy logs opencode's session id.
func TestChat_LogsCallerSessionID(t *testing.T) {
	var body map[string]any
	up := captureUpstream(t, nil, &body, nil)
	defer up.Close()
	mem := &reqlog.MemorySink{}
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport}, WithSink(mem))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"coder","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-session-id", "ses_f30a02a45ffeiIxjTTvV5rZ3WK")
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	recs := mem.Records()
	if len(recs) != 1 || recs[0].SessionID != "ses_f30a02a45ffeiIxjTTvV5rZ3WK" {
		t.Fatalf("records = %+v, want one with the opencode session id", recs)
	}
}
