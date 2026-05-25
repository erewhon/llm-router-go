package toolproxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erewhon/llm-router-go/internal/config"
)

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

func TestAutoTier(t *testing.T) {
	cases := map[string]bool{
		"auto": true, "auto-free": true, "auto-full": true,
		"openai/auto": true, "openai/auto-full": true,
		"coder": false, "auto-x": false, "": false,
	}
	for in, want := range cases {
		if _, ok := autoTier(in); ok != want {
			t.Errorf("autoTier(%q) ok = %v, want %v", in, ok, want)
		}
	}
	if tier, _ := autoTier("openai/auto-free"); tier != TierFree {
		t.Errorf("openai/auto-free -> %q, want auto-free", tier)
	}
}

func TestCosineSimilarity(t *testing.T) {
	if got := cosineSimilarity([]float64{1, 0, 0}, []float64{1, 0, 0}); math.Abs(got-1.0) > 1e-9 {
		t.Errorf("identical = %v, want 1.0", got)
	}
	if got := cosineSimilarity([]float64{1, 0}, []float64{0, 1}); math.Abs(got) > 1e-9 {
		t.Errorf("orthogonal = %v, want 0.0", got)
	}
	if got := cosineSimilarity([]float64{0, 0}, []float64{1, 1}); got != 0 {
		t.Errorf("zero vector = %v, want 0.0", got)
	}
	if got := cosineSimilarity([]float64{1, 2, 3}, []float64{2, 4, 6}); math.Abs(got-1.0) > 1e-9 {
		t.Errorf("parallel = %v, want 1.0", got)
	}
}

func TestScoreComplexity(t *testing.T) {
	if s := scoreComplexity("hi"); s != 0 {
		t.Errorf("trivial prompt scored %v, want 0", s)
	}
	complex := "Please refactor and optimize this code. " +
		strings.Repeat("It spans multiple modules and needs careful work. ", 12) +
		"See main.go and util.go. ```go\nfunc x(){}\n```"
	if s := scoreComplexity(complex); s < veryHardThreshold {
		t.Errorf("complex prompt scored %v, want >= %v", s, veryHardThreshold)
	}
	if s := scoreComplexity("optimize this"); s < hardThreshold-0.3 { // 1 keyword only
		t.Errorf("single-keyword scored %v unexpectedly low", s)
	}
}

func TestLastUserMessage(t *testing.T) {
	// Plain string, last user wins.
	msgs := []any{
		map[string]any{"role": "user", "content": "first"},
		map[string]any{"role": "assistant", "content": "reply"},
		map[string]any{"role": "user", "content": "second"},
	}
	if got, img := lastUserMessage(msgs); got != "second" || img {
		t.Errorf("got (%q,%v), want (second,false)", got, img)
	}
	// Multimodal with an image part → hasImage.
	img := []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "what is this"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "x"}},
	}}}
	if _, hasImg := lastUserMessage(img); !hasImg {
		t.Error("image part not detected")
	}
	// Multimodal text only.
	txt := []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "hello there"},
	}}}
	if got, hasImg := lastUserMessage(txt); got != "hello there" || hasImg {
		t.Errorf("got (%q,%v), want (hello there,false)", got, hasImg)
	}
	// No user message.
	if got, _ := lastUserMessage([]any{map[string]any{"role": "system", "content": "x"}}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("hello", 10); got != "hello" {
		t.Errorf("short string changed: %q", got)
	}
	if got := truncateRunes("hello world", 5); got != "hello" {
		t.Errorf("got %q, want hello", got)
	}
	// Multi-byte runes must not be split.
	s := "héllo wörld" // contains 2-byte runes
	got := truncateRunes(s, 5)
	if !isValidUTF8(got) {
		t.Errorf("truncation split a rune: %q", got)
	}
	if len([]rune(got)) != 5 {
		t.Errorf("got %d runes, want 5", len([]rune(got)))
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Classify with a fake embedding backend
//
// embedVec maps text → a 5-dim basis vector by substring rules; the category
// descriptions and the test prompts both run through it, so similarity is
// deterministic. Rule order matters: more specific categories are checked
// before "code" (the coder description also contains "code").
// ---------------------------------------------------------------------------

func embedVec(text string) []float64 {
	l := strings.ToLower(text)
	switch {
	case strings.Contains(l, "image") || strings.Contains(l, "screenshot") || strings.Contains(l, "photo"):
		return []float64{0, 0, 0, 0, 1} // vision
	case strings.Contains(l, "fill in the middle") || strings.Contains(l, "completion") || strings.Contains(l, "autocomplete"):
		return []float64{0, 1, 0, 0, 0} // coder-fim
	case strings.Contains(l, "explain") || strings.Contains(l, "analyze") || strings.Contains(l, "tradeoffs"):
		return []float64{0, 0, 1, 0, 0} // thinker
	case strings.Contains(l, "search") || strings.Contains(l, "news") || strings.Contains(l, "current information"):
		return []float64{0, 0, 0, 1, 0} // research
	default:
		return []float64{1, 0, 0, 0, 0} // coder (default)
	}
}

func newFakeEmbedServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": embedVec(req.Input)}},
		})
	}))
}

func newTestAutoRouter(t *testing.T, embedURL string, active map[string]bool) *AutoRouter {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ar := NewAutoRouter(embedURL, "test-embed", http.DefaultClient, logger, active)
	if err := ar.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return ar
}

func userMessages(content string) []any {
	return []any{map[string]any{"role": "user", "content": content}}
}

