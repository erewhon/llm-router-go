package toolproxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxBackendBody caps how much of a non-streaming backend response we read
// into memory before giving up — a misbehaving upstream can't OOM the proxy.
const maxBackendBody = 64 * 1024 * 1024

// ---------------------------------------------------------------------------
// Chat-completion wire types
//
// These mirror the subset of the OpenAI chat-completions response the tool
// loop needs to inspect. Unknown fields are ignored on decode; we never
// re-serialise a backend response verbatim (responses are rebuilt by
// response.go), so a partial view is fine.
// ---------------------------------------------------------------------------

type chatCompletion struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *usage       `json:"usage"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// SGLang/vLLM with a --reasoning-parser put thinking here. Both spellings
	// appear in the wild (SGLang: reasoning_content; some builds: reasoning),
	// so we read both and prefer `reasoning` to match the Python tool proxy's
	// `getattr(msg, "reasoning", None) or getattr(msg, "reasoning_content", None)`.
	ReasoningContent string     `json:"reasoning_content"`
	Reasoning        string     `json:"reasoning"`
	ToolCalls        []toolCall `json:"tool_calls"`
}

// reasoningText returns the backend-provided reasoning, preferring the
// `reasoning` field over `reasoning_content` (matching the Python proxy).
func (m chatMessage) reasoningText() string {
	if m.Reasoning != "" {
		return m.Reasoning
	}
	return m.ReasoningContent
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolCallFunc `json:"function"`
}

type toolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ---------------------------------------------------------------------------
// Backend client
// ---------------------------------------------------------------------------

// backendError carries a non-2xx upstream status + body so the handler can
// log it and surface a 502, mirroring the Python proxy treating any backend
// failure (raised by the OpenAI client) as a 502.
type backendError struct {
	status int
	body   string
}

func (e *backendError) Error() string {
	body := e.body
	if len(body) > 500 {
		body = body[:500] + "…"
	}
	return fmt.Sprintf("backend returned HTTP %d: %s", e.status, body)
}

// backendComplete makes a single non-streaming POST to the backend's
// /v1/chat/completions and decodes the response. The tool loop drives the
// conversation with these blocking calls; the final answer is streamed
// separately (see loop.go). ctx ties the call to the inbound request so a
// client disconnect cancels in-flight backend work.
func (p *Proxy) backendComplete(ctx context.Context, backendURL string, body []byte) (*chatCompletion, error) {
	url := strings.TrimSuffix(backendURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("toolproxy: build backend request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.backendHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("toolproxy: backend request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBackendBody))
	if err != nil {
		return nil, fmt.Errorf("toolproxy: read backend response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &backendError{status: resp.StatusCode, body: string(raw)}
	}

	var cc chatCompletion
	if err := json.Unmarshal(raw, &cc); err != nil {
		return nil, fmt.Errorf("toolproxy: decode backend response: %w", err)
	}
	return &cc, nil
}

// randHex returns n hex characters from crypto/rand, used for completion and
// tool-call IDs. n is small (≤16) so the odd allocation doesn't matter.
func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is effectively impossible on Linux; a fixed
		// fallback keeps IDs syntactically valid rather than panicking.
		return strings.Repeat("0", n)
	}
	return hex.EncodeToString(b)[:n]
}
