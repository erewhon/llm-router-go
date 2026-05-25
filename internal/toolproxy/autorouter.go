package toolproxy

// Phase 2c — auto-router. Classifies an incoming prompt by embedding it and
// comparing against pre-computed category embeddings, then returns a model
// alias. The proxy redirects the request (with the chosen alias) through
// LiteLLM, which resolves the alias to a real backend — the alias may be an
// external model (e.g. claude-opus-4-6) the tool proxy can't route itself.
//
// Port of src/llm_router/tool_proxy/auto_router.py. Three coding-focused
// complexity tiers:
//   - auto:      coder (default) / coder-fim / thinker / research / vision
//   - auto-free: + coder-hard upgrade for hard coding tasks
//   - auto-full: + claude-opus-4-6 for very hard coding tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// AutoTier is one of the three auto-routing model names.
type AutoTier string

const (
	TierAuto AutoTier = "auto"
	TierFree AutoTier = "auto-free"
	TierFull AutoTier = "auto-full"
)

// autoTier reports whether model (after stripping a LiteLLM "openai/" prefix)
// names an auto tier.
func autoTier(model string) (AutoTier, bool) {
	switch t := AutoTier(strings.TrimPrefix(model, "openai/")); t {
	case TierAuto, TierFree, TierFull:
		return t, true
	default:
		return "", false
	}
}

// routeCategory is one classification target. Kept as an ordered slice (not a
// map) so iteration is deterministic and ties break toward the first listed
// category, matching the Python dict's insertion-order iteration.
type routeCategory struct {
	alias string
	desc  string
}

// routeCategories and their descriptions are copied verbatim from the Python
// ROUTE_CATEGORIES so the embeddings — and thus routing decisions — match the
// production classifier.
var routeCategories = []routeCategory{
	{"coder", "Write code, debug, fix bugs, refactor, implement features, " +
		"programming, software development, functions, classes, algorithms"},
	{"coder-fim", "Fill in the middle, code completion, complete this function, " +
		"autocomplete, insert code here, fill the gap, tab completion, " +
		"inline completion, suggestion, predict next tokens"},
	{"thinker", "Explain in depth, analyze, reason about, compare tradeoffs, " +
		"plan architecture, think through, complex analysis, strategy"},
	{"research", "Search the web, find current information, latest news, " +
		"look up, what happened, recent events, fact check"},
	{"vision", "Describe this image, what do you see, screenshot, photo, " +
		"picture, visual, diagram, chart, OCR, read this image"},
}

// Complexity thresholds for tiered upgrades (Python HARD/VERY_HARD_THRESHOLD).
const (
	hardThreshold     = 0.45
	veryHardThreshold = 0.70
)

var complexityKeywords = []string{
	"refactor", "debug", "fix bug", "implement", "migrate",
	"redesign", "optimize", "rewrite", "multi-file", "across files",
	"architecture", "integration", "end-to-end", "full-stack",
	"complex", "large-scale", "entire codebase", "comprehensive",
}

// AutoRouter classifies prompts to a model alias via embedding similarity.
// Category embeddings are computed once (see RunInit) and published atomically;
// until then Classify falls back to "coder".
type AutoRouter struct {
	embedURL      string
	embedModel    string
	client        *http.Client
	logger        *slog.Logger
	activeAliases map[string]bool // nil = every category active

	embeddings atomic.Pointer[map[string][]float64]
}

// NewAutoRouter builds an AutoRouter. client should talk directly to the
// embedding backend (NOT through the web tools' VPN SOCKS5 proxy — the
// embedder is on the LAN). activeAliases, when non-nil, restricts which
// categories are embedded so the router never picks an alias whose model is
// disabled in the current config; nil means all categories.
func NewAutoRouter(embedURL, embedModel string, client *http.Client, logger *slog.Logger, activeAliases map[string]bool) *AutoRouter {
	if client == nil {
		client = http.DefaultClient
	}
	if logger == nil {
		logger = slog.Default()
	}
	if embedModel == "" {
		embedModel = "qwen3-embedding-4b"
	}
	return &AutoRouter{
		embedURL:      embedURL,
		embedModel:    embedModel,
		client:        client,
		logger:        logger,
		activeAliases: activeAliases,
	}
}

