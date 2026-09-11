package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/erewhon/llm-router-go/internal/auth"
	"github.com/erewhon/llm-router-go/internal/router/reqlog"
)

const (
	dashSecret = "caddy-only-knows-this"
	ownerEmail = "owner@example.org"
	kidEmail   = "kid@example.org"
)

// dashWith builds a dashboard handler with identity configured, over a
// router that has a PAT store to mint into.
func dashWith(t *testing.T, rt *Router, store *auth.Store, secret string) http.Handler {
	t.Helper()
	return rt.DashboardHandler(DashboardConfig{
		APIBase:    "http://localhost:4010",
		Tokens:     store,
		AuthSecret: secret,
		Owners:     []string{" Owner@Example.org "}, // normalised, so case and space do not matter
	})
}

// asProxy stamps the headers Caddy would: the shared secret and the identity.
func asProxy(req *http.Request, email string) *http.Request {
	req.Header.Set(DashboardAuthHeader, dashSecret)
	req.Header.Set("X-Auth-Request-Email", email)
	return req
}

func dashDo(t *testing.T, h http.Handler, req *http.Request) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 && strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec, out
}

func TestDashboardTokensOffWithoutASecret(t *testing.T) {
	rt := newTestRouter(t, nil)
	h := rt.DashboardHandler(DashboardConfig{APIBase: "http://localhost:4010", Tokens: patStore(t)})

	req := httptest.NewRequest(http.MethodGet, "/api/tokens", nil)
	req.Header.Set("X-Auth-Request-Email", kidEmail) // a header alone must buy nothing
	rec, out := dashDo(t, h, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: without a secret the listener cannot know who is asking (body %s)", rec.Code, rec.Body)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "not configured") {
		t.Errorf("error should say self-service is unconfigured: %v", out)
	}
}

func TestDashboardForgedIdentityHeaderIsRefused(t *testing.T) {
	// The threat this whole gate exists for: a request straight to the VIP,
	// bypassing Caddy, that simply sets the identity header. Without the
	// secret it must mint nothing, chat nothing, and read nothing.
	rt := newTestRouter(t, nil)
	h := dashWith(t, rt, patStore(t), dashSecret)

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/tokens", ""},
		{http.MethodPost, "/api/tokens", `{"label":"x"}`},
		{http.MethodDelete, "/api/tokens/abcdefabcdef", ""},
		{http.MethodPost, "/api/chat", `{"model":"coder","message":"hi"}`},
		{http.MethodGet, "/api/usage", ""},
	} {
		for _, secret := range []string{"", "wrong", dashSecret + "x"} {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("X-Auth-Request-Email", ownerEmail)
			if secret != "" {
				req.Header.Set(DashboardAuthHeader, secret)
			}
			rec, _ := dashDo(t, h, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with secret %q: status = %d, want 401", tc.method, tc.path, secret, rec.Code)
			}
		}
	}

	// And the read-only panels are untouched by the gate.
	req := httptest.NewRequest(http.MethodGet, "/api/router-metrics", nil)
	rec, _ := dashDo(t, h, req)
	if rec.Code != http.StatusOK {
		t.Errorf("/api/router-metrics = %d, want 200 — the gate covers identity-bearing routes only", rec.Code)
	}
}

func TestDashboardSecretWithoutIdentityIsRefused(t *testing.T) {
	rt := newTestRouter(t, nil)
	h := dashWith(t, rt, patStore(t), dashSecret)
	req := httptest.NewRequest(http.MethodGet, "/api/tokens", nil)
	req.Header.Set(DashboardAuthHeader, dashSecret)
	rec, _ := dashDo(t, h, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when the proxy sent no identity", rec.Code)
	}
}

