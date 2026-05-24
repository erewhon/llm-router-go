package toolproxy

import (
	"encoding/json"
	"net/http"
	"time"
)

// ---------------------------------------------------------------------------
// OpenAI-compatible response shapes the proxy builds itself.
//
// The tool loop never forwards a backend chat-completion verbatim — it rebuilds
// the response so the proxy owns the id and can attach proxy-side reasoning /
// tool_calls. The final streamed answer is the exception: it's relayed straight
// from the backend by the reverse proxy (see proxy.go), not built here.
// ---------------------------------------------------------------------------

type chatCompletionOut struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Created int64           `json:"created"`
	Model   string          `json:"model"`
	Choices []chatChoiceOut `json:"choices"`
	Usage   *usage          `json:"usage,omitempty"`
}

type chatChoiceOut struct {
	Index        int        `json:"index"`
	Message      messageOut `json:"message"`
	FinishReason string     `json:"finish_reason"`
}

type messageOut struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCall `json:"tool_calls,omitempty"`
}

// newChatCompletion assembles a non-streaming chat.completion object.
func newChatCompletion(id, model, content, reasoning string, toolCalls []toolCall, u *usage, finishReason string) chatCompletionOut {
	return chatCompletionOut{
		ID:      id,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []chatChoiceOut{{
			Index: 0,
			Message: messageOut{
				Role:             "assistant",
				Content:          content,
				ReasoningContent: reasoning,
				ToolCalls:        toolCalls,
			},
			FinishReason: finishReason,
		}},
		Usage: u,
	}
}

// writeChatCompletionJSON writes a non-streaming chat.completion to w.
func writeChatCompletionJSON(w http.ResponseWriter, cc chatCompletionOut) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cc)
}

// writeJSONError writes an OpenAI-shaped error envelope with the given status.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg},
	})
}

// ---------------------------------------------------------------------------
// SSE chunk builder (chat.completion.chunk)
// ---------------------------------------------------------------------------

type sseChunk struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []sseChoice `json:"choices"`
}

type sseChoice struct {
	Index        int            `json:"index"`
	Delta        map[string]any `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

// sseFields are the optional pieces of a streamed delta. An empty string means
// "omit this key" — the proxy never needs to emit an explicit empty delta.
type sseFields struct {
	role         string
	content      string
	reasoning    string
	finishReason string
}

// buildSSEChunk renders one chat.completion.chunk SSE event (terminated with
// the blank line SSE requires). finish_reason is always present, null until a
// terminal chunk sets it — matching the Python proxy's build_sse_chunk.
func buildSSEChunk(id, model string, f sseFields) string {
	delta := map[string]any{}
	if f.role != "" {
		delta["role"] = f.role
	}
	if f.content != "" {
		delta["content"] = f.content
	}
	if f.reasoning != "" {
		delta["reasoning_content"] = f.reasoning
	}
	var fr *string
	if f.finishReason != "" {
		fr = &f.finishReason
	}
	chunk := sseChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []sseChoice{{Index: 0, Delta: delta, FinishReason: fr}},
	}
	b, _ := json.Marshal(chunk)
	return "data: " + string(b) + "\n\n"
}

const sseDone = "data: [DONE]\n\n"
