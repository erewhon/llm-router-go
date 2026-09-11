package router

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/erewhon/llm-router-go/internal/auth"
)

// discardLogger keeps rejection logging out of test output; the middleware
// logs every 401 with its reason.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestRequireBearer(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mw := RequireBearer([]string{"sk-good", "sk-also-good"}, []string{"/health", "/metrics"})

	cases := []struct {
		name       string
		path       string
		auth       string
		wantStatus int
		wantSub    string
	}{
		{"valid bearer", "/v1/models", "Bearer sk-good", 200, "ok"},
		{"valid alt bearer", "/v1/chat/completions", "Bearer sk-also-good", 200, "ok"},
		{"missing auth header", "/v1/models", "", 401, "missing bearer"},
		{"wrong scheme", "/v1/models", "Basic abc==", 401, "missing bearer"},
		{"wrong key", "/v1/models", "Bearer sk-bad", 401, "invalid api key"},
		{"empty bearer", "/v1/models", "Bearer ", 401, "invalid api key"},
		{"exempt path no auth", "/health", "", 200, "ok"},
		{"exempt path with bad auth", "/metrics", "Bearer sk-bad", 200, "ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			w := httptest.NewRecorder()
			mw(ok).ServeHTTP(w, req)
			if w.Code != tc.wantStatus {
				t.Errorf("status=%d, want %d", w.Code, tc.wantStatus)
			}
			body, _ := io.ReadAll(w.Result().Body)
			if !strings.Contains(string(body), tc.wantSub) {
				t.Errorf("body=%q, want substring %q", string(body), tc.wantSub)
			}
		})
	}
}

func TestRequireBearerDisabled(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	for _, keys := range [][]string{nil, {}, {""}, {"  ", ""}} {
		mw := RequireBearer(keys, nil)
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		w := httptest.NewRecorder()
		mw(ok).ServeHTTP(w, req)
		if w.Code != 200 {
			t.Errorf("keys=%v: status=%d, want 200 (no-op)", keys, w.Code)
		}
	}
}

