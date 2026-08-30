package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// /v1/audio/speech (JSON in, raw audio out, generic proxy path)
// ---------------------------------------------------------------------------

// audioUpstream answers like Orpheus: a WAV body with a non-JSON content type.
func audioUpstream(t *testing.T, path *string, body *map[string]any) *httptest.Server {
	t.Helper()
	up := captureUpstream(t, path, body, nil)
	inner := up.Config.Handler
	up.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, r) // capture path/body, discard its JSON reply
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("RIFF fake wav bytes"))
	})
	return up
}

func TestAudioSpeech_RoutesToTTSBackend(t *testing.T) {
	var path string
	var body map[string]any
	up := audioUpstream(t, &path, &body)
	defer up.Close()
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport})

	rec := postTo(t, rt, "/v1/audio/speech",
		`{"model":"tts","input":"hello there","voice":"tara"}`) // alias of orpheus-tts
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if path != "/v1/audio/speech" {
		t.Errorf("upstream path = %q, want /v1/audio/speech", path)
	}
	if body["model"] != "orpheus-3b-0.1-ft" {
		t.Errorf("upstream model = %v, want bare hf_repo", body["model"])
	}
	if body["input"] != "hello there" || body["voice"] != "tara" {
		t.Errorf("input/voice not preserved: %v", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Errorf("Content-Type = %q, want audio/wav passed through", ct)
	}
	if got := rec.Body.String(); got != "RIFF fake wav bytes" {
		t.Errorf("audio body mangled: %q", got)
	}
}

// Naming a chat model on the speech endpoint is a class mismatch, not a proxy.
func TestAudioSpeech_RejectsChatModel(t *testing.T) {
	up := audioUpstream(t, nil, nil)
	defer up.Close()
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport})

	rec := postTo(t, rt, "/v1/audio/speech", `{"model":"coder","input":"nope"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "api_class") {
		t.Errorf("error should name the api_class mismatch: %s", rec.Body.String())
	}
}
