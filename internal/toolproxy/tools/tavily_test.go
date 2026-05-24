package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTavilyServer(t *testing.T, handler http.HandlerFunc) (string, *http.Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL, srv.Client()
}

func TestTavily_HappyPath(t *testing.T) {
	var gotBody map[string]any
	endpoint, client := newTavilyServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"answer": "Go was first released in 2009.",
			"results": [
				{"title":"Go - Wikipedia","content":"Go is a statically typed language.","url":"https://en.wikipedia.org/wiki/Go","score":0.92},
				{"title":"The Go Programming Language","content":"Build simple, secure systems.","url":"https://go.dev","score":0.87}
			]
		}`))
	})

	tool := tavily(client, endpoint, "tvly-test-key")
	got := tool.Run(context.Background(), `{"query":"when was Go released?"}`)

	if gotBody["api_key"] != "tvly-test-key" {
		t.Errorf("api_key not forwarded in request body: %v", gotBody)
	}
	if gotBody["query"] != "when was Go released?" {
		t.Errorf("query not forwarded: %v", gotBody)
	}
	if gotBody["search_depth"] != "basic" {
		t.Errorf("search_depth = %v, want basic (default)", gotBody["search_depth"])
	}
	if gotBody["include_answer"] != true {
		t.Errorf("include_answer = %v, want true", gotBody["include_answer"])
	}

	wantFragments := []string{
		"AI Summary: Go was first released in 2009.",
		"- Go - Wikipedia (relevance: 0.92)",
		"Go is a statically typed language.",
		"https://en.wikipedia.org/wiki/Go",
		"- The Go Programming Language (relevance: 0.87)",
		"https://go.dev",
	}
	for _, w := range wantFragments {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q:\n%s", w, got)
		}
	}
}

func TestTavily_AdvancedDepth(t *testing.T) {
	var gotDepth string
	endpoint, client := newTavilyServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotDepth, _ = body["search_depth"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answer":"","results":[]}`))
	})

	tool := tavily(client, endpoint, "k")
	_ = tool.Run(context.Background(), `{"query":"x","search_depth":"advanced"}`)
	if gotDepth != "advanced" {
		t.Errorf("search_depth forwarded = %q, want advanced", gotDepth)
	}
}

func TestTavily_OmitsAnswerWhenEmpty(t *testing.T) {
	endpoint, client := newTavilyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"X","content":"Y","url":"https://x","score":0.5}]}`))
	})
	tool := tavily(client, endpoint, "k")
	got := tool.Run(context.Background(), `{"query":"x"}`)
	if strings.Contains(got, "AI Summary") {
		t.Errorf("AI Summary leaked when answer empty:\n%s", got)
	}
	if !strings.Contains(got, "X (relevance: 0.50)") {
		t.Errorf("expected formatted result row: %q", got)
	}
}

func TestTavily_NoResults(t *testing.T) {
	endpoint, client := newTavilyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answer":"","results":[]}`))
	})
	tool := tavily(client, endpoint, "k")
	got := tool.Run(context.Background(), `{"query":"asdfgh"}`)
	if !strings.Contains(got, "No results found") {
		t.Errorf("expected No results message: %q", got)
	}
}

func TestTavily_AuthError(t *testing.T) {
	endpoint, client := newTavilyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"Invalid API key"}`))
	})
	tool := tavily(client, endpoint, "bad-key")
	got := tool.Run(context.Background(), `{"query":"x"}`)
	if !strings.Contains(got, "HTTP 401") {
		t.Errorf("expected HTTP 401 in error: %q", got)
	}
	if !strings.Contains(got, "Invalid API key") {
		t.Errorf("expected upstream detail surfaced: %q", got)
	}
}

func TestTavily_RateLimitError(t *testing.T) {
	endpoint, client := newTavilyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"Quota exceeded"}`))
	})
	tool := tavily(client, endpoint, "k")
	got := tool.Run(context.Background(), `{"query":"x"}`)
	if !strings.Contains(got, "HTTP 429") || !strings.Contains(got, "Quota exceeded") {
		t.Errorf("expected HTTP 429 + quota message: %q", got)
	}
}

func TestTavily_NonJSONErrorBody(t *testing.T) {
	endpoint, client := newTavilyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`<html>upstream is down</html>`))
	})
	tool := tavily(client, endpoint, "k")
	got := tool.Run(context.Background(), `{"query":"x"}`)
	if !strings.Contains(got, "HTTP 502") {
		t.Errorf("expected HTTP 502: %q", got)
	}
}

func TestTavily_MissingAPIKey(t *testing.T) {
	tool := Tavily(nil, "")
	got := tool.Run(context.Background(), `{"query":"x"}`)
	if !strings.Contains(got, "no API key") {
		t.Errorf("expected no-API-key error: %q", got)
	}
}

func TestTavily_EmptyQuery(t *testing.T) {
	tool := Tavily(nil, "k")
	got := tool.Run(context.Background(), `{"query":""}`)
	if !strings.Contains(got, "empty query") {
		t.Errorf("expected empty-query error: %q", got)
	}
}

func TestTavily_BadJSONArgs(t *testing.T) {
	tool := Tavily(nil, "k")
	got := tool.Run(context.Background(), "not json")
	if !strings.Contains(got, "Invalid arguments") {
		t.Errorf("expected Invalid arguments: %q", got)
	}
}

func TestTavily_InvalidSearchDepth(t *testing.T) {
	tool := Tavily(nil, "k")
	got := tool.Run(context.Background(), `{"query":"x","search_depth":"deeper"}`)
	if !strings.Contains(got, "basic or advanced") {
		t.Errorf("expected search_depth validation error: %q", got)
	}
}