func TestRequireBearerErrorEnvelope(t *testing.T) {
	mw := RequireBearer([]string{"sk-good"}, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(w, req)
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type=%q, want application/json", ct)
	}
	body, _ := io.ReadAll(w.Result().Body)
	for _, want := range []string{`"error"`, `"type"`, `"authentication_error"`, `"message"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("body=%q, missing %q", string(body), want)
		}
	}
}

// --- PAT authentication -----------------------------------------------------

// identityEcho reports the identity the middleware put on the request
// context, so tests assert on attribution rather than merely on status.
func identityEcho(seen *auth.Identity, sawIdentity *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.FromContext(r.Context())
		*seen, *sawIdentity = id, ok
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func patStore(t *testing.T) *auth.Store {
	t.Helper()
	s, err := auth.OpenStore(filepath.Join(t.TempDir(), "pat.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAuthenticatorPAT(t *testing.T) {
	store := patStore(t)
	wire, tok, err := store.Mint("steven", "laptop", nil, nil)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	var seen auth.Identity
	var ok bool
	h := NewAuthenticator(store, nil, []string{"/health"}, discardLogger()).
		Middleware()(identityEcho(&seen, &ok))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+wire)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
	}
	if !ok {
		t.Fatal("handler saw no identity on the context")
	}
	if seen.Principal != "steven" {
		t.Errorf("principal = %q, want steven", seen.Principal)
	}
	if seen.TokenID != tok.ID {
		t.Errorf("token id = %q, want %q", seen.TokenID, tok.ID)
	}
}

func TestAuthenticatorRejectsBadPATs(t *testing.T) {
	store := patStore(t)
	wire, tok, _ := store.Mint("steven", "", nil, nil)

	revokedWire, revokedTok, _ := store.Mint("gone", "", nil, nil)
	if err := store.Revoke(revokedTok.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	expiredWire, _, _ := store.Mint("stale", "", nil, &past)

	// A valid id with a wrong secret: the shape is right, the proof is not.
	id, _, _ := auth.Split(wire)
	forged := auth.Prefix + id + "_wrongsecret"

	cases := []struct{ name, bearer string }{
		{"revoked", revokedWire},
		{"expired", expiredWire},
		{"forged secret", forged},
		{"unknown id", auth.Prefix + "aaaaaaaaaaaa_whatever"},
		{"malformed", auth.Prefix + "nothex"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen auth.Identity
			var ok bool
			h := NewAuthenticator(store, nil, nil, discardLogger()).
				Middleware()(identityEcho(&seen, &ok))
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			req.Header.Set("Authorization", "Bearer "+tc.bearer)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", w.Code)
			}
			if ok {
				t.Error("rejected request still reached the handler with an identity")
			}
			// The 401 body must not distinguish revoked from expired from
			// unknown: that would confirm a token id was once real.
			if body := w.Body.String(); !strings.Contains(body, "invalid api key") {
				t.Errorf("body = %s, want the opaque 'invalid api key'", body)
			}
		})
	}
	_ = tok
}

// Legacy shared keys keep working and are attributed, so reqlog can show the
// migration completing rather than leaving it a guess.
func TestAuthenticatorLegacyKeyIsAttributed(t *testing.T) {
	var seen auth.Identity
	var ok bool
	h := NewAuthenticator(nil, []string{"sk-shared"}, nil, discardLogger()).
		Middleware()(identityEcho(&seen, &ok))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-shared")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !ok {
		t.Fatal("legacy key produced no identity; its traffic would be unattributable")
	}
	if !seen.Legacy() {
		t.Errorf("principal %q should report Legacy()", seen.Principal)
	}
	if strings.Contains(seen.Principal, "sk-shared") || strings.Contains(seen.TokenID, "sk-shared") {
		t.Errorf("identity leaks the shared key: %+v", seen)
	}
}

// Both credential kinds must work against one router during the migration.
func TestAuthenticatorAcceptsBothKinds(t *testing.T) {
	store := patStore(t)
	wire, _, _ := store.Mint("steven", "", nil, nil)
	a := NewAuthenticator(store, []string{"sk-shared"}, nil, discardLogger())

	for name, bearer := range map[string]string{"pat": wire, "legacy": "sk-shared"} {
		t.Run(name, func(t *testing.T) {
			var seen auth.Identity
			var ok bool
			h := a.Middleware()(identityEcho(&seen, &ok))
			req := httptest.NewRequest(http.MethodPost, "/v1/models", nil)
			req.Header.Set("Authorization", "Bearer "+bearer)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			if !ok || seen.Principal == "" {
				t.Error("no principal attributed")
			}
		})
	}
}

// A PAT-shaped credential must never be checked against the shared-key set:
// a mistyped token should fail as a token, not masquerade as a bad api key.
func TestPATShapedCredentialNeverFallsBackToSharedKeys(t *testing.T) {
	a := NewAuthenticator(nil, []string{auth.Prefix + "aaaaaaaaaaaa_secret"}, nil, discardLogger())
	var seen auth.Identity
	var ok bool
	h := a.Middleware()(identityEcho(&seen, &ok))
	req := httptest.NewRequest(http.MethodPost, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+auth.Prefix+"aaaaaaaaaaaa_secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401: a pat_-shaped shared key must not authenticate", w.Code)
	}
}

// Auth stays opt-in: no store and no keys means no gate, as before PATs.
func TestAuthenticatorDisabledWhenUnconfigured(t *testing.T) {
	var seen auth.Identity
	var ok bool
	h := NewAuthenticator(nil, nil, nil, discardLogger()).Middleware()(identityEcho(&seen, &ok))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 when auth is unconfigured", w.Code)
	}
	if ok {
		t.Error("unconfigured auth must not fabricate an identity")
	}
}

// Exempt paths bypass auth and therefore carry NO identity. Their reqlog rows
// are unattributed by construction; that must read as "no identity", never as
// a trusted one.
func TestExemptPathCarriesNoIdentity(t *testing.T) {
	store := patStore(t)
	var seen auth.Identity
	var ok bool
	h := NewAuthenticator(store, nil, []string{"/health"}, discardLogger()).
		Middleware()(identityEcho(&seen, &ok))
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ok {
		t.Errorf("exempt path produced an identity %+v, want none", seen)
	}
}

// A token store outage must answer 503, never 401.
//
// This is the safety net under the shared-Postgres SPOF accepted 2026-09-10.
// If it regresses, a database outage tells every caller in the fleet "invalid
// api key" and they all go rotate credentials that were never broken — during
// the incident. The distinction is worth a test that says so.
func TestStoreOutageAnswers503Not401(t *testing.T) {
	dir := t.TempDir()
	store, err := auth.OpenStore(dir + "/pat.db")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	wire, _, err := store.Mint("steven", "laptop", nil, nil)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	a := NewAuthenticator(store, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := a.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Healthy first, so the test proves the token itself is good.
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+wire)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthy store: status = %d, want 200", rec.Code)
	}

	// Now take the store away under it.
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+wire)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("store down: status = %d, want 503 (401 would blame the caller's key)", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"token_store_unavailable", "probably fine"} {
		if !strings.Contains(body, want) {
			t.Errorf("503 body missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "invalid api key") {
		t.Errorf("503 body must not blame the credential: %s", body)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("Retry-After not set on the 503")
	}
}

// A genuinely bad credential must still be 401 even while the store is fine —
// the counterpart, so the 503 path cannot be implemented by answering 503 to
// everything.
func TestBadCredentialIsStill401(t *testing.T) {
	dir := t.TempDir()
	store, err := auth.OpenStore(dir + "/pat.db")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	a := NewAuthenticator(store, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := a.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, bearer := range []string{"pat_aaaaaaaaaaaa_nope", "sk-not-a-key", "garbage"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("bearer %q: status = %d, want 401", bearer, rec.Code)
		}
	}
}
