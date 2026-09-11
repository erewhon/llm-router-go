package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/erewhon/llm-router-go/internal/auth"
	"github.com/erewhon/llm-router-go/internal/router/reqlog"
)

// scopedChat posts a chat request carrying an identity with the given scope
// on its context — the shape the Authenticator middleware would leave behind
// for a PAT minted with that scope.
func scopedChat(t *testing.T, rt *Router, scope, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	id := auth.Identity{Principal: "night-agent", TokenID: "0123456789ab"}
	if scope != "" {
		id.Scopes = []string{scope}
	}
	req = req.WithContext(auth.WithIdentity(req.Context(), id))
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)
	return rec
}

func TestLocalScopeRefusesADirectlyNamedCloudModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream was called; a models:local token must never reach a cloud seat")
	}))
	defer upstream.Close()
	sink := &reqlog.MemorySink{}
	rt := newPrivacyRouter(t, upstream.URL)
	rt.sink = sink

	rec := scopedChat(t, rt, auth.ScopeModelsLocal, `{"model":"or-seat","messages":[{"role":"user","content":"hi"}]}`, nil)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}
	var env struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error["code"] != "token_scope_denied" || env.Error["type"] != "permission_error" {
		t.Errorf("error = %v, want a scope-flavoured refusal, not a privacy-header one", env.Error)
	}
	if env.Error["scope"] != auth.ScopeModelsLocal || env.Error["token_id"] != "0123456789ab" {
		t.Errorf("refusal must name the scope and the token: %v", env.Error)
	}
	msg, _ := env.Error["message"].(string)
	for _, want := range []string{"or-seat", "token scope models:local", "0123456789ab"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q should mention %q — a 3am log line has to explain itself", msg, want)
		}
	}

	// The denial is an event you will want to query later: principal, token
	// and the refused model, under its own class.
	recs := sink.Records()
	if len(recs) != 1 {
		t.Fatalf("reqlog records = %d, want 1", len(recs))
	}
	r := recs[0]
	if r.Principal != "night-agent" || r.TokenID != "0123456789ab" {
		t.Errorf("attribution = %q/%q, want night-agent/0123456789ab", r.Principal, r.TokenID)
	}
	if r.ResolvedVia != "or-seat" {
		t.Errorf("ResolvedVia = %q, want the refused model", r.ResolvedVia)
	}
	if r.ErrorClass != errorClassScopeRefused {
		t.Errorf("ErrorClass = %q, want %q", r.ErrorClass, errorClassScopeRefused)
	}
	if r.Status != http.StatusForbidden {
		t.Errorf("Status = %d, want 403", r.Status)
	}
	if isUpstreamFailure(r.ErrorClass) {
		t.Error("a scope refusal must not count against the seat's failure rate — nothing was sent")
	}
}

