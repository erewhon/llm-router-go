package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// TavilyName is the registered tool name.
const TavilyName = "tavily_search"

// TavilyEndpoint is exposed so tests can re-target at an httptest server.
const TavilyEndpoint = "https://api.tavily.com/search"

const tavilyDescription = "Search the web using Tavily API, which is designed for AI agents " +
	"and returns clean, relevant excerpts with an optional AI-generated summary. " +
	"Use this for high-quality search results. Costs API credits."

var tavilyParameters = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"query": map[string]any{
			"type":        "string",
			"description": "The search query",
		},
		"search_depth": map[string]any{
			"type":        "string",
			"enum":        []string{"basic", "advanced"},
			"description": "Search depth: 'basic' (faster, 1 credit) or 'advanced' (more thorough, 2 credits)",
		},
	},
	"required": []string{"query"},
}

// Tavily returns the Tavily-backed search tool. apiKey must be non-empty;
// callers typically check `os.Getenv("TAVILY_API_KEY")` before registering
// so the model only sees the tool when it's actually usable.
func Tavily(client *http.Client, apiKey string) Tool {
	return tavily(client, TavilyEndpoint, apiKey)
}

// tavily is the test-injectable form.
func tavily(client *http.Client, endpoint, apiKey string) Tool {
	if client == nil {
		client = http.DefaultClient
	}
	return Tool{
		Name:        TavilyName,
		Description: tavilyDescription,
		Parameters:  tavilyParameters,
		Run: func(ctx context.Context, arguments string) string {
			var args struct {
				Query       string `json:"query"`
				SearchDepth string `json:"search_depth"`
			}
			if err := json.Unmarshal([]byte(arguments), &args); err != nil {
				return "Invalid arguments: " + arguments
			}
			if args.Query == "" {
				return "Invalid arguments: empty query"
			}
			if apiKey == "" {
				return "Tavily search failed: no API key configured"
			}
			depth := args.SearchDepth
			if depth == "" {
				depth = "basic"
			}
			if depth != "basic" && depth != "advanced" {
				return "Invalid arguments: search_depth must be basic or advanced"
			}
			return doTavily(ctx, pickClient(ctx, client), endpoint, apiKey, args.Query, depth)
		},
	}
}

type tavilyResult struct {
	Title   string  `json:"title"`
	Content string  `json:"content"`
	URL     string  `json:"url"`
	Score   float64 `json:"score"`
}

type tavilyResponse struct {
	Answer  string         `json:"answer"`
	Results []tavilyResult `json:"results"`
}

func doTavily(ctx context.Context, client *http.Client, endpoint, apiKey, query, depth string) string {
	reqBody, _ := json.Marshal(map[string]any{
		"api_key":        apiKey,
		"query":          query,
		"search_depth":   depth,
		"include_answer": true,
		"max_results":    5,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return "Tavily search failed: " + err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "Tavily search failed: " + err.Error()
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return "Tavily search failed: read body: " + err.Error()
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Surface the upstream message when present — Tavily returns
		// JSON errors with a "detail" field on auth/rate-limit failures.
		return fmt.Sprintf("Tavily search failed: HTTP %d: %s",
			resp.StatusCode, tavilyErrorMessage(body))
	}

	var data tavilyResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return "Tavily search failed: parse response: " + err.Error()
	}

	var out strings.Builder
	if data.Answer != "" {
		out.WriteString("AI Summary: ")
		out.WriteString(data.Answer)
		out.WriteString("\n")
	}
	for _, r := range data.Results {
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		fmt.Fprintf(&out, "- %s (relevance: %.2f)\n  %s\n  %s",
			r.Title, r.Score, r.Content, r.URL)
	}
	if out.Len() == 0 {
		return "No results found."
	}
	return strings.TrimRight(out.String(), "\n")
}

// tavilyErrorMessage tries to pull a useful error string out of a non-2xx
// Tavily response body, falling back to the raw body if it doesn't parse.
func tavilyErrorMessage(body []byte) string {
	var probe struct {
		Detail  string `json:"detail"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(body, &probe); err == nil {
		for _, s := range []string{probe.Detail, probe.Message, probe.Error} {
			if s != "" {
				return s
			}
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
