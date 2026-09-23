package toolproxy

// Streaming tool loop: every round is streamed from the backend and relayed to
// the client as it arrives.
//
// The earlier driver ran each round with stream:false and, once the model
// answered without tools, threw that answer away and generated it again as a
// stream. A streaming client therefore waited a whole generation for its first
// byte, and every tool-proxied streaming request cost two generations — the
// "buffered stream" that made client-side timings behind the router useless.
//
// Now each round is one streamed backend call with the tools attached:
//   - reasoning deltas are forwarded as they arrive;
//   - content deltas are forwarded, except that text which could begin a
//     textual <tool_call> tag (models without a tool-call parser) is held back;
//     once a tag starts, the rest of the round's content is captured and parsed
//     like the non-streaming path does;
//   - structured tool_calls deltas are accumulated, never forwarded.
// A round without tool calls has already been streamed — the proxy only adds
// the backend's terminal chunk (finish_reason, llama.cpp's timings) with usage
// summed across rounds, then [DONE]. Content forwarded before a tool call is
// the model's preamble; the next round continues after the tool results.
//
// Nothing is written until the first real delta, so an empty answer can still
// be refused with a plain 502 (see reportEmptyAnswer). The non-streaming JSON
// path is unchanged, including the regeneration of an answer a backend cut
// short while tools were attached (Atlas' prose cap): a streaming round that
// ends with finish_reason "length" is passed through as-is — the text is
// already with the client. No Atlas backend is tool-proxied as of 2026-09-23.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// toolCallTag opens a textual tool call in content.
const toolCallTag = "<tool_call>"

// maxSSELine bounds one backend SSE line (a chunk carrying a long tool-call
// argument or a big reasoning delta).
const maxSSELine = 8 * 1024 * 1024

// sseOut writes the client's SSE stream, deferring the headers until the
// first event so the proxy can still answer with a plain HTTP error before it.
type sseOut struct {
	w       http.ResponseWriter
	flusher http.Flusher
	id      string
	model   string
	started bool
	// roleSent: the first delta carries role:"assistant", as OpenAI streams do.
	roleSent bool
}

func newSSEOut(w http.ResponseWriter, model string) *sseOut {
	f, _ := w.(http.Flusher)
	return &sseOut{w: w, flusher: f, id: "chatcmpl-" + randHex(12), model: model}
}

func (s *sseOut) start() {
	if s.started {
		return
	}
	s.started = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
}

func (s *sseOut) raw(event string) {
	s.start()
	writeSSE(s.w, s.flusher, event)
}

// delta emits one content or reasoning delta.
func (s *sseOut) delta(content, reasoning string) {
	if content == "" && reasoning == "" {
		return
	}
	f := sseFields{content: content, reasoning: reasoning}
	if !s.roleSent {
		f.role = "assistant"
		s.roleSent = true
	}
	s.raw(buildSSEChunk(s.id, s.model, f))
}

// roundResult is one streamed backend round, reassembled.
type roundResult struct {
	content      string // every content delta, including held-back text
	reasoning    string
	toolCalls    []toolCall // structured tool_calls deltas, merged by index
	finishReason string
	usage        *usage
	// terminal holds the backend chunks that carried finish_reason, usage or
	// timings, with their content already relayed; replayed when the round
	// is the last one.
	terminal []map[string]any
	// forwarded is true once any content of this round reached the client.
	forwarded bool
	// held is content withheld because it began a textual tool call. It is
	// flushed after all if the round turns out to have no tool calls.
	held string
}

// contentGate decides what content may be forwarded now.
type contentGate struct {
	pending   string // not yet forwarded, might be the start of toolCallTag
	capturing bool   // a toolCallTag was seen; nothing more is forwarded
	captured  string
}

// push appends a content delta and returns the part that is safe to forward.
func (g *contentGate) push(s string) string {
	if g.capturing {
		g.captured += s
		return ""
	}
	g.pending += s
	if i := strings.Index(g.pending, toolCallTag); i >= 0 {
		out := g.pending[:i]
		g.captured = g.pending[i:]
		g.pending = ""
		g.capturing = true
		return out
	}
	keep := tagPrefixSuffix(g.pending)
	out := g.pending[:len(g.pending)-keep]
	g.pending = g.pending[len(g.pending)-keep:]
	return out
}

// tagPrefixSuffix is the length of the longest suffix of s that is a proper
// prefix of toolCallTag — the bytes that must wait for the next delta.
func tagPrefixSuffix(s string) int {
	for n := min(len(s), len(toolCallTag)-1); n > 0; n-- {
		if strings.HasSuffix(s, toolCallTag[:n]) {
			return n
		}
	}
	return 0
}