func TestLocalScopeIsEnforcedOnTheResolvedTargetNotTheName(t *testing.T) {
	// The role lists the cloud seat FIRST. A check on the requested name
	// ("private") would say nothing; a check on the first choice would
	// refuse. The scope is a filter on the walk, so the local seat serves.
	var got map[string]any
	upstream := captureUpstream(t, nil, &got, nil)
	defer upstream.Close()
	rt := newPrivacyRouter(t, upstream.URL)

	rec := scopedChat(t, rt, auth.ScopeModelsLocal, `{"model":"private","messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the local candidate (body: %s)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Router-Resolved") != "local-seat" {
		t.Errorf("X-Router-Resolved = %q, want local-seat", rec.Header().Get("X-Router-Resolved"))
	}
	if got["model"] != "openai/gpt-oss-120b" {
		t.Errorf("upstream model = %v, want the local seat's hf_repo", got["model"])
	}
	if rec.Header().Get(PrivacyHeader) != PrivacyLocal {
		t.Errorf("%s = %q, want %q — the enforced tier is reported back", PrivacyHeader, rec.Header().Get(PrivacyHeader), PrivacyLocal)
	}
}

func TestLocalScopeFailsRatherThanFailingOverToACloudSeat(t *testing.T) {
	// The sharp edge from the task: local seat is down, the role's overflow
	// list names a paid provider. An unscoped caller fails over there. A
	// models:local token must get an ERROR, not an external call.
	rt := newPrivacyRouter(t, "")
	base, err := rt.registry.APIBase("local-seat", nil)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(base)
	localHost := u.Host

	t.Run("unscoped caller fails over", func(t *testing.T) {
		tp := &deadBackends{dead: map[string]bool{localHost: true}}
		rt := newPrivacyRouter(t, "")
		rt.transport = tp
		rec := scopedChat(t, rt, "", `{"model":"local-then-cloud-overflow","messages":[]}`, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("control case: status = %d, want 200 via overflow (body: %s)", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("X-Router-Resolved") != "or-seat" {
			t.Errorf("control case resolved to %q, want or-seat", rec.Header().Get("X-Router-Resolved"))
		}
	})

	t.Run("models:local token fails closed", func(t *testing.T) {
		tp := &deadBackends{dead: map[string]bool{localHost: true}}
		rt := newPrivacyRouter(t, "")
		rt.transport = tp
		rec := scopedChat(t, rt, auth.ScopeModelsLocal, `{"model":"local-then-cloud-overflow","messages":[]}`, nil)
		if rec.Code == http.StatusOK {
			t.Fatalf("served (%s) — the scope was violated by failover", rec.Header().Get("X-Router-Resolved"))
		}
		for _, h := range tp.dialed() {
			if strings.Contains(h, "openrouter") {
				t.Fatalf("dialed %s — a models:local token reached a paid provider on the failover path", h)
			}
		}
		if len(tp.dialed()) != 1 || tp.dialed()[0] != localHost {
			t.Errorf("dialed = %v, want only the local seat", tp.dialed())
		}
	})
}

func TestScopeCannotBeLoosenedByTheHeader(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream was called")
	}))
	defer upstream.Close()
	rt := newPrivacyRouter(t, upstream.URL)

	for _, v := range []string{PrivacyAny, PrivacyZDR} {
		rec := scopedChat(t, rt, auth.ScopeModelsLocal,
			`{"model":"or-seat","messages":[]}`, map[string]string{PrivacyHeader: v})
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: %s = %q loosened a models:local token to status %d", PrivacyHeader, PrivacyHeader, v, rec.Code)
		}
	}
}

func TestHeaderCanTightenBeyondTheScope(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream was called")
	}))
	defer upstream.Close()
	rt := newPrivacyRouter(t, upstream.URL)

	// A local_or_zdr token could reach or-seat; the caller says local.
	rec := scopedChat(t, rt, auth.ScopeModelsLocalOrZDR,
		`{"model":"or-seat","messages":[]}`, map[string]string{PrivacyHeader: PrivacyLocal})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	// The header did the refusing, so it is reported as a privacy refusal,
	// not as a scope denial — the caller can fix this one themselves.
	if !strings.Contains(rec.Body.String(), "privacy_tier_unavailable") {
		t.Errorf("a header-driven refusal should carry the privacy code: %s", rec.Body.String())
	}
}

func TestUnscopedAndLegacyIdentitiesAreUnrestricted(t *testing.T) {
	var got map[string]any
	upstream := captureUpstream(t, nil, &got, nil)
	defer upstream.Close()
	rt := newPrivacyRouter(t, upstream.URL)

	for _, id := range []auth.Identity{
		{Principal: "steven", TokenID: "aaaaaaaaaaaa"},                        // pre-scopes PAT: no scope column
		auth.LegacyIdentity("sk-old-shared"),                                  // shared key
		{Principal: "x", TokenID: "b", Scopes: []string{auth.ScopeModelsAll}}, // explicit
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"zen-seat","messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(auth.WithIdentity(req.Context(), id))
		rec := httptest.NewRecorder()
		rt.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 — rollout must change nothing for existing identities (body: %s)",
				id.Principal, rec.Code, rec.Body.String())
		}
	}
}

func TestZDRScopeAttachesTheDirectiveAndRefusesTheUnenforceable(t *testing.T) {
	var got map[string]any
	upstream := captureUpstream(t, nil, &got, nil)
	defer upstream.Close()
	rt := newPrivacyRouter(t, upstream.URL)

	rec := scopedChat(t, rt, auth.ScopeModelsLocalOrZDR, `{"model":"or-seat","messages":[]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("or-seat under models:local_or_zdr: status = %d (body: %s)", rec.Code, rec.Body.String())
	}
	if prov, _ := got["provider"].(map[string]any); prov["zdr"] != true {
		t.Errorf("provider = %#v, want zdr:true put on the wire by the scope", got["provider"])
	}

	rec = scopedChat(t, rt, auth.ScopeModelsLocalOrZDR, `{"model":"zen-seat","messages":[]}`, nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "token_scope_denied") {
		t.Errorf("zen-seat under models:local_or_zdr: status = %d, want a 403 scope denial (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestLocalScopeCoversTheMultipartImageDoor(t *testing.T) {
	// qwen-image-edit in the test registry is external with no node: from a
	// models:local token's point of view it is off-fleet, and the multipart
	// endpoint must refuse exactly as the JSON ones do.
	up := captureUpstream(t, nil, nil, nil)
	defer up.Close()
	sink := &reqlog.MemorySink{}
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport}, WithSink(sink))

	body, ctype := multipartBody(t, "qwen-image-edit")
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", body)
	req.Header.Set("Content-Type", ctype)
	req = req.WithContext(auth.WithIdentity(req.Context(),
		auth.Identity{Principal: "family", TokenID: "cccccccccccc", Scopes: []string{auth.ScopeModelsLocal}}))
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "token_scope_denied") {
		t.Fatalf("status = %d, want 403 scope denial (body: %s)", rec.Code, rec.Body.String())
	}
	recs := sink.Records()
	if len(recs) != 1 || recs[0].ErrorClass != errorClassScopeRefused || recs[0].Principal != "family" {
		t.Errorf("reqlog = %+v, want one scope_refused row attributed to family", recs)
	}
}