func TestDashboardFamilyMintsLocalOnlyAndCannotWiden(t *testing.T) {
	rt := newTestRouter(t, nil)
	store := patStore(t)
	h := dashWith(t, rt, store, dashSecret)

	// What may I mint?
	rec, out := dashDo(t, h, asProxy(httptest.NewRequest(http.MethodGet, "/api/tokens", nil), kidEmail))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if out["principal"] != kidEmail || out["owner"] != false {
		t.Errorf("list = %v, want principal %s, owner false", out, kidEmail)
	}
	allowed, _ := out["allowed_scopes"].([]any)
	if len(allowed) != 1 || allowed[0] != auth.ScopeModelsLocal {
		t.Errorf("allowed_scopes = %v, want [%s] only", allowed, auth.ScopeModelsLocal)
	}

	// Widening is an owner action.
	req := asProxy(httptest.NewRequest(http.MethodPost, "/api/tokens",
		strings.NewReader(`{"label":"laptop","scope":"models:*"}`)), kidEmail)
	rec, _ = dashDo(t, h, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("family minting models:*: status = %d, want 403", rec.Code)
	}
	req = asProxy(httptest.NewRequest(http.MethodPost, "/api/tokens",
		strings.NewReader(`{"label":"laptop","scope":"models:local_or_zdr"}`)), kidEmail)
	rec, _ = dashDo(t, h, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("family minting models:local_or_zdr: status = %d, want 403 — it can still spend", rec.Code)
	}

	// A blank scope defaults to the safest, and a body "principal" is ignored.
	req = asProxy(httptest.NewRequest(http.MethodPost, "/api/tokens",
		strings.NewReader(`{"label":"laptop","principal":"`+ownerEmail+`","expires_days":30}`)), kidEmail)
	rec, out = dashDo(t, h, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body)
	}
	if out["principal"] != kidEmail {
		t.Fatalf("minted for %v — the body's principal field was honoured", out["principal"])
	}
	if out["scope"] != auth.ScopeModelsLocal {
		t.Errorf("scope = %v, want the family default %s", out["scope"], auth.ScopeModelsLocal)
	}
	wire, _ := out["token"].(string)
	if !auth.LooksLikePAT(wire) {
		t.Fatalf("token = %q, want a pat_ wire token shown once", wire)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("the one-time reveal must be uncacheable")
	}
	if out["expires_at"] == nil {
		t.Error("expires_days was given; expires_at should be set")
	}

	// The list shows it, without the secret and without the hash.
	rec, out = dashDo(t, h, asProxy(httptest.NewRequest(http.MethodGet, "/api/tokens", nil), kidEmail))
	if strings.Contains(rec.Body.String(), wire) {
		t.Fatal("the wire token was re-fetchable from the list")
	}
	if strings.Contains(rec.Body.String(), "secret_hash") || strings.Contains(rec.Body.String(), auth.HashSecret(strings.SplitN(wire, "_", 3)[2])) {
		t.Fatal("the list leaked the secret hash")
	}
	toks, _ := out["tokens"].([]any)
	if len(toks) != 1 {
		t.Fatalf("tokens = %v, want the one just minted", toks)
	}
	tok := toks[0].(map[string]any)
	if tok["state"] != "active" || tok["scope"] != auth.ScopeModelsLocal || tok["label"] != "laptop" {
		t.Errorf("token row = %v", tok)
	}

	// The token authenticates at the front door as this person, with the
	// family scope: local seats yes, a paid provider no.
	id, err := store.Verify(wire)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.Principal != kidEmail || id.ModelScope() != auth.ScopeModelsLocal {
		t.Errorf("identity = %+v", id)
	}
}

func TestDashboardOwnerMayMintUnrestricted(t *testing.T) {
	rt := newTestRouter(t, nil)
	h := dashWith(t, rt, patStore(t), dashSecret)

	rec, out := dashDo(t, h, asProxy(httptest.NewRequest(http.MethodGet, "/api/tokens", nil), "OWNER@example.org"))
	if rec.Code != http.StatusOK || out["owner"] != true {
		t.Fatalf("owner list = %d %v", rec.Code, out)
	}
	req := asProxy(httptest.NewRequest(http.MethodPost, "/api/tokens",
		strings.NewReader(`{"label":"opencode","scope":"models:*"}`)), ownerEmail)
	rec, out = dashDo(t, h, req)
	if rec.Code != http.StatusCreated || out["scope"] != auth.ScopeModelsAll {
		t.Fatalf("owner minting models:*: %d %v", rec.Code, out)
	}
	// But a blank scope is still local-only, even for the owner: safest by
	// default, widest by choice.
	req = asProxy(httptest.NewRequest(http.MethodPost, "/api/tokens", strings.NewReader(`{"label":"x"}`)), ownerEmail)
	rec, out = dashDo(t, h, req)
	if rec.Code != http.StatusCreated || out["scope"] != auth.ScopeModelsLocal {
		t.Fatalf("owner blank scope: %d %v, want %s", rec.Code, out, auth.ScopeModelsLocal)
	}
}

