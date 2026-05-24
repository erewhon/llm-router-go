package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func runFetch(t *testing.T, urlStr string, client *http.Client) string {
	t.Helper()
	tool := FetchURL(client)
	return tool.Run(context.Background(), `{"url":"`+urlStr+`"}`)
}

func TestFetchURL_HTMLTextExtraction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html>
<html><head><title>t</title><script>var x=1;</script><style>body{color:red}</style></head>
<body>
  <h1>Headline</h1>
  <p>First paragraph with <a href="/x">a link</a> in it.</p>
  <script>alert('boom')</script>
  <p>Second paragraph.</p>
  <noscript>JavaScript required.</noscript>
</body></html>`))
	}))
	defer srv.Close()

	got := runFetch(t, srv.URL, srv.Client())

	for _, want := range []string{"Headline", "First paragraph", "Second paragraph"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
	for _, banned := range []string{"var x=1", "color:red", "JavaScript required", "boom"} {
		if strings.Contains(got, banned) {
			t.Errorf("extracted text leaks %q:\n%s", banned, got)
		}
	}
}

func TestFetchURL_TruncatesLargeBody(t *testing.T) {
	huge := strings.Repeat("Hello world. ", 2000) // > 8000 chars
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(huge))
	}))
	defer srv.Close()

	got := runFetch(t, srv.URL, srv.Client())
	if len(got) <= MaxFetchChars {
		t.Errorf("output not truncated: len=%d, max=%d", len(got), MaxFetchChars)
	}
	if !strings.Contains(got, "[Truncated") {
		t.Errorf("truncation marker missing:\n%s", got[len(got)-200:])
	}
}

func TestFetchURL_RejectsBinaryContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte{0x89, 0x50, 0x4E, 0x47})
	}))
	defer srv.Close()

	got := runFetch(t, srv.URL, srv.Client())
	if !strings.Contains(got, "Non-text content type") {
		t.Errorf("expected Non-text… message, got %q", got)
	}
}

func TestFetchURL_AcceptsJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hello":"world"}`))
	}))
	defer srv.Close()

	got := runFetch(t, srv.URL, srv.Client())
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Errorf("JSON not surfaced: %q", got)
	}
}

func TestFetchURL_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	got := runFetch(t, srv.URL, srv.Client())
	if !strings.Contains(got, "HTTP 403") {
		t.Errorf("expected HTTP 403 in message: %q", got)
	}
}

func TestFetchURL_InvalidScheme(t *testing.T) {
	got := runFetch(t, "ftp://example.com/x", nil)
	if !strings.Contains(got, "must start with http") {
		t.Errorf("expected scheme rejection: %q", got)
	}
}

func TestFetchURL_BadJSONArgs(t *testing.T) {
	tool := FetchURL(nil)
	got := tool.Run(context.Background(), "{not json")
	if !strings.Contains(got, "Invalid arguments") {
		t.Errorf("expected Invalid arguments: %q", got)
	}
}

func TestFetchURL_EmptyURL(t *testing.T) {
	tool := FetchURL(nil)
	got := tool.Run(context.Background(), `{"url":""}`)
	if !strings.Contains(got, "empty url") {
		t.Errorf("expected empty url error: %q", got)
	}
}

func TestFetchURL_UserAgentSentToUpstream(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<p>ok</p>"))
	}))
	defer srv.Close()

	_ = runFetch(t, srv.URL, srv.Client())
	if !strings.Contains(gotUA, "LLMAgent") {
		t.Errorf("UA = %q, want LLMAgent fragment", gotUA)
	}
}

// ---------------------------------------------------------------------------
// HTML extraction details
// ---------------------------------------------------------------------------

func TestExtractText_CollapsesWhitespace(t *testing.T) {
	in := []byte(`<p>One</p>
<p>Two

   words</p>`)
	got := strings.TrimSpace(extractText(in))
	want := "One\nTwo words"
	if got != want {
		t.Errorf("extractText = %q, want %q", got, want)
	}
}

func TestExtractText_CrudeFallbackForBrokenHTML(t *testing.T) {
	// html.Parse is extremely tolerant — even very broken HTML tends to
	// parse. This test just ensures the function doesn't crash and
	// returns *something* sensible from a garbage input.
	got := extractText([]byte(`<weird</nope><div>hello</div`))
	if !strings.Contains(got, "hello") {
		t.Errorf("expected hello in output: %q", got)
	}
}
