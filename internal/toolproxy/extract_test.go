package toolproxy

import "testing"

func TestExtractToolCalls_Native(t *testing.T) {
	msg := chatMessage{
		ToolCalls: []toolCall{
			{ID: "call_1", Type: "function", Function: toolCallFunc{Name: "calculator", Arguments: `{"expression":"2+2"}`}},
			{ID: "call_2", Type: "function", Function: toolCallFunc{Name: "web_search", Arguments: `{"query":"x"}`}},
		},
	}
	got := extractToolCalls(msg)
	if len(got) != 2 {
		t.Fatalf("got %d calls, want 2", len(got))
	}
	if got[0].ID != "call_1" || got[0].Function.Name != "calculator" {
		t.Errorf("call[0] = %+v", got[0])
	}
	if got[1].Function.Arguments != `{"query":"x"}` {
		t.Errorf("call[1] args = %q", got[1].Function.Arguments)
	}
}

func TestExtractToolCalls_NativeBlankIDGetsGenerated(t *testing.T) {
	msg := chatMessage{
		ToolCalls: []toolCall{{Function: toolCallFunc{Name: "calculator", Arguments: "{}"}}},
	}
	got := extractToolCalls(msg)
	if len(got) != 1 {
		t.Fatalf("got %d calls, want 1", len(got))
	}
	if got[0].ID == "" {
		t.Error("blank ID was not replaced")
	}
	if got[0].Type != "function" {
		t.Errorf("type = %q, want function", got[0].Type)
	}
}

func TestExtractToolCalls_TagFallbackObjectArgs(t *testing.T) {
	msg := chatMessage{Content: `sure<tool_call>{"name": "calculator", "arguments": {"expression":"2+2"}}</tool_call>`}
	got := extractToolCalls(msg)
	if len(got) != 1 {
		t.Fatalf("got %d calls, want 1", len(got))
	}
	if got[0].Function.Name != "calculator" {
		t.Errorf("name = %q", got[0].Function.Name)
	}
	if got[0].Function.Arguments != `{"expression":"2+2"}` {
		t.Errorf("arguments = %q, want object JSON", got[0].Function.Arguments)
	}
	if got[0].ID == "" || got[0].Type != "function" {
		t.Errorf("call = %+v", got[0])
	}
}

func TestExtractToolCalls_TagFallbackStringArgs(t *testing.T) {
	// A string-valued arguments field is unwrapped to its raw value, matching
	// the Python proxy's str(args) path.
	msg := chatMessage{Content: `<tool_call>{"name":"calculator","arguments":"2+2"}</tool_call>`}
	got := extractToolCalls(msg)
	if len(got) != 1 {
		t.Fatalf("got %d calls, want 1", len(got))
	}
	if got[0].Function.Arguments != "2+2" {
		t.Errorf("arguments = %q, want unwrapped string", got[0].Function.Arguments)
	}
}

func TestExtractToolCalls_TagFallbackMissingArgs(t *testing.T) {
	msg := chatMessage{Content: `<tool_call>{"name":"calculator"}</tool_call>`}
	got := extractToolCalls(msg)
	if len(got) != 1 || got[0].Function.Arguments != "{}" {
		t.Fatalf("got %+v, want one call with arguments {}", got)
	}
}

func TestExtractToolCalls_TagSkipsBadEntries(t *testing.T) {
	// First block is invalid JSON, second has no name; both skipped.
	msg := chatMessage{Content: `<tool_call>not json</tool_call><tool_call>{"arguments":{}}</tool_call>`}
	if got := extractToolCalls(msg); len(got) != 0 {
		t.Errorf("got %d calls, want 0", len(got))
	}
}

func TestExtractToolCalls_NativeWinsOverTags(t *testing.T) {
	msg := chatMessage{
		Content:   `<tool_call>{"name":"ignored","arguments":{}}</tool_call>`,
		ToolCalls: []toolCall{{ID: "call_1", Function: toolCallFunc{Name: "calculator", Arguments: "{}"}}},
	}
	got := extractToolCalls(msg)
	if len(got) != 1 || got[0].Function.Name != "calculator" {
		t.Errorf("got %+v, want only the native calculator call", got)
	}
}

func TestStripToolCallTags(t *testing.T) {
	cases := map[string]string{
		`hello<tool_call>{"name":"x"}</tool_call>world`: "helloworld",
		`  <tool_call>{"name":"x"}</tool_call>  `:       "",
		"plain text": "plain text",
		"a<tool_call>1</tool_call>b<tool_call>2</tool_call>c": "abc",
	}
	for in, want := range cases {
		if got := stripToolCallTags(in); got != want {
			t.Errorf("stripToolCallTags(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractThinking(t *testing.T) {
	cases := []struct {
		in, wantReason, wantClean string
	}{
		{"<think>plan</think>answer", "plan", "answer"},
		{"no tags here", "", "no tags here"},
		{"<think>a</think>mid<think>b</think>end", "a\n\nb", "midend"},
		{"<think>\n  deep thought  \n</think>\n\nThe answer", "deep thought", "The answer"},
		{"leading</think>tail", "", "leadingtail"}, // orphan close stripped (Python parity)
		{"", "", ""},
	}
	for _, c := range cases {
		gotR, gotC := extractThinking(c.in)
		if gotR != c.wantReason || gotC != c.wantClean {
			t.Errorf("extractThinking(%q) = (%q,%q), want (%q,%q)", c.in, gotR, gotC, c.wantReason, c.wantClean)
		}
	}
}

func TestReasoningText_PrefersReasoning(t *testing.T) {
	if got := (chatMessage{Reasoning: "a", ReasoningContent: "b"}).reasoningText(); got != "a" {
		t.Errorf("got %q, want a (reasoning preferred)", got)
	}
	if got := (chatMessage{ReasoningContent: "b"}).reasoningText(); got != "b" {
		t.Errorf("got %q, want b (reasoning_content fallback)", got)
	}
	if got := (chatMessage{}).reasoningText(); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
