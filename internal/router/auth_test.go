package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