// Ready reports whether category embeddings have been computed.
func (ar *AutoRouter) Ready() bool {
	e := ar.embeddings.Load()
	return e != nil && len(*e) > 0
}

// Initialize computes and publishes the category embeddings. Categories whose
// alias isn't in activeAliases are skipped. Returns an error only if nothing
// could be embedded (so RunInit knows to retry); a partial success is
// published and returns nil, matching the Python proxy's keep-what-worked
// behaviour.
func (ar *AutoRouter) Initialize(ctx context.Context) error {
	embeddings := map[string][]float64{}
	var firstErr error
	for _, cat := range routeCategories {
		if ar.activeAliases != nil && !ar.activeAliases[cat.alias] {
			ar.logger.Info("auto-router: skipping disabled category", "alias", cat.alias)
			continue
		}
		emb, err := ar.getEmbedding(ctx, cat.desc)
		if err != nil {
			ar.logger.Warn("auto-router: category embed failed", "alias", cat.alias, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		embeddings[cat.alias] = emb
		ar.logger.Info("auto-router: category embedded", "alias", cat.alias, "dims", len(emb))
	}
	if len(embeddings) == 0 {
		if firstErr != nil {
			return fmt.Errorf("auto-router: no categories embedded: %w", firstErr)
		}
		return fmt.Errorf("auto-router: no active categories to embed")
	}
	ar.embeddings.Store(&embeddings)
	ar.logger.Info("auto-router ready", "categories", len(embeddings),
		"hard", hardThreshold, "very_hard", veryHardThreshold)
	return nil
}

// RunInit retries Initialize with exponential backoff until it succeeds or ctx
// is cancelled. Meant to run in a background goroutine so a slow or unreachable
// embedding backend never blocks startup — auto routes fall back to "coder"
// until the first success.
func (ar *AutoRouter) RunInit(ctx context.Context) {
	const maxBackoff = 60 * time.Second
	backoff := 2 * time.Second
	for {
		if err := ar.Initialize(ctx); err == nil {
			return
		} else {
			ar.logger.Warn("auto-router init failed; will retry",
				"err", err, "retry_in", backoff.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// Classify returns the model alias a prompt should route to. Falls back to
// "coder" whenever it can't do better (not initialized, no user message,
// embedding failure).
func (ar *AutoRouter) Classify(ctx context.Context, messages []any, tier AutoTier) string {
	embPtr := ar.embeddings.Load()
	if embPtr == nil || len(*embPtr) == 0 {
		ar.logger.WarnContext(ctx, "auto-router not initialized; defaulting to coder")
		return "coder"
	}
	embeddings := *embPtr

	userMsg, hasImage := lastUserMessage(messages)
	if hasImage {
		ar.logger.InfoContext(ctx, "auto-route: image detected -> vision")
		return "vision"
	}
	if userMsg == "" {
		return "coder"
	}

	// Only the intent matters for classification, not the full context.
	classifyText := truncateRunes(userMsg, 500)
	promptEmb, err := ar.getEmbedding(ctx, classifyText)
	if err != nil {
		ar.logger.WarnContext(ctx, "auto-route embed failed; defaulting to coder", "err", err)
		return "coder"
	}

	bestAlias, bestScore := "coder", -1.0
	scores := make(map[string]float64, len(embeddings))
	for _, cat := range routeCategories { // deterministic order / tie-break
		catEmb, ok := embeddings[cat.alias]
		if !ok {
			continue
		}
		s := cosineSimilarity(promptEmb, catEmb)
		scores[cat.alias] = math.Round(s*1000) / 1000
		if s > bestScore {
			bestScore, bestAlias = s, cat.alias
		}
	}

	// Complexity upgrades apply to coding tasks only.
	if bestAlias == "coder" && (tier == TierFree || tier == TierFull) {
		complexity := scoreComplexity(userMsg)
		if tier == TierFull && complexity >= veryHardThreshold {
			ar.logDecision(ctx, tier, classifyText, bestAlias, "claude-opus-4-6", scores, &complexity)
			return "claude-opus-4-6"
		}
		if complexity >= hardThreshold {
			ar.logDecision(ctx, tier, classifyText, bestAlias, "coder-hard", scores, &complexity)
			return "coder-hard"
		}
	}

	ar.logDecision(ctx, tier, classifyText, bestAlias, bestAlias, scores, nil)
	return bestAlias
}

func (ar *AutoRouter) logDecision(ctx context.Context, tier AutoTier, prompt, base, final string, scores map[string]float64, complexity *float64) {
	attrs := []any{
		"tier", string(tier),
		"base", base,
		"final", final,
		"prompt", truncateRunes(prompt, 200),
		"scores", scores,
	}
	if complexity != nil {
		attrs = append(attrs, "complexity", math.Round(*complexity*1000)/1000)
	}
	ar.logger.InfoContext(ctx, "auto-route decision", attrs...)
}

// getEmbedding requests a single embedding from the OpenAI-compatible
// embeddings endpoint. Single-input only by design: the OpenArc backend's
// batched path is broken (see project notes), and the Python proxy embeds one
// string at a time too.
func (ar *AutoRouter) getEmbedding(ctx context.Context, text string) ([]float64, error) {
	reqBody, err := json.Marshal(map[string]any{"model": ar.embedModel, "input": text})
	if err != nil {
		return nil, err
	}
	url := strings.TrimSuffix(ar.embedURL, "/") + "/v1/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := ar.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBackendBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body := string(raw)
		if len(body) > 200 {
			body = body[:200] + "…"
		}
		return nil, fmt.Errorf("embeddings HTTP %d: %s", resp.StatusCode, body)
	}

	var out struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode embeddings response: %w", err)
	}
	if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embeddings response had no vector")
	}
	return out.Data[0].Embedding, nil
}

// ---------------------------------------------------------------------------
// Pure helpers (no I/O)
// ---------------------------------------------------------------------------

// lastUserMessage returns the text of the most recent user message. If that
// message is multimodal and contains an image part, hasImage is true (the
// caller routes straight to vision); otherwise text is the concatenated/last
// text part (or the plain string content).
func lastUserMessage(messages []any) (text string, hasImage bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		m, ok := messages[i].(map[string]any)
		if !ok || m["role"] != "user" {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			return c, false
		case []any:
			for _, part := range c {
				p, ok := part.(map[string]any)
				if !ok {
					continue
				}
				switch p["type"] {
				case "image_url", "image":
					return "", true
				case "text":
					if t, ok := p["text"].(string); ok {
						text = t
					}
				}
			}
		}
		return text, false // first user message from the end decides
	}
	return "", false
}

// cosineSimilarity matches the Python helper: the dot product runs over the
// shorter length (zip strict=False) while each norm uses its full vector.
func cosineSimilarity(a, b []float64) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot float64
	for i := 0; i < n; i++ {
		dot += a[i] * b[i]
	}
	var normA, normB float64
	for _, x := range a {
		normA += x * x
	}
	for _, y := range b {
		normB += y * y
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// scoreComplexity scores a prompt 0..1 on length, complexity keywords, code
// blocks, and multiple source-file references. Port of _score_complexity.
func scoreComplexity(text string) float64 {
	score := 0.0
	lower := strings.ToLower(text)

	switch {
	case len(text) > 500:
		score += 0.3
	case len(text) > 200:
		score += 0.15
	}

	matches := 0
	for _, kw := range complexityKeywords {
		if strings.Contains(lower, kw) {
			matches++
		}
	}
	score += math.Min(float64(matches)*0.15, 0.4)

	if strings.Contains(text, "```") {
		score += 0.2
	}

	fileRefs := 0
	for _, ext := range []string{".py", ".ts", ".js", ".go", ".rs", ".java", ".tsx", ".jsx"} {
		fileRefs += strings.Count(lower, ext)
	}
	if fileRefs >= 2 {
		score += 0.2
	}

	return math.Min(score, 1.0)
}

// truncateRunes returns at most n runes of s, never splitting a multi-byte
// rune (Python slices by character, so we match that rather than byte-slicing).
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
