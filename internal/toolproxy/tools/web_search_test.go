package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ddgFixture is a stripped-down approximation of what
// html.duckduckgo.com/html/ returns: three result blocks with the
// title/snippet/url anchors, including the /l/?uddg= redirect form.
const ddgFixture = `<!doctype html>
<html><body>
<div class="results">
  <div class="result results_links results_links_deep web-result">
    <h2 class="result__title">
      <a class="result__a" rel="nofollow" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2F&amp;rut=abc">The Go Programming Language</a>
    </h2>
    <a class="result__snippet" rel="nofollow" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2F">Build simple, secure, scalable systems with Go.</a>
    <a class="result__url" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2F">go.dev</a>
  </div>

  <div class="result results_links results_links_deep web-result">
    <h2 class="result__title">
      <a class="result__a" href="https://pkg.go.dev/net/http">net/http package - pkg.go.dev</a>
    </h2>
    <a class="result__snippet" href="https://pkg.go.dev/net/http">Package http provides HTTP client and server implementations.</a>
    <a class="result__url" href="https://pkg.go.dev/net/http">pkg.go.dev/net/http</a>
  </div>

  <div class="result results_links">
    <h2 class="result__title">
      <a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgolang.org%2Fdoc">Documentation - The Go Programming Language</a>
    </h2>
    <a class="result__snippet" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgolang.org%2Fdoc">The Go programming language is an open source project to make programmers more productive.</a>
    <a class="result__url" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgolang.org%2Fdoc">golang.org/doc</a>
  </div>
</div>
</body></html>`

func newSearchProxy(t *testing.T, handler http.HandlerFunc) (string, *http.Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL, srv.Client()
}

func TestWebSearch_ParsesDDGFixture(t *testing.T) {
	endpoint, client := newSearchProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("DDG should be POST, got %s", r.Method)
		}
		_ = r.ParseForm()
		if got := r.Form.Get("q"); got != "go programming language" {
			t.Errorf("query form value = %q", got)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(ddgFixture))
	})

	tool := webSearch(client, endpoint)
	got := tool.Run(context.Background(), `{"query":"go programming language"}`)

	wantSnippets := []string{
		"The Go Programming Language",
		"https://go.dev/",
		"net/http package",
		"https://pkg.go.dev/net/http", // un-redirected literal URL
		"open source project",
	}
	for _, w := range wantSnippets {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q:\n%s", w, got)
		}
	}
	// Make sure the result__url text is used as the display URL when
	// available (e.g. "go.dev" or "pkg.go.dev/net/http"):
	if !strings.Contains(got, "pkg.go.dev/net/http") {
		t.Errorf("display URL didn't reach output:\n%s", got)
	}
}

func TestWebSearch_NoResults(t *testing.T) {
	endpoint, client := newSearchProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><div class="no-results">Nothing found.</div></body></html>`))
	})
	tool := webSearch(client, endpoint)
	got := tool.Run(context.Background(), `{"query":"asdfghjkl"}`)
	if !strings.Contains(got, "No results") {
		t.Errorf("expected No results message: %q", got)
	}
}

func TestWebSearch_HTTPError(t *testing.T) {
	endpoint, client := newSearchProxy(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "ratelimited", http.StatusTooManyRequests)
	})
	tool := webSearch(client, endpoint)
	got := tool.Run(context.Background(), `{"query":"x"}`)
	if !strings.Contains(got, "HTTP 429") {
		t.Errorf("expected HTTP 429: %q", got)
	}
}

func TestWebSearch_EmptyQuery(t *testing.T) {
	tool := WebSearch(nil)
	got := tool.Run(context.Background(), `{"query":""}`)
	if !strings.Contains(got, "empty query") {
		t.Errorf("expected empty query error: %q", got)
	}
}

func TestWebSearch_BadJSONArgs(t *testing.T) {
	tool := WebSearch(nil)
	got := tool.Run(context.Background(), "not json")
	if !strings.Contains(got, "Invalid arguments") {
		t.Errorf("expected Invalid arguments: %q", got)
	}
}

func TestWebSearch_CapsResultCount(t *testing.T) {
	// Build a fixture with 8 results and verify only MaxSearchResults
	// (5) appear.
	var b strings.Builder
	b.WriteString(`<html><body><div class="results">`)
	for i := 0; i < 8; i++ {
		fmtResult(&b, i)
	}
	b.WriteString(`</div></body></html>`)

	endpoint, client := newSearchProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(b.String()))
	})
	tool := webSearch(client, endpoint)
	got := tool.Run(context.Background(), `{"query":"x"}`)

	hits := strings.Count(got, "Title-")
	if hits != MaxSearchResults {
		t.Errorf("rendered %d results, want %d", hits, MaxSearchResults)
	}
	// 6th result must not have leaked in.
	if strings.Contains(got, "Title-5") || strings.Contains(got, "Title-7") {
		t.Errorf("leaked beyond cap:\n%s", got)
	}
}

func fmtResult(b *strings.Builder, i int) {
	href := "https://example.com/x" + string(rune('0'+i))
	b.WriteString(`<div class="result web-result">
  <h2 class="result__title"><a class="result__a" href="` + href + `">Title-` + string(rune('0'+i)) + `</a></h2>
  <a class="result__snippet" href="` + href + `">Snippet-` + string(rune('0'+i)) + `</a>
  <a class="result__url" href="` + href + `">example.com/` + string(rune('0'+i)) + `</a>
</div>`)
}

func TestDecodeRedirect(t *testing.T) {
	cases := map[string]string{
		"//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2F&rut=x": "https://go.dev/",
		"https://duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fa":             "https://example.com/a",
		"https://pkg.go.dev/net/http":                                              "https://pkg.go.dev/net/http", // no /l/? — unchanged
		"":                                                                        "",
		"/l/?uddg=https%3A%2F%2Ffoo.test%2Fpath":                                  "https://foo.test/path",
	}
	for in, want := range cases {
		if got := decodeRedirect(in); got != want {
			t.Errorf("decodeRedirect(%q) = %q, want %q", in, got, want)
		}
	}
}
