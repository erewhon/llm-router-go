package toolproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/erewhon/llm-router-go/internal/toolproxy/tools"
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
	// finishReason is the backend's own finish_reason for the round that ended
	// the loop. Needed because a backend can cut a final answer short while
	// tools are still attached (see truncatedFinal / regenerateFinal).
	finishReason string
	// tools summarises what the executed proxy tools actually did, so an empty
	// answer can be reported with the reason behind it.
	tools toolStats
	// lastRaw describes the backend message that ended the loop. Only used for
	// diagnostics when the answer comes back empty — it's the only way to tell
	// the different empty-completion causes apart from outside the engine.
	lastRaw rawMessageInfo
}

// toolStats counts proxy-tool executions across a whole loop run.
type toolStats struct {
	executed int
	failed   int
	// firstFailure is "<tool>: <output>" for the first failing call, truncated.
	firstFailure string
}

// allFailed reports whether every executed tool failed — the signature of a
// broken egress rather than one bad query.
func (t toolStats) allFailed() bool { return t.executed > 0 && t.failed == t.executed }

// rawMessageInfo is the shape of a backend assistant message, for logging when
// the content is empty. A model that emitted tokens but no content means the
// engine dropped something (e.g. a tool call naming an undeclared function).
type rawMessageInfo struct {
	finishReason     string
	completionTokens int
	hadToolCalls     bool
	hadReasoning     bool
	contentLen       int
}

// logAttrs renders the info for a structured log call.
func (r rawMessageInfo) logAttrs() []any {
	return []any{
		"backend_finish_reason", r.finishReason,
		"completion_tokens", r.completionTokens,
		"had_tool_calls", r.hadToolCalls,
		"had_reasoning", r.hadReasoning,
		"content_len", r.contentLen,
	}
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
	var stats toolStats
	var lastRaw rawMessageInfo

	for round := 0; round < p.maxToolRounds; round++ {
		bodyMap["messages"] = messages
		bodyMap["tools"] = allTools
		bodyMap["tool_choice"] = toolChoice
		bodyMap["stream"] = false
		// stream_options is only valid alongside stream=true. The client's value
		// survives in bodyMap, so it must go whenever we force non-streaming or
		// strict backends reject the whole request. vLLM 400s; Atlas tolerated it.
		delete(bodyMap, "stream_options")

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
		// Reasoning (2d): prefer the backend's structured reasoning_content;
		// fall back to <think> tags inlined in the content. cleanContent has any
		// think block removed so it isn't shown to the client or replayed to the
		// model. The streaming final answer is relayed raw, so this tag split
		// only affects non-streamed and client-call/max-rounds responses;
		// structured reasoning_content deltas pass through the relay untouched.
		tagReasoning, cleanContent := extractThinking(content)
		reasoning := msg.reasoningText()
		if reasoning == "" {
			reasoning = tagReasoning
		}
		lastContent, lastReasoning = cleanContent, reasoning

		calls := extractToolCalls(msg)
		lastRaw = rawMessageInfo{
			finishReason:     cc.Choices[0].FinishReason,
			completionTokens: usageCompletionTokens(cc.Usage),
			hadToolCalls:     len(calls) > 0,
			hadReasoning:     reasoning != "",
			contentLen:       len(cleanContent),
		}
		if len(calls) == 0 {
			return loopResult{
				outcome: outcomeFinal, content: cleanContent, reasoning: reasoning,
				usage: cc.Usage, messages: messages,
				finishReason: cc.Choices[0].FinishReason,
				tools:        stats, lastRaw: lastRaw,
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
				outcome: outcomeClientCalls, content: cleanContent, reasoning: reasoning,
				toolCalls: calls, usage: cc.Usage, messages: messages,
				finishReason: cc.Choices[0].FinishReason,
				tools:        stats, lastRaw: lastRaw,
			}, nil
		}

		// All proxy-owned: record the assistant turn (thinking stripped so the
		// model doesn't see its own reasoning replayed), run each tool, append
		// the results, and loop for the model's next move.
		p.logger.InfoContext(ctx, "executing proxy tools", "round", round+1, "count", len(proxyCalls))
		messages = append(messages, assistantToolCallMessage(cleanContent, calls))
		messages = p.executeProxyCalls(ctx, round, proxyCalls, &stats, messages)
	}

	p.logger.WarnContext(ctx, "max tool rounds reached",
		"rounds", p.maxToolRounds, "tools_executed", stats.executed, "tools_failed", stats.failed)
	return loopResult{
		outcome: outcomeMaxRounds, content: lastContent, reasoning: lastReasoning,
		messages: messages, tools: stats, lastRaw: lastRaw,
	}, nil
}

