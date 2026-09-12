package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erewhon/llm-router-go/internal/auth"
	"github.com/erewhon/llm-router-go/internal/router/reqlog"
)

// passthroughAuth builds the middleware the way main.go does: the Anthropic
// paths are attributed, never gated.
func passthroughAuth(t *testing.T, store *auth.Store, keyPrincipals map[string]string) func(http.Handler) http.Handler {
	t.Helper()
	return NewAuthenticator(store, []string{"sk-shared"}, []string{"/health"}, discardLogger()).
		Passthrough(AnthropicPaths...).
		KeyPrincipals(keyPrincipals).
		Middleware()
}

// identityAndAuthEcho reports the identity on the context AND the
// Authorization header the downstream handler (the upstream, in effect) saw.
func identityAndAuthEcho(seen *auth.Identity, ok *bool, authHdr, apiKey *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen, *ok = auth.FromContext(r.Context())
		*authHdr = r.Header.Get("Authorization")
		*apiKey = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusOK)
	})
}

func TestPassthroughIsAttributedOnTheAPIKeyFingerprint(t *testing.T) {
	var (
		seen            auth.Identity
		ok              bool
		authHdr, apiKey string
	)
	h := passthroughAuth(t, patStore(t), nil)(identityAndAuthEcho(&seen, &ok, &authHdr, &apiKey))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("x-api-key", "sk-ant-api03-somebodys-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the passthrough is never gated (body %s)", w.Code, w.Body)
	}
	if !ok {
		t.Fatal("no identity on the context; the busiest path must not stay anonymous")
	}
	fp := auth.Fingerprint("sk-ant-api03-somebodys-key")
	if seen.Principal != auth.AnthropicKeyPrincipalPrefix+fp || seen.TokenID != "anthropic-"+fp {
		t.Errorf("identity = %+v, want anthropic:%s / anthropic-%s", seen, fp, fp)
	}
	if strings.Contains(seen.Principal, "somebodys-key") {
		t.Error("the credential itself leaked into the principal")
	}
	if apiKey != "sk-ant-api03-somebodys-key" {
		t.Errorf("x-api-key = %q, want forwarded untouched", apiKey)
	}
	if seen.ModelScope() != auth.ScopeModelsAll {
		t.Errorf("a fingerprint identity carries no scope (unrestricted); got %s", seen.ModelScope())
	}
}

func TestPassthroughOAuthBearerIsFingerprintedSeparately(t *testing.T) {
	var (
		seen            auth.Identity
		ok              bool
		authHdr, apiKey string
	)
	h := passthroughAuth(t, patStore(t), nil)(identityAndAuthEcho(&seen, &ok, &authHdr, &apiKey))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-claude-code-session")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK || !ok {
		t.Fatalf("status = %d ok=%v, want 200 with an identity", w.Code, ok)
	}
	fp := auth.Fingerprint("sk-ant-oat01-claude-code-session")
	if seen.Principal != auth.AnthropicOAuthPrincipalPrefix+fp {
		t.Errorf("principal = %q, want anthropic-oauth:%s", seen.Principal, fp)
	}
	// The OAuth token is the caller's upstream credential: it must reach the
	// upstream exactly as sent. Only a router PAT gets stripped.
	if authHdr != "Bearer sk-ant-oat01-claude-code-session" {
		t.Errorf("Authorization = %q, want the OAuth bearer forwarded untouched", authHdr)
	}
}

func TestPassthroughPATAlongsideResolvesAPersonAndIsStripped(t *testing.T) {
	store := patStore(t)
	wire, tok, err := store.Mint("erewhon@flatland.org", "claude-code", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var (
		seen            auth.Identity
		ok              bool
		authHdr, apiKey string
	)
	h := passthroughAuth(t, store, nil)(identityAndAuthEcho(&seen, &ok, &authHdr, &apiKey))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+wire)
	req.Header.Set("x-api-key", "sk-ant-api03-real-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK || !ok {
		t.Fatalf("status = %d ok=%v (body %s)", w.Code, ok, w.Body)
	}
	if seen.Principal != "erewhon@flatland.org" || seen.TokenID != tok.ID {
		t.Errorf("identity = %+v, want the PAT's person", seen)
	}
	if authHdr != "" {
		t.Errorf("Authorization = %q, want the router PAT STRIPPED before the upstream sees it", authHdr)
	}
	if apiKey != "sk-ant-api03-real-key" {
		t.Errorf("x-api-key = %q, want the Anthropic key forwarded", apiKey)
	}
}