// complexCoderPrompt routes to coder (contains "code", no higher-priority
// keyword) and scores well above veryHardThreshold.
const complexCoderPrompt = "Please refactor and optimize this code. " +
	"It spans multiple modules across the project and needs careful, methodical work to get right. " +
	"It spans multiple modules across the project and needs careful, methodical work to get right. " +
	"It spans multiple modules across the project and needs careful, methodical work to get right. " +
	"It spans multiple modules across the project and needs careful, methodical work to get right. " +
	"See main.go and util.go. ```go\nfunc x(){}\n```"

func TestAutoRouter_ClassifyBaseCategories(t *testing.T) {
	srv := newFakeEmbedServer(t)
	defer srv.Close()
	ar := newTestAutoRouter(t, srv.URL, nil)

	cases := map[string]string{
		"please write some code to sort a list": "coder",
		"explain the tradeoffs between A and B": "thinker",
		"search the web for the latest news":    "research",
		"autocomplete this for me":              "coder-fim",
	}
	for prompt, want := range cases {
		if got := ar.Classify(context.Background(), userMessages(prompt), TierAuto); got != want {
			t.Errorf("Classify(%q) = %q, want %q", prompt, got, want)
		}
	}
}

func TestAutoRouter_VisionByImageShortCircuits(t *testing.T) {
	srv := newFakeEmbedServer(t)
	defer srv.Close()
	ar := newTestAutoRouter(t, srv.URL, nil)

	msgs := []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "x"}},
	}}}
	if got := ar.Classify(context.Background(), msgs, TierAuto); got != "vision" {
		t.Errorf("image prompt -> %q, want vision", got)
	}
}

func TestAutoRouter_ComplexityUpgrades(t *testing.T) {
	srv := newFakeEmbedServer(t)
	defer srv.Close()
	ar := newTestAutoRouter(t, srv.URL, nil)
	ctx := context.Background()
	msgs := userMessages(complexCoderPrompt)

	if got := ar.Classify(ctx, msgs, TierAuto); got != "coder" {
		t.Errorf("auto tier upgraded a complex prompt to %q, want coder (no upgrade)", got)
	}
	if got := ar.Classify(ctx, msgs, TierFree); got != "coder-hard" {
		t.Errorf("auto-free -> %q, want coder-hard", got)
	}
	if got := ar.Classify(ctx, msgs, TierFull); got != "claude-opus-4-6" {
		t.Errorf("auto-full -> %q, want claude-opus-4-6", got)
	}

	// A simple coder prompt must NOT upgrade even on the higher tiers.
	simple := userMessages("write code to add two numbers")
	if got := ar.Classify(ctx, simple, TierFull); got != "coder" {
		t.Errorf("simple coder prompt on auto-full -> %q, want coder", got)
	}
}

func TestAutoRouter_NotInitializedDefaultsCoder(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ar := NewAutoRouter("http://127.0.0.1:1", "m", http.DefaultClient, logger, nil)
	// No Initialize() — embeddings unpublished.
	if ar.Ready() {
		t.Error("Ready() true before init")
	}
	if got := ar.Classify(context.Background(), userMessages("anything"), TierAuto); got != "coder" {
		t.Errorf("uninitialized Classify -> %q, want coder", got)
	}
}

func TestAutoRouter_DisabledCategorySkipped(t *testing.T) {
	srv := newFakeEmbedServer(t)
	defer srv.Close()
	// research excluded from the active set.
	active := map[string]bool{"coder": true, "coder-fim": true, "thinker": true, "vision": true}
	ar := newTestAutoRouter(t, srv.URL, active)

	if _, ok := (*ar.embeddings.Load())["research"]; ok {
		t.Error("research category was embedded despite being disabled")
	}
	// A research-y prompt can't match the missing category, so it falls back.
	if got := ar.Classify(context.Background(), userMessages("search the web for news"), TierAuto); got == "research" {
		t.Error("router selected the disabled research alias")
	}
}

func TestAutoRouter_InitFailsWhenEmbedderDown(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ar := NewAutoRouter("http://127.0.0.1:1", "m", http.DefaultClient, logger, nil)
	if err := ar.Initialize(context.Background()); err == nil {
		t.Error("Initialize returned nil with the embedder down")
	}
}

// ---------------------------------------------------------------------------
// End-to-end: auto request through the proxy redirects to LiteLLM
// ---------------------------------------------------------------------------

func TestProxy_AutoRouteRedirectsToLiteLLM(t *testing.T) {
	embed := newFakeEmbedServer(t)
	defer embed.Close()

	var gotModel, gotAuth, gotPath string
	litellm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		gotModel, _ = b["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer litellm.Close()

	reg, err := config.LoadBytes([]byte(testYAML))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ar := newTestAutoRouter(t, embed.URL, nil)
	p := New(reg, logger, WithFlushInterval(0), WithAutoRouter(ar), WithLiteLLM(litellm.URL, "test-key"))

	rec := postChat(t, p, `{"model":"auto","messages":[{"role":"user","content":"please write some code"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotModel != "coder" {
		t.Errorf("LiteLLM received model %q, want coder", gotModel)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", gotAuth)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("LiteLLM response not relayed: %s", rec.Body.String())
	}
}

func TestProxy_AutoWithoutRouterFallsThrough(t *testing.T) {
	// No auto-router configured: "auto" isn't in testYAML, so it 404s rather
	// than being intercepted.
	p := newTestProxy(t, nil)
	rec := postChat(t, p, `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