// streamDelta is the subset of a chat.completion.chunk the loop reads.
type streamDelta struct {
	Content          *string `json:"content"`
	ReasoningContent string  `json:"reasoning_content"`
	Reasoning        string  `json:"reasoning"`
	ToolCalls        []struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

type streamChunk struct {
	Choices []struct {
		Delta        streamDelta `json:"delta"`
		FinishReason *string     `json:"finish_reason"`
	} `json:"choices"`
	Usage *usage `json:"usage"`
}

// backendStream opens one streamed chat-completions call. No client timeout:
// a long generation is bounded by the inbound request's context, like the
// reverse-proxied passthrough.
func (p *Proxy) backendStream(ctx context.Context, backendURL string, body []byte) (*http.Response, error) {
	url := strings.TrimSuffix(backendURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("toolproxy: build backend request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{Transport: p.transport}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("toolproxy: backend request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()
		return nil, &backendError{status: resp.StatusCode, body: string(raw)}
	}
	return resp, nil
}

// streamRound runs one streamed round, forwarding what is safe as it arrives.
func (p *Proxy) streamRound(ctx context.Context, out *sseOut, backendURL string, body []byte) (roundResult, error) {
	var rr roundResult
	resp, err := p.backendStream(ctx, backendURL, body)
	if err != nil {
		return rr, err
	}
	defer resp.Body.Close()

	var gate contentGate
	var content, reasoning strings.Builder
	type partialCall struct {
		id, name string
		args     strings.Builder
	}
	calls := map[int]*partialCall{}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), maxSSELine)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var ch streamChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			continue // a malformed keep-alive or vendor event; skip it
		}
		if ch.Usage != nil {
			rr.usage = ch.Usage
		}
		terminal := ch.Usage != nil
		for _, c := range ch.Choices {
			d := c.Delta
			r := d.Reasoning
			if r == "" {
				r = d.ReasoningContent
			}
			if r != "" {
				reasoning.WriteString(r)
			}
			var fwd string
			if d.Content != nil && *d.Content != "" {
				content.WriteString(*d.Content)
				fwd = gate.push(*d.Content)
			}
			if fwd != "" {
				rr.forwarded = true
			}
			out.delta(fwd, r)
			for _, tc := range d.ToolCalls {
				pc := calls[tc.Index]
				if pc == nil {
					pc = &partialCall{}
					calls[tc.Index] = pc
				}
				if tc.ID != "" {
					pc.id = tc.ID
				}
				if tc.Function.Name != "" {
					pc.name = tc.Function.Name
				}
				pc.args.WriteString(tc.Function.Arguments)
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				rr.finishReason = *c.FinishReason
				terminal = true
			}
		}
		if !terminal {
			// llama.cpp puts timings on the final content chunk; keep any
			// chunk that carries them for the replay.
			terminal = bytes.Contains([]byte(data), []byte(`"timings"`))
		}
		if terminal {
			var m map[string]any
			if json.Unmarshal([]byte(data), &m) == nil {
				stripDeltas(m)
				rr.terminal = append(rr.terminal, m)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return rr, fmt.Errorf("toolproxy: read backend stream: %w", err)
	}

	// Whatever the gate still holds: a tag prefix that never completed goes
	// out now; a captured tool-call block is kept for the caller to parse.
	if gate.pending != "" {
		out.delta(gate.pending, "")
		rr.forwarded = true
	}
	rr.held = gate.captured
	rr.content = content.String()
	rr.reasoning = reasoning.String()
	idx := make([]int, 0, len(calls))
	for i := range calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		pc := calls[i]
		if pc.name == "" {
			continue
		}
		id := pc.id
		if id == "" {
			id = "call_" + randHex(8)
		}
		args := pc.args.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		rr.toolCalls = append(rr.toolCalls, toolCall{ID: id, Type: "function", Function: toolCallFunc{Name: pc.name, Arguments: args}})
	}
	return rr, nil
}

// stripDeltas removes the delta payload from a terminal chunk whose content
// and reasoning were already relayed, keeping finish_reason, usage, timings.
func stripDeltas(m map[string]any) {
	choices, _ := m["choices"].([]any)
	for _, c := range choices {
		if cm, ok := c.(map[string]any); ok {
			cm["delta"] = map[string]any{}
		}
	}
}

// addUsage sums two usage blocks; nil means none reported.
func addUsage(a, b *usage) *usage {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	s := &usage{
		PromptTokens:     a.PromptTokens + b.PromptTokens,
		CompletionTokens: a.CompletionTokens + b.CompletionTokens,
		TotalTokens:      a.TotalTokens + b.TotalTokens,
	}
	s.RespTokPerSec = b.RespTokPerSec // the rate is per generation, not summable
	return s
}

