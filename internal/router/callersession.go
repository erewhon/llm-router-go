package router

// The caller's own session id, for the request log only.
//
// A harness names its session on the wire, and that name is the key
// agent-monitor and tokenator already join on. Logging it lets a session's
// router rows be found directly instead of inferred from timing and model.
//
// WHAT EACH HARNESS SENDS (captured against a stub listener, 2026-09-23):
//   - Claude Code 2.1.280: X-Claude-Code-Session-Id: <transcript UUID>, and
//     the same UUID inside metadata.user_id, which is a JSON-encoded string
//     {"device_id":…,"account_uuid":…,"session_id":…}. Older builds encoded
//     it as "user_<hash>_account_<uuid>_session_<uuid>".
//   - opencode 1.18.30 (openai-compatible provider): x-session-id and
//     x-session-affinity, both the ses_… id. x-opencode-session is what it
//     sends to Zen, which a caller may also point at the router.
//   - Pi 0.87.0: nothing. Its rows stay unattributed.
//
// This is log-only. Nothing here forwards, mints or rewrites a header; the
// outbound affinity header is session.go's business, and a value carrying its
// "llmr-" prefix is the router's own, never a caller's.

import (
	"encoding/json"
	"net/http"
	"strings"
)

// maxCallerSessionID caps what one row stores. Real ids are 36 (UUID) or ~30
// (ses_…) characters; anything far longer is not a session id.
const maxCallerSessionID = 128

// callerSessionHeaders is the precedence order: the most specific header
// first, so a harness that sends several agrees with itself anyway and a
// generic header never shadows a named one.
var callerSessionHeaders = []string{
	"X-Claude-Code-Session-Id",
	"X-Session-Id",
	"X-Opencode-Session",
	"X-Session-Affinity",
}

// callerSessionID returns the caller's session id from its headers, falling
// back to an Anthropic metadata.user_id (pass "" when the request has none).
// Returns "" when the caller named no session.
func callerSessionID(h http.Header, anthropicUserID string) string {
	for _, name := range callerSessionHeaders {
		if v := cleanSessionID(h.Get(name)); v != "" {
			return v
		}
	}
	return cleanSessionID(sessionFromUserID(anthropicUserID))
}

// sessionFromUserID pulls the session id out of both metadata.user_id shapes
// Claude Code has used: a JSON object encoded as a string, and the older
// "…_session_<uuid>" suffix.
func sessionFromUserID(uid string) string {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return ""
	}
	if strings.HasPrefix(uid, "{") {
		var v struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(uid), &v) == nil {
			return v.SessionID
		}
		return ""
	}
	if i := strings.LastIndex(uid, "_session_"); i >= 0 {
		return uid[i+len("_session_"):]
	}
	return ""
}

// anthropicUserID reads metadata.user_id from an Anthropic Messages body,
// touching nothing else in it. "" when absent or unparseable.
func anthropicUserID(body []byte) string {
	var req struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if json.Unmarshal(body, &req) != nil {
		return ""
	}
	return req.Metadata.UserID
}

func cleanSessionID(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.HasPrefix(v, sessionIDPrefix) {
		return ""
	}
	if len(v) > maxCallerSessionID {
		v = v[:maxCallerSessionID]
	}
	return v
}
