package router

import (
	"bytes"
	"io"
	"mime/multipart"
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

// ---------------------------------------------------------------------------
// /v1/audio/transcriptions (multipart in, JSON out, images/edits proxy path)
// ---------------------------------------------------------------------------

// transcriptionBody builds an OpenAI-shaped transcription form: a small fake
// WAV file part, the model field, and a response_format field.
func transcriptionBody(t *testing.T, model string) (*bytes.Buffer, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	fw, err := mw.CreateFormFile("file", "turn.wav")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write([]byte("RIFF fake wav bytes")); err != nil {
		t.Fatalf("write audio part: %v", err)
	}
	if model != "" {
		if err := mw.WriteField("model", model); err != nil {
			t.Fatalf("WriteField model: %v", err)
		}
	}
	if err := mw.WriteField("response_format", "json"); err != nil {
		t.Fatalf("WriteField response_format: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return buf, mw.FormDataContentType()
}

func TestAudioTranscriptions_MultipartForwardsVerbatim(t *testing.T) {
	var gotPath, gotCT string
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":" hello there"}`))
	}))
	defer up.Close()
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport})

	// The model field comes AFTER the file part here, as OpenAI clients send
	// it; the form scan must drain the audio to reach it.
	body, ct := transcriptionBody(t, "stt") // alias of whisper-large-v3-turbo
	sent := body.Bytes()
	rec := postMultipart(t, rt, "/v1/audio/transcriptions", body, ct)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/audio/transcriptions" {
		t.Errorf("upstream path = %q, want /v1/audio/transcriptions", gotPath)
	}
	if !bytes.Equal(gotBody, sent) {
		t.Errorf("multipart body was modified in transit (len %d -> %d)", len(sent), len(gotBody))
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data") {
		t.Errorf("upstream content-type = %q", gotCT)
	}
	if got := rec.Body.String(); got != `{"text":" hello there"}` {
		t.Errorf("transcription body mangled: %q", got)
	}
}

// A TTS model on the transcriptions endpoint is a class mismatch: both are
// "audio", but speech and stt are different classes and different backends.
func TestAudioTranscriptions_RejectsTTSModel(t *testing.T) {
	rt := newTestRouter(t, nil)
	body, ct := transcriptionBody(t, "tts")
	rec := postMultipart(t, rt, "/v1/audio/transcriptions", body, ct)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "api_class") {
		t.Errorf("error should name the api_class mismatch: %s", rec.Body.String())
	}
}

func TestAudioTranscriptions_MissingModelIs400(t *testing.T) {
	rt := newTestRouter(t, nil)
	body, ct := transcriptionBody(t, "")
	rec := postMultipart(t, rt, "/v1/audio/transcriptions", body, ct)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

// A JSON body on the transcriptions endpoint is the wrong encoding, not a
// proxy: the form is where the model lives.
func TestAudioTranscriptions_NonMultipartIs400(t *testing.T) {
	rt := newTestRouter(t, nil)
	rec := postTo(t, rt, "/v1/audio/transcriptions", `{"model":"stt"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}
