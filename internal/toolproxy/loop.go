package toolproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// maxRoundsMessage is the placeholder content returned when the tool loop
// exhausts maxToolRounds without the model producing a tool-free answer.
const maxRoundsMessage = "(max tool rounds reached)"

// loopOutcome is why the tool loop stopped.
type loopOutcome int

const (
	outcomeFinal       loopOutcome = iota // model answered with no tool calls
	outcomeClientCalls                    // model called a tool the proxy doesn't own
	outcomeMaxRounds                      // hit maxToolRounds still calling proxy tools
)

// loopResult is what runLoop hands back to the streaming / non-streaming
// drivers. content has had <tool_call> tags stripped; reasoning is the
// backend-provided thinking (if any). messages is the accumulated conversation
// including executed-tool turns — the streaming driver re-streams the final
// answer from it.
type loopResult struct {
	outcome   loopOutcome
	content   string
	reasoning string
	toolCalls []toolCall // populated for outcomeClientCalls (all calls, proxy + client)
	usage     *usage
	messages  []any
}

// runLoop drives the tool-execution conversation. Each round calls the backend
// non-streaming with the injected tools; proxy-owned tool calls are executed
// and fed back as `tool` messages, then the loop repeats. It stops when the
// model answers without tools (outcomeFinal), calls a tool the proxy doesn't
// own (outcomeClientCalls — handed back for the client to run), or runs out of
// rounds (outcomeMaxRounds). Port of the Python proxy's tool loop
// (_non_streaming_chat_completion / the loop half of _stream_chat_completion).
func (p *Proxy) runLoop(ctx context.Context, backendURL string, bodyMap map[string]any, messages []any, allTools []any, toolChoice any) (loopResult, error) {
	var lastContent, lastReasoning string

	for round := 0; round < p.maxToolRounds; round++ {
		bodyMap["messages"] = messages
		bodyMap["tools"] = allTools
		bodyMap["tool_choice"] = toolChoice
		bodyMap["stream"] = false

		body, err := json.Marshal(bodyMap)
		if err != nil {
			return loopResult{}, fmt.Errorf("toolproxy: marshal loop body: %w", err)
		}
		cc, err := p.backendComplete(ctx, backendURL, body)
		if err != nil {
			return loopResult{}, err
		}
		if len(cc.Choices) == 0 {
			return loopResult{}, fmt.Errorf("toolproxy: backend returned no choices")
		}

		msg := cc.Choices[0].Message
		content := stripToolCallTags(msg.Content)
		reasoning := msg.reasoningText()
		lastContent, lastReasoning = content, reasoning

		calls := extractToolCalls(msg)
		if len(calls) == 0 {
			return loopResult{
				outcome: outcomeFinal, content: content, reasoning: reasoning,
				usage: cc.Usage, messages: messages,
			}, nil
		}

		var proxyCalls, clientCalls []toolCall
		for _, c := range calls {
			if p.tools.Has(c.Function.Name) {
				proxyCalls = append(proxyCalls, c)
			} else {
				clientCalls = append(clientCalls, c)
			}
		}

		// Any client-owned call: hand the whole batch back to the client. We
		// don't execute a partial set — the client needs all of them to satisfy
		// the assistant turn it'll send next.
		if len(clientCalls) > 0 {
			p.logger.InfoContext(ctx, "returning client tool calls",
				"client", len(clientCalls), "proxy", len(proxyCalls))
			return loopResult{
				outcome: outcomeClientCalls, content: content, reasoning: reasoning,
				toolCalls: calls, usage: cc.Usage, messages: messages,
			}, nil
		}

		// All proxy-owned: record the assistant turn, run each tool, append the
		// results, and loop for the model's next move.
		p.logger.InfoContext(ctx, "executing proxy tools", "round", round+1, "count", len(proxyCalls))
		messages = append(messages, assistantToolCallMessage(content, calls))
		for _, c := range proxyCalls {
			out := p.tools.Execute(ctx, c.Function.Name, c.Function.Arguments)
			messages = append(messages, toolResultMessage(c.ID, out))
		}
	}

	p.logger.WarnContext(ctx, "max tool rounds reached", "rounds", p.maxToolRounds)
	return loopResult{
		outcome: outcomeMaxRounds, content: lastContent, reasoning: lastReasoning,
		messages: messages,
	}, nil
}

// runToolLoopJSON runs the loop and writes a single non-streaming
// chat.completion. Mirrors _non_streaming_chat_completion.
func (p *Proxy) runToolLoopJSON(w http.ResponseWriter, r *http.Request, res resolveResult, bodyMap map[string]any, messages []any, allTools []any, toolChoice any) {
	ctx := r.Context()
	model := res.BackendModel

	result, err := p.runLoop(ctx, res.BackendURL, bodyMap, messages, allTools, toolChoice)
	if err != nil {
		p.logger.ErrorContext(ctx, "tool loop backend error", "backend_url", res.BackendURL, "err", err)
		writeJSONError(w, http.StatusBadGateway, "backend error: "+err.Error())
		return
	}

	id := "chatcmpl-" + randHex(12)
	switch result.outcome {
	case outcomeClientCalls:
		writeChatCompletionJSON(w, newChatCompletion(id, model, result.content, result.reasoning, result.toolCalls, result.usage, "tool_calls"))
	case outcomeMaxRounds:
		content := result.content
		if content == "" {
			content = maxRoundsMessage
		}
		// Match the Python proxy: usage omitted on the max-rounds path.
		writeChatCompletionJSON(w, newChatCompletion(id, model, content, result.reasoning, nil, nil, "stop"))
	default: // outcomeFinal
		writeChatCompletionJSON(w, newChatCompletion(id, model, result.content, result.reasoning, nil, result.usage, "stop"))
	}
}

