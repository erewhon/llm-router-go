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
// /v1/images/generations (JSON, generic proxy path)
// ---------------------------------------------------------------------------

func TestImagesGenerations_RoutesToImageGenBackend(t *testing.T) {
	var path string
	var body map[string]any
	up := captureUpstream(t, &path, &body, nil)
	defer up.Close()
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport})

	rec := postTo(t, rt, "/v1/images/generations",
		`{"model":"flux","prompt":"a lighthouse","size":"512x512"}`) // alias of flux-dev
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if path != "/v1/images/generations" {
		t.Errorf("upstream path = %q, want /v1/images/generations", path)
	}
	if body["model"] != "FLUX.1-dev" {
		t.Errorf("upstream model = %v, want bare hf_repo", body["model"])
	}
	if body["prompt"] != "a lighthouse" {
		t.Errorf("prompt not preserved: %v", body["prompt"])
	}
}

// Naming a chat model on the images endpoint is a class mismatch, not a proxy.
func TestImagesGenerations_RejectsChatModel(t *testing.T) {
	up := captureUpstream(t, nil, nil, nil)
	defer up.Close()
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport})

	rec := postTo(t, rt, "/v1/images/generations",
		`{"model":"coder","prompt":"nope"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "api_class") {
		t.Errorf("error should name the api_class mismatch: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// /v1/images/edits (multipart/form-data path)
// ---------------------------------------------------------------------------

// multipartBody builds a form with a model field and a small fake image part.
func multipartBody(t *testing.T, model string) (*bytes.Buffer, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	if model != "" {
		if err := mw.WriteField("model", model); err != nil {
			t.Fatalf("WriteField: %v", err)
		}
	}
	fw, err := mw.CreateFormFile("image", "input.png")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write([]byte("\x89PNG fake bytes")); err != nil {
		t.Fatalf("write image part: %v", err)
	}
	if err := mw.WriteField("prompt", "make it blue"); err != nil {
		t.Fatalf("WriteField prompt: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return buf, mw.FormDataContentType()
}

func postMultipart(t *testing.T, rt *Router, path string, body *bytes.Buffer, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, body)
	req.Header.Set("Content-Type", contentType)
	rt.Handler().ServeHTTP(rec, req)
	return rec
}

func TestImagesEdits_MultipartForwardsVerbatim(t *testing.T) {
	var gotPath, gotCT string
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"aGk="}]}`))
	}))
	defer up.Close()
	rt := newTestRouter(t, &transportRedirect{to: up.URL, rt: http.DefaultTransport})

	body, ct := multipartBody(t, "image-edit") // alias of qwen-image-edit
	sent := body.Bytes()
	rec := postMultipart(t, rt, "/v1/images/edits", body, ct)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/images/edits" {
		t.Errorf("upstream path = %q, want /v1/images/edits", gotPath)
	}
	// The body must pass through untouched — same boundary, same bytes.
	if !bytes.Equal(gotBody, sent) {
		t.Errorf("multipart body was modified in transit (len %d -> %d)", len(sent), len(gotBody))
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data") {
		t.Errorf("upstream content-type = %q", gotCT)
	}
}

func TestImagesEdits_MissingModelIs400(t *testing.T) {
	rt := newTestRouter(t, nil)
	body, ct := multipartBody(t, "")
	rec := postMultipart(t, rt, "/v1/images/edits", body, ct)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestImagesEdits_NonMultipartIs400(t *testing.T) {
	rt := newTestRouter(t, nil)
	rec := postTo(t, rt, "/v1/images/edits", `{"model":"image-edit"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestImagesEdits_UnknownModelIs404(t *testing.T) {
	rt := newTestRouter(t, nil)
	body, ct := multipartBody(t, "no-such-model")
	rec := postMultipart(t, rt, "/v1/images/edits", body, ct)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

func TestImagesEdits_RejectsImageGenModel(t *testing.T) {
	rt := newTestRouter(t, nil)
	body, ct := multipartBody(t, "flux") // image_gen, not image_edit
	rec := postMultipart(t, rt, "/v1/images/edits", body, ct)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "api_class") {
		t.Errorf("error should name the api_class mismatch: %s", rec.Body.String())
	}
}