// runToolLoopStreaming drives the loop with streamed rounds (see file doc).
func (p *Proxy) runToolLoopStreaming(w http.ResponseWriter, r *http.Request, res resolveResult, bodyMap map[string]any, messages []any, allTools []any, toolChoice any) {
	ctx := r.Context()
	out := newSSEOut(w, res.BackendModel)
	var stats toolStats
	var total *usage

	fail := func(err error) {
		p.logger.ErrorContext(ctx, "tool loop backend error (streaming)", "backend_url", res.BackendURL, "err", err)
		// In-band: the stream has begun semantically even if no byte has.
		out.raw(buildSSEChunk(out.id, out.model, sseFields{role: "assistant"}))
		out.raw(buildSSEChunk(out.id, out.model, sseFields{content: "Error: " + err.Error(), finishReason: "stop"}))
		out.raw(sseDone)
	}

	var last roundResult
	for round := 0; round < p.maxToolRounds; round++ {
		bodyMap["messages"] = messages
		bodyMap["tools"] = allTools
		bodyMap["tool_choice"] = toolChoice
		bodyMap["stream"] = true
		// Usage is needed to sum across rounds and for the router's reqlog;
		// ask for it whatever the client's stream_options said.
		so, _ := bodyMap["stream_options"].(map[string]any)
		if so == nil {
			so = map[string]any{}
		}
		so["include_usage"] = true
		bodyMap["stream_options"] = so
		body, err := json.Marshal(bodyMap)
		if err != nil {
			fail(fmt.Errorf("toolproxy: marshal loop body: %w", err))
			return
		}

		rr, err := p.streamRound(ctx, out, res.BackendURL, body)
		if err != nil {
			fail(err)
			return
		}
		total = addUsage(total, rr.usage)

		calls := rr.toolCalls
		if len(calls) == 0 && rr.held != "" {
			calls = extractToolCallsFromContent(rr.held)
			if len(calls) == 0 {
				// A literal "<tool_call>" in prose, not a call: send it on.
				out.delta(rr.held, "")
				rr.forwarded = true
			}
		}
		_, cleanContent := extractThinking(stripToolCallTags(rr.content))
		last = rr

		if len(calls) == 0 {
			final := loopResult{
				outcome: outcomeFinal, content: cleanContent, reasoning: rr.reasoning,
				finishReason: rr.finishReason, tools: stats,
				lastRaw: rawMessageInfo{finishReason: rr.finishReason, completionTokens: usageCompletionTokens(rr.usage),
					hadReasoning: rr.reasoning != "", contentLen: len(cleanContent)},
			}
			if !out.started && p.reportEmptyAnswer(ctx, w, final) {
				return
			}
			if rr.finishReason == "length" {
				p.logger.WarnContext(ctx, "streamed final answer ended at finish_reason length",
					append([]any{"backend_url", res.BackendURL}, final.lastRaw.logAttrs()...)...)
			}
			p.finishStream(out, rr, total)
			return
		}

		var proxyCalls, clientCalls []toolCall
		for _, c := range calls {
			if p.tools.Has(c.Function.Name) {
				proxyCalls = append(proxyCalls, c)
			} else {
				clientCalls = append(clientCalls, c)
			}
		}
		if len(clientCalls) > 0 {
			p.logger.InfoContext(ctx, "returning client tool calls",
				"client", len(clientCalls), "proxy", len(proxyCalls))
			// The content (if any) already went out as deltas; the breakout
			// object carries only the calls. Same framing as before: one
			// chat.completion inside the stream, then [DONE].
			content := ""
			if !rr.forwarded {
				content = cleanContent
			}
			cc := newChatCompletion(out.id, out.model, content, "", calls, total, "tool_calls")
			b, _ := json.Marshal(cc)
			out.raw("data: " + string(b) + "\n\n")
			out.raw(sseDone)
			return
		}

		p.logger.InfoContext(ctx, "executing proxy tools", "round", round+1, "count", len(proxyCalls))
		messages = append(messages, assistantToolCallMessage(cleanContent, calls))
		messages = p.executeProxyCalls(ctx, round, proxyCalls, &stats, messages)
	}

	p.logger.WarnContext(ctx, "max tool rounds reached",
		"rounds", p.maxToolRounds, "tools_executed", stats.executed, "tools_failed", stats.failed)
	if !last.forwarded {
		out.delta(maxRoundsContent(loopResult{tools: stats}), "")
	}
	out.raw(buildSSEChunk(out.id, out.model, sseFields{finishReason: "stop"}))
	if total != nil {
		out.raw(usageChunk(out.id, out.model, total))
	}
	out.raw(sseDone)
}

// finishStream replays the final round's terminal chunks with the usage summed
// over all rounds, then [DONE]. A backend that sent no terminal chunk gets a
// synthesized stop.
func (p *Proxy) finishStream(out *sseOut, rr roundResult, total *usage) {
	usageSent := false
	for _, m := range rr.terminal {
		if _, has := m["usage"]; has && total != nil {
			m["usage"] = total
			usageSent = true
		}
		b, _ := json.Marshal(m)
		out.raw("data: " + string(b) + "\n\n")
	}
	if len(rr.terminal) == 0 {
		out.raw(buildSSEChunk(out.id, out.model, sseFields{finishReason: "stop"}))
	}
	if !usageSent && total != nil {
		out.raw(usageChunk(out.id, out.model, total))
	}
	out.raw(sseDone)
}

// usageChunk is the OpenAI include_usage frame: empty choices, usage set.
func usageChunk(id, model string, u *usage) string {
	b, _ := json.Marshal(map[string]any{
		"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []any{}, "usage": u,
	})
	return "data: " + string(b) + "\n\n"
}
