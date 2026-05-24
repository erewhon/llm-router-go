package toolproxy

import (
	"encoding/json"
	"regexp"
	"strings"
)

// toolCallTagRe matches a <tool_call>{...}</tool_call> block, DOTALL so the
// JSON payload can span lines. Port of the Python proxy's _TOOL_CALL_RE.
var toolCallTagRe = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)

// extractToolCalls returns the tool calls in a message, preferring the
// backend's structured tool_calls and falling back to <tool_call> XML tags in
// the content. SGLang's --tool-call-parser (qwen3_coder for our models)
// normally populates tool_calls directly; the tag fallback covers builds or
// templates where the parser doesn't fire, matching the Python proxy's
// extract_tool_calls.
func extractToolCalls(msg chatMessage) []toolCall {
	if len(msg.ToolCalls) > 0 {
		out := make([]toolCall, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			id := tc.ID
			if id == "" {
				id = "call_" + randHex(8)
			}
			out = append(out, toolCall{
				ID:       id,
				Type:     "function",
				Function: toolCallFunc{Name: tc.Function.Name, Arguments: tc.Function.Arguments},
			})
		}
		return out
	}
	return extractToolCallsFromContent(msg.Content)
}

// extractToolCallsFromContent parses <tool_call>{"name":..,"arguments":..}</tool_call>
// blocks out of free text. arguments may be an object (re-serialised to a
// string) or already a string; entries without a name, or with unparseable
// JSON, are skipped.
func extractToolCallsFromContent(content string) []toolCall {
	matches := toolCallTagRe.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return nil
	}
	var out []toolCall
	for _, m := range matches {
		var obj struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(m[1]), &obj); err != nil {
			continue
		}
		if obj.Name == "" {
			continue
		}
		args := strings.TrimSpace(string(obj.Arguments))
		// A JSON string argument ("...") is unwrapped to its value; an object
		// or array is kept as its compact JSON form; absent becomes "{}".
		switch {
		case args == "" || args == "null":
			args = "{}"
		case strings.HasPrefix(args, `"`):
			var s string
			if json.Unmarshal(obj.Arguments, &s) == nil {
				args = s
			}
		}
		out = append(out, toolCall{
			ID:       "call_" + randHex(8),
			Type:     "function",
			Function: toolCallFunc{Name: obj.Name, Arguments: args},
		})
	}
	return out
}

// stripToolCallTags removes <tool_call>...</tool_call> blocks from content and
// trims the result, so assistant text added to the conversation history (and
// returned to clients) doesn't carry the raw tool-call markup.
func stripToolCallTags(content string) string {
	return strings.TrimSpace(toolCallTagRe.ReplaceAllString(content, ""))
}
