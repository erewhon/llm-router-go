package router

// Context-envelope gating: skip a role candidate the request is too big for.
//
// Two thresholds share one code path:
//
//   - context_length is HARD. The seat cannot fit the prompt at all, so
//     dispatching it only moves the failure from the router to the backend,
//     where it arrives later and reads worse.
//   - effective_context is SOFT. The seat will serve the prompt, but badly
//     enough that answering "the best available thinker" with it is not
//     honouring the request. Lightning is the motivating case: ~102 t/s decode
//     at short context, ~6 t/s past 64K, which turns an agent loop into a
//     45-minute timeout.
//
// Both gates govern ROLE resolution only. Naming a model directly bypasses
// roles entirely (models.yaml says so at the top), so "you asked for it, you
// get it" survives untouched — the gate only ever fires when the caller asked
// for an intent and left the choice to the router.

import (
	"encoding/json"
	"fmt"

	"github.com/erewhon/llm-router-go/internal/config"
)

// charsPerToken is the chars-per-token proxy used to size a request without
// dragging a tokenizer into the hot path. It is applied to the TEXT the
// backend will tokenize (see estimatePromptTokens), never to the JSON around
// it.
//
// Calibrated 2026-09-18 against usage.prompt_tokens from the fleet's own
// seats. Text chars per real token: Go source 3.9 (GLM, gpt-oss) / 3.35
// (Gemma); English docs 3.7 / 3.35; a 220K-char agent transcript with
// numbered tool results 3.25 (GLM) and 2.9 (gpt-oss, whose harmony framing
// adds tokens per tool turn); and the Pi session that motivated this file
// ran near 4.4 on space-indented code (a run of spaces is one token).
// 3.5 sits in the middle of that band: a few percent high on plain code
// and prose, ~10-20% low on dense tool-heavy transcripts. The band is why
// the hard gate carries hardGateSlack rather than refusing at the window.
//
// qualeval's CHARS_PER_TOKEN (estimate.py) is a separate cost-estimation
// constant and need not track this one.
const charsPerToken = 3.5

// imageTokens is what one image part counts for. Vision requests carry the
// image as a base64 data URL, which a byte-based estimate would score as
// hundreds of thousands of tokens; the backends themselves bill an image at
// a few hundred to a couple of thousand tokens depending on size.
const imageTokens = 1024

// messageOverheadTokens covers what the chat template adds per message —
// role markers, separators, tool-call framing — none of which is in the
// content the client sent.
const messageOverheadTokens = 4

// hardGateSlack is how far past a seat's declared window the estimate must
// land before the router refuses on context_length. The estimate is a
// heuristic with a ±25% band across content types (see charsPerToken); a
// refusal inside that band is the router guessing, and guessing wrong costs
// the caller a seat that would have served. Past the band the prompt cannot
// fit by any reading, and refusing here beats a slower, worse-worded failure
// at the backend. Inside it the backend — the only party with the tokenizer
// — makes the call. The soft gate (effective_context) is not slackened: it
// marks where a seat stops being worth routing to, and "a little early" is
// the right side to err on there.
const hardGateSlack = 1.25

// estimatePromptTokens sizes a request from its parsed body.
//
// Deliberately the WHOLE transcript — every message, the system prompt, the
// tool definitions and every tool result — rather than the last user
// message. In an agent loop the last message is typically a short tool
// result sitting on top of an enormous transcript, which is precisely the
// shape that overruns an envelope; sizing on the last message alone is
// structurally blind to it.
//
// But only the text of it. The earlier implementation divided the raw body
// length by charsPerToken, and on an agent transcript the JSON envelope —
// quoting, escaped newlines and tabs in every line of code, the tool-result
// wrapper per turn — inflated that by a quarter, so the hard gate refused
// prompts the seat had room for. Measuring what the tokenizer will actually
// see removes that bias without a tokenizer.
//
// A body with no messages (nothing to size) returns 0, which disables the
// gate; contextGate documents that contract.
func estimatePromptTokens(body map[string]any) int {
	msgs, _ := body["messages"].([]any)
	if len(msgs) == 0 {
		return 0
	}
	chars, tokens := 0, 0
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		tokens += messageOverheadTokens
		c, t := contentSize(m["content"])
		chars, tokens = chars+c, tokens+t
		// reasoning_content (prior-turn thinking some clients echo back) is
		// deliberately not counted: the GLM, Qwen and harmony chat templates
		// drop earlier turns' reasoning before tokenizing, so it never
		// reaches the model.
		if name, _ := m["name"].(string); name != "" {
			chars += len(name)
		}
		if calls, ok := m["tool_calls"].([]any); ok {
			for _, rawCall := range calls {
				call, _ := rawCall.(map[string]any)
				fn, _ := call["function"].(map[string]any)
				name, _ := fn["name"].(string)
				args, _ := fn["arguments"].(string)
				chars += len(name) + len(args)
			}
		}
	}
	// Tool definitions reach the model as text (the template renders the
	// JSON schema), so their compact serialization is the right measure.
	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		if b, err := json.Marshal(tools); err == nil {
			chars += len(b)
		}
	}
	return tokens + int(float64(chars)/charsPerToken)
}

// contentSize measures one message's content: a string, or the OpenAI
// content-part array (text parts by length, image/audio/file parts at a flat
// per-part cost). Returns (chars, tokens) so the two units stay separate
// until the caller converts.
func contentSize(content any) (chars, tokens int) {
	switch c := content.(type) {
	case string:
		return len(c), 0
	case []any:
		for _, rawPart := range c {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := part["text"].(string); ok {
				chars += len(text)
				continue
			}
			switch part["type"] {
			case "image_url", "input_image", "input_audio", "file":
				tokens += imageTokens
			}
		}
	}
	return chars, tokens
}

// contextGate reports why a candidate should be skipped for a request of
// promptTokens, or "" when it passes.
//
// promptTokens <= 0 disables the gate. Introspection paths (/v1/availability,
// /v1/models, the dashboard) resolve roles with no request in hand, and must
// keep reporting where a role points rather than where it would point for a
// hypothetical prompt.
//
// The hard limit comes from wellKnownContext, so the router refuses what it
// advertised it could not take — once the estimate is past the window by
// more than hardGateSlack, the estimator's own error band. Passing a zero
// default keeps that honest in the other direction: a model that never
// declared a window (no context_length, no max_model_len) gets no hard gate
// rather than an invented one, which is what makes this change inert for
// undeclared models.
func (rt *Router) contextGate(m config.ModelDefinition, promptTokens int) string {
	if promptTokens <= 0 {
		return ""
	}
	if hard := rt.wellKnownContext(m, 0); hard > 0 && float64(promptTokens) > float64(hard)*hardGateSlack {
		return fmt.Sprintf("prompt ~%s tokens > context_length %s",
			formatTokens(promptTokens), formatTokens(hard))
	}
	if m.EffectiveContext > 0 && promptTokens > m.EffectiveContext {
		return fmt.Sprintf("prompt ~%s tokens > effective_context %s",
			formatTokens(promptTokens), formatTokens(m.EffectiveContext))
	}
	return ""
}

// formatTokens renders a token count the way it gets read at 2am: "72k", not
// "74213". Sub-1k counts keep their digits — at that size the exact number is
// still meaningful.
//
// k is 1024, not 1000, and that choice is load-bearing for readability: these
// thresholds are power-of-two context windows, and a reason string calling
// 131072 "131k" would not match the model everyone calls a 128k seat. The
// dashboard's fmtCtx divides the same way, so the two never disagree.
func formatTokens(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%dk", n/1024)
}