// executeProxyCalls runs each proxy-owned call, counts it in stats, and
// appends its result to messages. Shared by the JSON and streaming loops.
func (p *Proxy) executeProxyCalls(ctx context.Context, round int, calls []toolCall, stats *toolStats, messages []any) []any {
	for _, c := range calls {
		out := p.tools.Execute(ctx, c.Function.Name, c.Function.Arguments)
		stats.executed++
		if tools.IsFailure(out) {
			stats.failed++
			if stats.firstFailure == "" {
				stats.firstFailure = c.Function.Name + ": " + truncateForLog(out, 200)
			}
			// Log every failing tool call: without this a dead egress is
			// invisible in the journal until someone reproduces it by hand.
			p.logger.WarnContext(ctx, "proxy tool failed",
				"tool", c.Function.Name, "round", round+1,
				"args", truncateForLog(c.Function.Arguments, 200),
				"result", truncateForLog(out, 200))
		}
		messages = append(messages, toolResultMessage(c.ID, out))
	}
	return messages
}

// usageCompletionTokens safely reads completion tokens from a possibly-nil usage.
func usageCompletionTokens(u *usage) int {
	if u == nil {
		return 0
	}
	return u.CompletionTokens
}

// truncateForLog caps a string so a huge tool payload can't flood the journal.
func truncateForLog(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// truncatedFinal reports whether the loop's final answer was cut short by the
// backend rather than ending naturally. Some engines cap the prose a model may
// emit while tool definitions are still attached — Atlas, for one, enforces a
// 384-token "inter-tool prose budget" and ends the response with
// finish_reason "length" on the assumption the model should have called a tool.
// A research-style final answer is exactly that shape (long prose, tools still
// in the request), so it gets truncated mid-sentence.
func truncatedFinal(res loopResult) bool {
	return res.outcome == outcomeFinal && res.finishReason == "length"
}

// regenerateFinal re-asks the backend for the final answer with the tools
// stripped, so no tool-related generation cap applies. This is the
// non-streaming twin of what runToolLoopStreaming already does when it
// re-issues the final generation without tools; the conversation is unchanged
// (tool results are all in messages), only the tool definitions are dropped.
// The prior result supplies the conversation and the tool stats to carry
// forward — the regenerated result replaces the loop's, so it must stay
// diagnosable (see reportEmptyAnswer).
func (p *Proxy) regenerateFinal(ctx context.Context, backendURL string, bodyMap map[string]any, prior loopResult) (loopResult, error) {
	body := make(map[string]any, len(bodyMap))
	for k, v := range bodyMap {
		body[k] = v
	}
	body["messages"] = prior.messages
	delete(body, "tools")
	delete(body, "tool_choice")
	body["stream"] = false
	delete(body, "stream_options") // only valid with stream=true; see runLoop

	raw, err := json.Marshal(body)
	if err != nil {
		return loopResult{}, fmt.Errorf("toolproxy: marshal regenerate body: %w", err)
	}
	cc, err := p.backendComplete(ctx, backendURL, raw)
	if err != nil {
		return loopResult{}, err
	}
	if len(cc.Choices) == 0 {
		return loopResult{}, fmt.Errorf("toolproxy: backend returned no choices")
	}

	msg := cc.Choices[0].Message
	tagReasoning, cleanContent := extractThinking(stripToolCallTags(msg.Content))
	reasoning := msg.reasoningText()
	if reasoning == "" {
		reasoning = tagReasoning
	}
	return loopResult{
		outcome: outcomeFinal, content: cleanContent, reasoning: reasoning,
		usage: cc.Usage, messages: prior.messages,
		finishReason: cc.Choices[0].FinishReason,
		tools:        prior.tools,
		lastRaw: rawMessageInfo{
			finishReason:     cc.Choices[0].FinishReason,
			completionTokens: usageCompletionTokens(cc.Usage),
			hadToolCalls:     len(extractToolCalls(msg)) > 0,
			hadReasoning:     reasoning != "",
			contentLen:       len(cleanContent),
		},
	}, nil
}

// emptyAnswer reports whether a terminal outcome produced no usable answer.
// Deliberately strict (empty after trimming) rather than "near-empty": a short
// answer like "4" is legitimate, and guessing at a length threshold would
// reject real completions.
// Scoped to outcomeFinal: outcomeClientCalls carries tool_calls as its payload
// (empty content is expected), and outcomeMaxRounds already returns a
// non-empty marker of its own — see maxRoundsContent.
func emptyAnswer(res loopResult) bool {
	if res.outcome != outcomeFinal || strings.TrimSpace(res.content) != "" {
		return false
	}
	// Empty content with finish_reason "length" and reasoning attached is an
	// honest generation cap, not a swallowed answer: a reasoning model under a
	// small max_tokens spends the whole budget thinking and never reaches the
	// answer (Nemotron Lightning does this on any plain completion with
	// max_tokens below its reasoning run). The backend itself returns HTTP 200
	// for it, so the proxy must pass it through — the client's remedy is a
	// larger max_tokens, and a 502 here made the whole route look down.
	if res.finishReason == "length" && res.reasoning != "" {
		return false
	}
	return true
}

// maxRoundsContent is the body for a run that exhausted its rounds. The bare
// placeholder says nothing about *why*, so failing tools are named — a run that
// burned every round on a dead egress should not look like a chatty model.
func maxRoundsContent(res loopResult) string {
	if res.content != "" {
		return res.content
	}
	if res.tools.failed > 0 {
		return fmt.Sprintf("%s — %d of %d tool call(s) failed. First failure — %s",
			maxRoundsMessage, res.tools.failed, res.tools.executed, res.tools.firstFailure)
	}
	return maxRoundsMessage
}

// emptyAnswerMessage explains an empty completion to the caller, naming the
// cause when the proxy can tell. Two distinct paths produce one:
//
//   - tools ran and failed (dead SOCKS/VPN egress, upstream 4xx/5xx) — the
//     model gets nothing but error strings and gives up;
//   - no tool ran at all and the backend emitted tokens that never became
//     content. Observed with Atlas: the model calls a tool the request never
//     declared (e.g. a system prompt advertising tavily_search while the proxy
//     only registers web_search/fetch_url/calculator), and grammar-constrained
//     decoding silently discards the whole tool call.
//
// Either way the old behaviour — HTTP 200 with content:"" — made an outage
// look like a successful request.
func emptyAnswerMessage(res loopResult) string {
	switch {
	case res.tools.allFailed():
		return fmt.Sprintf(
			"tool proxy: model returned no answer after all %d tool call(s) failed "+
				"(check the web-tool egress). First failure — %s",
			res.tools.executed, res.tools.firstFailure)
	case res.tools.failed > 0:
		return fmt.Sprintf(
			"tool proxy: model returned no answer; %d of %d tool call(s) failed. First failure — %s",
			res.tools.failed, res.tools.executed, res.tools.firstFailure)
	case res.tools.executed == 0:
		return fmt.Sprintf(
			"tool proxy: model returned no answer and executed no tools "+
				"(backend finish_reason=%q, %d completion tokens, tool_calls=%v). "+
				"The backend generated tokens that never surfaced as content — this happens when the model "+
				"calls a tool the request didn't declare, so the engine discards the call. Check that every "+
				"tool named in the system prompt is actually registered on the proxy.",
			res.lastRaw.finishReason, res.lastRaw.completionTokens, res.lastRaw.hadToolCalls)
	default:
		return fmt.Sprintf(
			"tool proxy: model returned no answer after %d successful tool call(s) "+
				"(backend finish_reason=%q, %d completion tokens).",
			res.tools.executed, res.lastRaw.finishReason, res.lastRaw.completionTokens)
	}
}

// reportEmptyAnswer logs the empty completion with the raw backend shape and
// writes a 502. Returns true when it handled the response.
func (p *Proxy) reportEmptyAnswer(ctx context.Context, w http.ResponseWriter, res loopResult) bool {
	if !emptyAnswer(res) {
		return false
	}
	msg := emptyAnswerMessage(res)
	attrs := append([]any{"tools_executed", res.tools.executed, "tools_failed", res.tools.failed,
		"first_failure", res.tools.firstFailure}, res.lastRaw.logAttrs()...)
	p.logger.ErrorContext(ctx, "empty completion from backend; returning 502 instead of an empty 200", attrs...)
	writeJSONError(w, http.StatusBadGateway, msg)
	return true
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

	// A final answer the backend truncated while tools were attached is retried
	// once without them; without it, non-streaming callers silently receive a
	// half-written answer — invalid JSON for structured-output clients. (The
	// streaming driver relays rounds live and cannot retract text; see stream.go.)
	if truncatedFinal(result) {
		p.logger.WarnContext(ctx, "final answer truncated with tools attached; regenerating without tools",
			"backend_url", res.BackendURL, "content_len", len(result.content))
		regen, rErr := p.regenerateFinal(ctx, res.BackendURL, bodyMap, result)
		if rErr != nil {
			// Keep the truncated answer rather than failing the request — it's
			// degraded but not empty.
			p.logger.ErrorContext(ctx, "regenerate final answer failed; returning truncated answer",
				"backend_url", res.BackendURL, "err", rErr)
		} else {
			result = regen
		}
	}

	// Never hand back a successful-looking empty completion — that is what made
	// a dead egress indistinguishable from a working request.
	if p.reportEmptyAnswer(ctx, w, result) {
		return
	}

	id := "chatcmpl-" + randHex(12)
	switch result.outcome {
	case outcomeClientCalls:
		writeChatCompletionJSON(w, newChatCompletion(id, model, result.content, result.reasoning, result.toolCalls, result.usage, "tool_calls"))
	case outcomeMaxRounds:
		// Match the Python proxy: usage omitted on the max-rounds path.
		writeChatCompletionJSON(w, newChatCompletion(id, model, maxRoundsContent(result), result.reasoning, nil, nil, "stop"))
	default: // outcomeFinal
		// Report the backend's own finish_reason. Hardcoding "stop" here used to
		// disguise a truncated answer ("length") as a natural completion, which
		// made backend-side generation caps invisible to clients.
		writeChatCompletionJSON(w, newChatCompletion(id, model, result.content, result.reasoning, nil, result.usage, finalFinishReason(result)))
	}
}

// finalFinishReason is the finish_reason reported for an outcomeFinal answer,
// defaulting to "stop" when the backend didn't supply one.
func finalFinishReason(res loopResult) string {
	if res.finishReason != "" {
		return res.finishReason
	}
	return "stop"
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