func TestDashboardMintValidation(t *testing.T) {
	rt := newTestRouter(t, nil)
	h := dashWith(t, rt, patStore(t), dashSecret)
	for _, body := range []string{
		`{}`,                                   // no label
		`{"label":"   "}`,                      // blank label
		`{"label":"x","scope":"models:cloud"}`, // unknown scope
		`{"label":"x","expires_days":-1}`,
		`{"label":"x","expires_days":99999}`,
		`not json`,
	} {
		req := asProxy(httptest.NewRequest(http.MethodPost, "/api/tokens", strings.NewReader(body)), ownerEmail)
		rec, _ := dashDo(t, h, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, rec.Code)
		}
	}
}

func TestDashboardRevokeIsOwnOnly(t *testing.T) {
	rt := newTestRouter(t, nil)
	store := patStore(t)
	h := dashWith(t, rt, store, dashSecret)

	wire, tok, err := store.Mint(ownerEmail, "owner's", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The kid cannot see it...
	rec, out := dashDo(t, h, asProxy(httptest.NewRequest(http.MethodGet, "/api/tokens", nil), kidEmail))
	if toks, _ := out["tokens"].([]any); rec.Code != http.StatusOK || len(toks) != 0 {
		t.Fatalf("kid's list shows %v — another principal's tokens leaked", out["tokens"])
	}
	// ...nor revoke it, nor learn that it exists.
	rec, _ = dashDo(t, h, asProxy(httptest.NewRequest(http.MethodDelete, "/api/tokens/"+tok.ID, nil), kidEmail))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("kid revoking owner's token: status = %d, want 404", rec.Code)
	}
	rec, _ = dashDo(t, h, asProxy(httptest.NewRequest(http.MethodDelete, "/api/tokens/000000000000", nil), kidEmail))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("kid revoking a nonexistent token: status = %d, want the same 404", rec.Code)
	}
	if _, err := store.Verify(wire); err != nil {
		t.Fatalf("owner's token was affected: %v", err)
	}
	// The owner can, and it stops working on the next request.
	rec, _ = dashDo(t, h, asProxy(httptest.NewRequest(http.MethodDelete, "/api/tokens/"+tok.ID, nil), ownerEmail))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("owner revoke: status = %d, want 204 (body %s)", rec.Code, rec.Body)
	}
	if _, err := store.Verify(wire); err == nil {
		t.Fatal("revoked token still verifies")
	}
}

func TestDashboardChatIsAttributedAndScoped(t *testing.T) {
	var got map[string]any
	upstream := captureUpstream(t, nil, &got, nil)
	defer upstream.Close()
	sink := &reqlog.MemorySink{}
	rt := newPrivacyRouter(t, upstream.URL)
	rt.sink = sink
	h := dashWith(t, rt, patStore(t), dashSecret)

	// A family member's quick chat to a local seat: served, and the reqlog
	// row names the person — no more "unattributed" for dashboard traffic.
	req := asProxy(httptest.NewRequest(http.MethodPost, "/api/chat",
		strings.NewReader(`{"model":"local-seat","message":"hi"}`)), kidEmail)
	rec, _ := dashDo(t, h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat local-seat: %d %s", rec.Code, rec.Body)
	}
	// To a paid provider: refused by the family scope, as a token would be.
	req = asProxy(httptest.NewRequest(http.MethodPost, "/api/chat",
		strings.NewReader(`{"model":"or-seat","message":"hi"}`)), kidEmail)
	rec, _ = dashDo(t, h, req)
	if !strings.Contains(rec.Body.String(), "token_scope_denied") {
		t.Fatalf("family chat to or-seat should be refused by scope; got %d %s", rec.Code, rec.Body)
	}
	// The owner's chat to the same seat goes through.
	req = asProxy(httptest.NewRequest(http.MethodPost, "/api/chat",
		strings.NewReader(`{"model":"or-seat","message":"hi"}`)), ownerEmail)
	rec, _ = dashDo(t, h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner chat or-seat: %d %s", rec.Code, rec.Body)
	}

	recs := sink.Records()
	if len(recs) != 3 {
		t.Fatalf("reqlog records = %d, want 3", len(recs))
	}
	if recs[0].Principal != kidEmail || recs[0].TokenID != dashboardTokenID {
		t.Errorf("record 0 attribution = %q/%q, want %s/%s", recs[0].Principal, recs[0].TokenID, kidEmail, dashboardTokenID)
	}
	if recs[1].ErrorClass != errorClassScopeRefused || recs[1].Principal != kidEmail {
		t.Errorf("record 1 = class %q principal %q, want scope_refused for %s", recs[1].ErrorClass, recs[1].Principal, kidEmail)
	}
	if recs[2].Principal != ownerEmail || recs[2].Status != http.StatusOK {
		t.Errorf("record 2 = %q/%d, want %s/200", recs[2].Principal, recs[2].Status, ownerEmail)
	}
}

