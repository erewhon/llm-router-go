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
	"fmt"

	"github.com/erewhon/llm-router-go/internal/config"
)

// charsPerToken is the chars-per-token proxy used to size a request without
// dragging a tokenizer into the hot path. It matches the constant qualeval
// uses, so an estimate here and a measurement there stay comparable.
//
// THE BIAS IS DELIBERATE, and the next person should not "fix" it blind:
// chars/3.5 UNDERCOUNTS some tokenizers by up to ~50%. Two things make that
// acceptable. The estimate runs over the whole serialized JSON body — braces,
// keys and quoting included — which adds weight the tokenizer never sees and
// offsets part of the undercount. And every threshold it is compared against
// is chosen with margin: an envelope marks where a seat stops being worth
// routing to, not a cliff at one exact token. Swapping this for a real
// tokenizer means re-deriving the effective_context values in models.yaml,
// which were picked against this bias.
const charsPerToken = 3.5

// estimatePromptTokens sizes a request from its serialized body.
//
// Deliberately the WHOLE body — every message, the system prompt and the tool
// definitions — rather than the last user message. In an agent loop the last
// message is typically a short tool result sitting on top of an enormous
// transcript, which is precisely the shape that overruns an envelope; sizing
// on the last message alone is structurally blind to it.
func estimatePromptTokens(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	return int(float64(len(body)) / charsPerToken)
}

// contextGate reports why a candidate should be skipped for a request of
// promptTokens, or "" when it passes.
//
// promptTokens <= 0 disables the gate. Introspection paths (/v1/availability,
// /v1/models, the dashboard) resolve roles with no request in hand, and must
// keep reporting where a role points rather than where it would point for a
// hypothetical prompt.
//
// The hard limit comes from wellKnownContext, so the router refuses exactly
// what it advertised it could not take. Passing a zero default keeps that
// honest in the other direction: a model that never declared a window (no
// context_length, no max_model_len) gets no hard gate rather than an invented
// one, which is what makes this change inert for undeclared models.
func (rt *Router) contextGate(m config.ModelDefinition, promptTokens int) string {
	if promptTokens <= 0 {
		return ""
	}
	if hard := rt.wellKnownContext(m, 0); hard > 0 && promptTokens > hard {
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
