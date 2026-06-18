package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// WebSearchName is the registered tool name.
const WebSearchName = "web_search"

// MaxSearchResults caps the number of hits returned to the model.
// Matches the Python tool's default.
const MaxSearchResults = 5

const webSearchDescription = "Search the web for current information using DuckDuckGo. " +
	"Use this when you need up-to-date facts, news, or information you don't have."

var webSearchParameters = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"query": map[string]any{
			"type":        "string",
			"description": "The search query",
		},
	},
	"required": []string{"query"},
}

// DDGHTMLEndpoint is the DuckDuckGo HTML-form endpoint. Exposed so
// tests can re-target the search at an httptest server.
const DDGHTMLEndpoint = "https://html.duckduckgo.com/html/"

// WebSearch returns the Tool that calls DuckDuckGo's HTML endpoint
// through the supplied http.Client (typically VPN-routed).
func WebSearch(client *http.Client) Tool {
	return webSearch(client, DDGHTMLEndpoint)
}

// webSearch is the test-injectable form.
func webSearch(client *http.Client, endpoint string) Tool {
	if client == nil {
		client = http.DefaultClient
	}
	return Tool{
		Name:        WebSearchName,
		Description: webSearchDescription,
		Parameters:  webSearchParameters,
		Run: func(ctx context.Context, arguments string) string {
			var args struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal([]byte(arguments), &args); err != nil {
				return "Invalid arguments: " + arguments
			}
			if args.Query == "" {
				return "Invalid arguments: empty query"
			}
			return doSearch(ctx, pickClient(ctx, client), endpoint, args.Query)
		},
	}
}

func doSearch(ctx context.Context, client *http.Client, endpoint, query string) string {
	form := url.Values{}
	form.Set("q", query)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "Search failed: " + err.Error()
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; LLMAgent/1.0)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := client.Do(req)
	if err != nil {
		return "Search failed: " + err.Error()
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Sprintf("Search failed: HTTP %d", resp.StatusCode)
	}

	const bodyLimit = 4 * 1024 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if err != nil {
		return "Search failed: read body: " + err.Error()
	}

	results, err := parseDDGResults(body)
	if err != nil {
		return "Search failed: " + err.Error()
	}
	if len(results) == 0 {
		return "No results found."
	}

	var out strings.Builder
	for i, r := range results {
		if i >= MaxSearchResults {
			break
		}
		fmt.Fprintf(&out, "- %s\n  %s\n  %s", r.Title, r.Snippet, r.URL)
		if i < MaxSearchResults-1 && i < len(results)-1 {
			out.WriteString("\n\n")
		}
	}
	return strings.TrimRight(out.String(), "\n")
}

// SearchResult is a single hit. Exported so external callers (and
// tests) can build the same shape if needed.
type SearchResult struct {
	Title   string
	Snippet string
	URL     string
}

// parseDDGResults extracts results from a DuckDuckGo HTML-form response.
// DDG groups each result inside a container element whose class
// contains "result"; within that, "result__a" carries the title +
// href, "result__snippet" carries the description, and "result__url"
// carries the display URL. DDG sometimes wraps links in a redirect
// ("/l/?uddg=…"); decodeRedirect unwraps that.
func parseDDGResults(body []byte) ([]SearchResult, error) {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}

	// Step 1: find every result container by hunting for the title
	// anchor (class result__a). Each one anchors a result; we then
	// look at siblings in its surrounding result block.
	var titles []*html.Node
	findAll(doc, func(n *html.Node) bool {
		return isElement(n, "a") && hasClass(n, "result__a")
	}, &titles)

	var out []SearchResult
	seen := map[string]bool{}

	for _, a := range titles {
		// Find the enclosing result container — climb until we hit a
		// node whose class contains "result" (not just "result__*").
		container := enclosingResult(a)
		if container == nil {
			container = a.Parent // fallback
		}

		// Look within the container for the snippet + display URL.
		var snippetNode, urlNode *html.Node
		walk(container, func(n *html.Node) {
			if isElement(n, "a") {
				if snippetNode == nil && hasClass(n, "result__snippet") {
					snippetNode = n
				}
				if urlNode == nil && hasClass(n, "result__url") {
					urlNode = n
				}
			}
		})

		href, _ := getAttr(a, "href")
		href = decodeRedirect(href)
		// The result__url link is also wrapped in DDG's /l/?uddg= shim;
		// extract the same way to surface a stable href when result__a
		// is missing or weird.
		if href == "" && urlNode != nil {
			if h, ok := getAttr(urlNode, "href"); ok {
				href = decodeRedirect(h)
			}
		}
		title := strings.TrimSpace(textOf(a))
		snippet := ""
		if snippetNode != nil {
			snippet = strings.TrimSpace(textOf(snippetNode))
		}

		if title == "" || href == "" {
			continue
		}
		key := title + "|" + href
		if seen[key] {
			continue
		}
		seen[key] = true

		out = append(out, SearchResult{
			Title:   title,
			Snippet: snippet,
			URL:     href,
		})
	}
	return out, nil
}

// decodeRedirect unwraps DuckDuckGo's "/l/?uddg=…" redirect links.
// Returns the original input on anything unexpected.
func decodeRedirect(href string) string {
	if href == "" {
		return ""
	}
	// Accept "/l/?...", "//duckduckgo.com/l/?...", "https://duckduckgo.com/l/?..."
	if !strings.Contains(href, "/l/?") {
		return href
	}
	q := href[strings.Index(href, "?")+1:]
	values, err := url.ParseQuery(q)
	if err != nil {
		return href
	}
	if u := values.Get("uddg"); u != "" {
		return u
	}
	return href
}

// ---------------------------------------------------------------------------
// Tiny HTML helpers (no goquery dep)
// ---------------------------------------------------------------------------

func isElement(n *html.Node, name string) bool {
	return n != nil && n.Type == html.ElementNode && n.Data == name
}

func getAttr(n *html.Node, key string) (string, bool) {
	if n == nil {
		return "", false
	}
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func hasClass(n *html.Node, cls string) bool {
	v, ok := getAttr(n, "class")
	if !ok {
		return false
	}
	for _, c := range strings.Fields(v) {
		if c == cls {
			return true
		}
	}
	return false
}

func textOf(n *html.Node) string {
	var buf strings.Builder
	walkHTML(n, &buf)
	return collapseWhitespace(buf.String())
}

func findAll(n *html.Node, pred func(*html.Node) bool, out *[]*html.Node) {
	if pred(n) {
		*out = append(*out, n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		findAll(c, pred, out)
	}
}

func walk(n *html.Node, fn func(*html.Node)) {
	fn(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, fn)
	}
}

// enclosingResult walks up from n to find the nearest ancestor whose
// class contains "result" but isn't merely "result__*" (those are the
// inner pieces, not the container). Returns nil if none is found.
func enclosingResult(n *html.Node) *html.Node {
	for p := n.Parent; p != nil; p = p.Parent {
		v, ok := getAttr(p, "class")
		if !ok {
			continue
		}
		for _, c := range strings.Fields(v) {
			// "result", "results_links", "web-result", etc. — anything
			// containing "result" that isn't a result__inner class.
			if strings.Contains(c, "result") && !strings.HasPrefix(c, "result__") {
				return p
			}
		}
	}
	return nil
}