func TestDashboardUsageByPrincipal(t *testing.T) {
	sink := &reqlog.MemorySink{}
	now := time.Now()
	p, c := 100, 50
	cost := 0.0123
	sink.Log(reqlog.Record{TS: now, Principal: ownerEmail, Status: 200, PromptTokens: &p, CompletionTokens: &c, UpstreamCostUSD: &cost})
	sink.Log(reqlog.Record{TS: now, Principal: ownerEmail, Status: 502, ErrorClass: "connect"})
	sink.Log(reqlog.Record{TS: now, Principal: kidEmail, Status: 200, PromptTokens: &p})
	sink.Log(reqlog.Record{TS: now, Principal: kidEmail, Status: 403, ErrorClass: errorClassScopeRefused})
	sink.Log(reqlog.Record{TS: now.Add(-48 * time.Hour), Principal: kidEmail, Status: 200}) // outside the window
	sink.Log(reqlog.Record{TS: now, Status: 200})                                           // unattributed

	rt := newTestRouter(t, nil, WithSink(sink))
	h := dashWith(t, rt, patStore(t), dashSecret)

	rec, out := dashDo(t, h, asProxy(httptest.NewRequest(http.MethodGet, "/api/usage?hours=24", nil), ownerEmail))
	if rec.Code != http.StatusOK || out["available"] != true {
		t.Fatalf("owner usage: %d %v", rec.Code, out)
	}
	rows, _ := out["rows"].([]any)
	if len(rows) != 3 {
		t.Fatalf("owner sees %d rows, want 3 (owner, kid, unattributed): %v", len(rows), rows)
	}
	byP := map[string]map[string]any{}
	for _, r := range rows {
		m := r.(map[string]any)
		byP[m["principal"].(string)] = m
	}
	if o := byP[ownerEmail]; o["requests"] != 2.0 || o["failures"] != 1.0 || o["prompt_tokens"] != 100.0 || o["cost_usd"] != cost {
		t.Errorf("owner row = %v", o)
	}
	if k := byP[kidEmail]; k["requests"] != 2.0 || k["failures"] != 1.0 {
		t.Errorf("kid row = %v (a scope refusal is a failure worth seeing)", k)
	}
	if _, ok := byP[""]; !ok {
		t.Error("unattributed traffic must be visible as its own row, not silently absent")
	}

	// The kid sees only their own row.
	rec, out = dashDo(t, h, asProxy(httptest.NewRequest(http.MethodGet, "/api/usage", nil), kidEmail))
	rows, _ = out["rows"].([]any)
	if rec.Code != http.StatusOK || len(rows) != 1 || rows[0].(map[string]any)["principal"] != kidEmail {
		t.Errorf("kid usage = %d %v, want their one row", rec.Code, out)
	}
}

func TestDashboardUsageUnavailableWithoutAQueryableSink(t *testing.T) {
	rt := newTestRouter(t, nil) // NopSink
	h := dashWith(t, rt, patStore(t), dashSecret)
	rec, out := dashDo(t, h, asProxy(httptest.NewRequest(http.MethodGet, "/api/usage", nil), ownerEmail))
	if rec.Code != http.StatusOK || out["available"] != false {
		t.Fatalf("usage over NopSink = %d %v, want available:false so the panel hides itself", rec.Code, out)
	}
}
