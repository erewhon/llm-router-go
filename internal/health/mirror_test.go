package health

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestMirror(t *testing.T, url string) *Mirror {
	t.Helper()
	return NewMirror(MirrorConfig{
		URL:    url,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestMirrorReadsModelsAndRoles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/availability" {
			t.Errorf("path = %q, want /v1/availability", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{
			"tracking": true,
			"models": [
				{"model":"qwen3.6-hypatia","state":"unavailable"},
				{"model":"minimax-reap","state":"available"},
				{"model":"never-polled","state":"unknown"}
			],
			"roles": [
				{"role":"coder","available":true,"target":"minimax-reap"},
				{"role":"thinker","available":false}
			]
		}`)
	}))
	defer srv.Close()

	m := newTestMirror(t, srv.URL)
	m.FetchOnce(context.Background())

	if !m.Fresh() {
		t.Fatalf("mirror should be fresh after a successful fetch")
	}
	cases := map[string]bool{
		"qwen3.6-hypatia": false,
		"minimax-reap":    true,
		// Unknown state means the router has no evidence against it.
		"never-polled": true,
		"coder":        true,
		"thinker":      false,
		// A name the router didn't mention is not our business to block.
		"something-else": true,
	}
	for name, want := range cases {
		if got := m.Routable(name); got != want {
			t.Errorf("Routable(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestMirrorFailsOpenWhenRouterUnreachable(t *testing.T) {
	// Port 1 is reliably closed. A tool proxy whose status feed is down must
	// keep routing, not refuse everything.
	m := newTestMirror(t, "http://127.0.0.1:1")
	m.FetchOnce(context.Background())

	if m.Fresh() {
		t.Errorf("mirror should not report fresh after a failed fetch")
	}
	for _, name := range []string{"coder", "thinker", "anything"} {
		if !m.Routable(name) {
			t.Errorf("Routable(%q) = false; an unreachable mirror must fail open", name)
		}
	}
}

func TestMirrorFailsOpenBeforeFirstFetch(t *testing.T) {
	m := newTestMirror(t, "http://127.0.0.1:1")
	if !m.Routable("coder") {
		t.Errorf("everything should be routable before the first fetch")
	}
}

func TestMirrorSendsBearer(t *testing.T) {
	// /v1/availability sits behind the router's bearer gate. A mirror without
	// a key 401s on every poll and silently degrades to "everything routable",
	// which looks healthy but makes the whole feature inert — so the token
	// actually reaching the wire is worth asserting.
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if gotAuth != "Bearer sk-test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"tracking":true,"roles":[{"role":"coder","available":false}]}`)
	}))
	defer srv.Close()

	m := NewMirror(MirrorConfig{
		URL:    srv.URL,
		Bearer: "sk-test-key",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	m.FetchOnce(context.Background())

	if gotAuth != "Bearer sk-test-key" {
		t.Fatalf("Authorization = %q, want the bearer to be sent", gotAuth)
	}
	if !m.Fresh() {
		t.Errorf("mirror should be fresh once authenticated")
	}
	if m.Routable("coder") {
		t.Errorf("authenticated fetch should have applied the real state")
	}
}

func TestMirrorGoesStaleOnLaterFailure(t *testing.T) {
	down := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if down {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `{"tracking":true,"roles":[{"role":"coder","available":false}]}`)
	}))
	defer srv.Close()

	m := newTestMirror(t, srv.URL)
	m.FetchOnce(context.Background())
	if m.Routable("coder") {
		t.Fatalf("coder should read as down from a fresh fetch")
	}

	// The router starts erroring: rather than keep enforcing a snapshot that
	// may now be wrong, fall open.
	down = true
	m.FetchOnce(context.Background())
	if !m.Routable("coder") {
		t.Errorf("a stale mirror must fail open rather than enforce old state")
	}
}
