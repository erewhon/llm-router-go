package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golang.org/x/net/html"
)

// FetchURLName is the registered tool name.
const FetchURLName = "fetch_url"

// MaxFetchChars caps the body returned to the model. Matches the Python
// tool's MAX_FETCH_CHARS so model context-window assumptions hold.
const MaxFetchChars = 8000

const fetchURLDescription = "Fetch and read the contents of a web page. " +
	"Use this to read articles, documentation, or any URL. " +
	"Returns the main text content of the page."

var fetchURLParameters = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"url": map[string]any{
			"type":        "string",
			"description": "The full URL to fetch (must start with http:// or https://)",
		},
	},
	"required": []string{"url"},
}

// FetchURL returns the Tool definition for the URL-text-extraction tool.
// client is the http.Client used for outbound requests — typically the
// shared SOCKS5-via-VPN client from NewHTTPClient.
func FetchURL(client *http.Client) Tool {
	if client == nil {
		client = http.DefaultClient
	}
	return Tool{
		Name:        FetchURLName,
		Description: fetchURLDescription,
		Parameters:  fetchURLParameters,
		Run: func(ctx context.Context, arguments string) string {
			var args struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal([]byte(arguments), &args); err != nil {
				return "Invalid arguments: " + arguments
			}
			if args.URL == "" {
				return "Invalid arguments: empty url"
			}
			if !strings.HasPrefix(args.URL, "http://") && !strings.HasPrefix(args.URL, "https://") {
				return "Fetch failed: url must start with http:// or https://"
			}
			return doFetch(ctx, client, args.URL)
		},
	}
}

func doFetch(ctx context.Context, client *http.Client, url string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "Fetch failed: " + err.Error()
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; LLMAgent/1.0)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return "Fetch failed: " + err.Error()
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Sprintf("Fetch failed: HTTP %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !textyContentType(ct) {
		return "Non-text content type: " + ct
	}

	// Cap how much of the body we'll parse so a 100MB page can't OOM
	// the proxy. The model only sees MaxFetchChars characters anyway.
	const bodyLimit = 4 * 1024 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if err != nil {
		return "Fetch failed: read body: " + err.Error()
	}

	text := extractText(body)
	text = strings.TrimSpace(text)
	if text == "" {
		return "Could not extract text content from: " + url
	}
	if len(text) > MaxFetchChars {
		return text[:MaxFetchChars] + fmt.Sprintf(
			"\n\n[Truncated — showing first %d of %d characters]",
			MaxFetchChars, len(text),
		)
	}
	return text
}

// textyContentType returns true for HTML/JSON/XML/plain text MIME types.
// The Python tool uses the same substring heuristic.
func textyContentType(ct string) bool {
	c := strings.ToLower(ct)
	return strings.Contains(c, "text") ||
		strings.Contains(c, "json") ||
		strings.Contains(c, "xml")
}

// extractText walks an HTML document collecting text from text nodes,
// skipping <script>/<style>/<noscript> subtrees, and inserting a
// newline after block-level elements so the model sees structure
// rather than a single long line.
func extractText(body []byte) string {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		// Not valid HTML — fall back to crude tag-strip.
		return crudeTagStrip(string(body))
	}
	var buf strings.Builder
	walkHTML(doc, &buf)
	return collapseWhitespace(buf.String())
}

func walkHTML(n *html.Node, buf *strings.Builder) {
	if n.Type == html.ElementNode {
		switch n.Data {
		case "script", "style", "noscript", "template":
			return
		}
	}
	if n.Type == html.TextNode {
		// Source-code newlines inside a text node are inline whitespace
		// in HTML semantics; collapse them to single spaces so block
		// boundaries (added below) are the only source of "\n" in the
		// extracted text.
		buf.WriteString(compressInlineWhitespace(n.Data))
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkHTML(c, buf)
	}
	if n.Type == html.ElementNode && isBlockElement(n.Data) {
		buf.WriteByte('\n')
	}
}

// compressInlineWhitespace collapses runs of whitespace (space, tab,
// CR, LF) in a text node to single spaces, preserving at most one
// leading and one trailing space so that adjacent inline runs don't
// smush together.
func compressInlineWhitespace(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	inSpace := false
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
		default:
			b.WriteRune(r)
			inSpace = false
		}
	}
	return b.String()
}

func isBlockElement(name string) bool {
	switch name {
	case "p", "div", "br", "li", "ul", "ol",
		"h1", "h2", "h3", "h4", "h5", "h6",
		"tr", "td", "th", "thead", "tbody",
		"section", "article", "header", "footer", "nav", "aside",
		"blockquote", "pre", "hr":
		return true
	}
	return false
}

func collapseWhitespace(s string) string {
	// Trim trailing spaces on each line, then collapse runs of blank
	// lines down to a single empty line.
	var out strings.Builder
	var blankRun int
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimRight(line, " \t\r")
		// also collapse runs of inner whitespace
		t = strings.Join(strings.Fields(t), " ")
		if t == "" {
			blankRun++
			if blankRun <= 1 {
				out.WriteByte('\n')
			}
			continue
		}
		blankRun = 0
		out.WriteString(t)
		out.WriteByte('\n')
	}
	return out.String()
}

// crudeTagStrip is the fallback if html.Parse rejects the body — slice
// out script/style blocks and then drop everything between < and >.
func crudeTagStrip(s string) string {
	for _, tag := range []string{"script", "style"} {
		s = stripTagBlock(s, tag)
	}
	var buf strings.Builder
	inTag := false
	for _, r := range s {
		switch r {
		case '<':
			inTag = true
		case '>':
			inTag = false
			buf.WriteByte(' ')
		default:
			if !inTag {
				buf.WriteRune(r)
			}
		}
	}
	return collapseWhitespace(buf.String())
}

func stripTagBlock(s, tag string) string {
	open := "<" + tag
	close := "</" + tag + ">"
	for {
		i := indexFold(s, open)
		if i < 0 {
			return s
		}
		j := indexFold(s[i:], close)
		if j < 0 {
			return s[:i]
		}
		s = s[:i] + s[i+j+len(close):]
	}
}

func indexFold(s, substr string) int {
	return strings.Index(strings.ToLower(s), strings.ToLower(substr))
}