func TestPassthroughBadPATIsRefused(t *testing.T) {
	// Presenting a PAT is opting in to being checked: a revoked or forged
	// one must not silently degrade to fingerprint attribution.
	store := patStore(t)
	wire, tok, _ := store.Mint("someone", "", nil, nil)
	if err := store.Revoke(tok.ID); err != nil {
		t.Fatal(err)
	}
	var seen auth.Identity
	var ok bool
	var a, k string
	h := passthroughAuth(t, store, nil)(identityAndAuthEcho(&seen, &ok, &a, &k))

	for _, bearer := range []string{wire, "pat_000000000000_forged"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("x-api-key", "sk-ant-api03-real-key")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("bearer %q: status = %d, want 401", bearer[:16], w.Code)
		}
	}
}

func TestPassthroughFingerprintMapNamesThePerson(t *testing.T) {
	fp := auth.Fingerprint("sk-ant-api03-erewhons-key")
	var seen auth.Identity
	var ok bool
	var a, k string
	h := passthroughAuth(t, patStore(t), map[string]string{fp: "erewhon@flatland.org"})(identityAndAuthEcho(&seen, &ok, &a, &k))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("x-api-key", "sk-ant-api03-erewhons-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !ok || seen.Principal != "erewhon@flatland.org" {
		t.Errorf("identity = %+v, want the mapped person", seen)
	}
	if seen.TokenID != "anthropic-"+fp {
		t.Errorf("token id = %q, want the fingerprint kept for audit", seen.TokenID)
	}
}

func TestPassthroughWithNoCredentialCarriesNoIdentity(t *testing.T) {
	var seen auth.Identity
	var ok bool
	var a, k string
	h := passthroughAuth(t, patStore(t), nil)(identityAndAuthEcho(&seen, &ok, &a, &k))
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || ok {
		t.Errorf("status = %d ok=%v, want 200 and no identity (the upstream refuses it)", w.Code, ok)
	}
}

func TestMintRefusesTheAnthropicNamespaces(t *testing.T) {
	store := patStore(t)
	for _, p := range []string{"anthropic:deadbeef", "anthropic-oauth:deadbeef", "legacy:deadbeef"} {
		if _, _, err := store.Mint(p, "", nil, nil); err == nil {
			t.Errorf("Mint(%q) succeeded; synthetic namespaces must be reserved", p)
		}
	}
}

func TestPassthroughHonoursTheTokenScope(t *testing.T) {
	// A models:local PAT sent alongside an Anthropic key: api.anthropic.com
	// is neither fleet hardware nor ZDR-enforceable, so the passthrough must
	// refuse exactly as the OpenAI-shaped endpoints would — and log it.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream was called; a models:local token must never reach api.anthropic.com")
	}))
	defer upstream.Close()
	sink := &reqlog.MemorySink{}
	rt := newTestRouter(t, &transportRedirect{to: upstream.URL, rt: http.DefaultTransport}, WithSink(sink))
	store := patStore(t)
	wire, tok, _ := store.Mint("kid@example.org", "laptop", []string{auth.ScopeModelsLocal}, nil)
	h := passthroughAuth(t, store, nil)(rt.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+wire)
	req.Header.Set("x-api-key", "sk-ant-api03-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "token_scope_denied") {
		t.Fatalf("status = %d, want 403 scope denial (body %s)", w.Code, w.Body)
	}
	recs := sink.Records()
	if len(recs) != 1 {
		t.Fatalf("reqlog records = %d, want 1", len(recs))
	}
	if recs[0].Principal != "kid@example.org" || recs[0].TokenID != tok.ID || recs[0].ErrorClass != errorClassScopeRefused {
		t.Errorf("record = principal %q token %q class %q", recs[0].Principal, recs[0].TokenID, recs[0].ErrorClass)
	}

	// The same person with an unrestricted token goes through.
	wire2, _, _ := store.Mint("kid@example.org", "wide", []string{auth.ScopeModelsAll}, nil)
	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream2.Close()
	rt.transport = &transportRedirect{to: upstream2.URL, rt: http.DefaultTransport}
	req = httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+wire2)
	req.Header.Set("x-api-key", "sk-ant-api03-key")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("unrestricted token: status = %d (body %s)", w.Code, w.Body)
	}
	if recs := sink.Records(); len(recs) != 2 || recs[1].Principal != "kid@example.org" || recs[1].Status != 200 {
		t.Errorf("second record = %+v", recs[len(recs)-1])
	}
}