// runToolLoopStreaming runs the loop, then streams the result as SSE. On a
// natural answer (outcomeFinal) it re-streams the final generation straight
// from the backend for true token-by-token output; the client-call and
// max-rounds branches emit hand-built SSE. Mirrors _stream_chat_completion.
func (p *Proxy) runToolLoopStreaming(w http.ResponseWriter, r *http.Request, res resolveResult, bodyMap map[string]any, messages []any, allTools []any, toolChoice any) {
	ctx := r.Context()
	model := res.BackendModel

	result, err := p.runLoop(ctx, res.BackendURL, bodyMap, messages, allTools, toolChoice)

	// Final answer: drop the tools, re-issue the request as a stream, and relay
	// the backend's tokens through the reverse proxy. This is the one branch
	// that doesn't build SSE by hand (and the deliberate redundant generation
	// the Python proxy also pays for true streaming).
	if err == nil && result.outcome == outcomeFinal {
		bodyMap["messages"] = result.messages
		delete(bodyMap, "tools")
		delete(bodyMap, "tool_choice")
		bodyMap["stream"] = true
		finalBody, mErr := json.Marshal(bodyMap)
		if mErr == nil {
			p.reverseProxyTo(w, r, res.BackendURL, finalBody)
			return
		}
		err = fmt.Errorf("toolproxy: marshal final stream body: %w", mErr)
	}

	// Every other branch emits SSE directly.
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	chunkID := "chatcmpl-" + randHex(12)

	if err != nil {
		p.logger.ErrorContext(ctx, "tool loop backend error (streaming)", "backend_url", res.BackendURL, "err", err)
		writeSSE(w, flusher, buildSSEChunk(chunkID, model, sseFields{role: "assistant"}))
		writeSSE(w, flusher, buildSSEChunk(chunkID, model, sseFields{content: "Error: " + err.Error(), finishReason: "stop"}))
		writeSSE(w, flusher, sseDone)
		return
	}

	switch result.outcome {
	case outcomeClientCalls:
		// Single chat.completion object inside the stream, then [DONE] — the
		// Python proxy's build_tool_calls_response breakout (no usage field).
		cc := newChatCompletion(chunkID, model, result.content, result.reasoning, result.toolCalls, nil, "tool_calls")
		b, _ := json.Marshal(cc)
		writeSSE(w, flusher, "data: "+string(b)+"\n\n")
		writeSSE(w, flusher, sseDone)
	case outcomeMaxRounds:
		writeSSE(w, flusher, buildSSEChunk(chunkID, model, sseFields{role: "assistant"}))
		if result.reasoning != "" {
			writeSSE(w, flusher, buildSSEChunk(chunkID, model, sseFields{reasoning: result.reasoning}))
		}
		content := result.content
		if content == "" {
			content = maxRoundsMessage
		}
		writeSSE(w, flusher, buildSSEChunk(chunkID, model, sseFields{content: content, finishReason: "stop"}))
		writeSSE(w, flusher, sseDone)
	}
}

// writeSSE writes one SSE event and flushes so the client sees it immediately.
func writeSSE(w http.ResponseWriter, flusher http.Flusher, s string) {
	_, _ = io.WriteString(w, s)
	if flusher != nil {
		flusher.Flush()
	}
}

// ---------------------------------------------------------------------------
// Tool injection + message construction
// ---------------------------------------------------------------------------

// mergeTools builds the request's `tools` array: every proxy tool first, then
// any client-supplied tool whose name doesn't collide with a proxy tool
// (proxy names win). Matches the Python proxy's merge.
func (p *Proxy) mergeTools(clientTools []any) []any {
	defs := p.tools.Definitions()
	out := make([]any, 0, len(defs)+len(clientTools))
	proxyNames := make(map[string]bool, len(defs))
	for _, n := range p.tools.Names() {
		proxyNames[n] = true
	}
	for _, d := range defs {
		out = append(out, d)
	}
	for _, ct := range clientTools {
		if name := toolFunctionName(ct); name != "" && !proxyNames[name] {
			out = append(out, ct)
		}
	}
	return out
}

// toolFunctionName digs the function name out of an OpenAI tool definition
// ({"type":"function","function":{"name":...}}). Returns "" if the shape is off.
func toolFunctionName(t any) string {
	m, ok := t.(map[string]any)
	if !ok {
		return ""
	}
	fn, ok := m["function"].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := fn["name"].(string)
	return name
}

// assistantToolCallMessage is the assistant turn recorded in history when the
// model calls proxy tools. Reasoning is intentionally omitted — the model
// shouldn't see its own thinking replayed (matching the Python proxy).
func assistantToolCallMessage(content string, calls []toolCall) map[string]any {
	return map[string]any{
		"role":       "assistant",
		"content":    content,
		"tool_calls": calls,
	}
}

// toolResultMessage is the `tool` turn carrying one tool's output.
func toolResultMessage(toolCallID, content string) map[string]any {
	return map[string]any{
		"role":         "tool",
		"tool_call_id": toolCallID,
		"content":      content,
	}
}
