package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeBase(t *testing.T) {
	cases := map[string]string{
		"http://h:5397":        "http://h:5397",
		"http://h:5397/":       "http://h:5397",
		"http://h:5397/v1":     "http://h:5397",
		"http://h:5397/v1/":    "http://h:5397",
		"https://x.api.aws/v1": "https://x.api.aws",
	}
	for in, want := range cases {
		if got := normalizeBase(in); got != want {
			t.Errorf("normalizeBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitSentences(t *testing.T) {
	got := splitSentences("Hello there. How are you?  I am fine!\nNew line here")
	want := []string{"Hello there.", "How are you?", "I am fine!", "New line here"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("splitSentences = %v, want %v", got, want)
	}

	// Abbreviations / decimals: a '.' not followed by whitespace is not a break.
	if g := splitSentences("Version 3.14 is ready."); len(g) != 1 || g[0] != "Version 3.14 is ready." {
		t.Errorf("decimal split wrong: %v", g)
	}

	// Whitespace collapse.
	if g := splitSentences("a\n\n  b   c"); strings.Join(g, "|") != "a|b c" {
		t.Errorf("whitespace collapse wrong: %v", g)
	}

	// Long, punctuation-free text is hard-split on spaces under the cap.
	long := strings.Repeat("word ", 200) // ~1000 bytes, no sentence terminator
	for _, c := range splitSentences(long) {
		if len(c) > maxChunkBytes {
			t.Fatalf("chunk exceeds cap: %d bytes", len(c))
		}
	}

	if g := splitSentences("   "); g != nil {
		t.Errorf("blank input should yield nil, got %v", g)
	}
}

func TestSynthSendsBearerWhenKeySet(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("RIFF"))
	}))
	defer srv.Close()

	c := &client{base: srv.URL, voice: "tara", model: "orpheus", apiKey: "sk-test", http: srv.Client()}
	wav, err := c.synth(context.Background(), "hi")
	if err != nil {
		t.Fatalf("synth: %v", err)
	}
	if string(wav) != "RIFF" {
		t.Errorf("wav = %q", wav)
	}
	if gotPath != "/v1/audio/speech" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want Bearer sk-test", gotAuth)
	}

	c.apiKey = ""
	if _, err := c.synth(context.Background(), "hi"); err != nil {
		t.Fatalf("synth without key: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization sent with empty key: %q", gotAuth)
	}
}

func TestSynthUnauthorizedHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := &client{base: srv.URL, http: srv.Client()}
	_, err := c.synth(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "LLM_ROUTER_API_KEY") {
		t.Errorf("want hint naming LLM_ROUTER_API_KEY, got %v", err)
	}
}
