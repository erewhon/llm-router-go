package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRegistry_RegisterAndExecute(t *testing.T) {
	r := NewRegistry()
	r.Register(Tool{
		Name:        "echo",
		Description: "echoes its input",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"msg": map[string]any{"type": "string"},
			},
		},
		Run: func(ctx context.Context, args string) string {
			var v struct {
				Msg string `json:"msg"`
			}
			_ = json.Unmarshal([]byte(args), &v)
			return v.Msg
		},
	})

	if !r.Has("echo") {
		t.Errorf("Has(echo) = false")
	}
	if got := r.Execute(context.Background(), "echo", `{"msg":"hi"}`); got != "hi" {
		t.Errorf("Execute returned %q, want hi", got)
	}
}

func TestRegistry_UnknownTool(t *testing.T) {
	r := NewRegistry()
	got := r.Execute(context.Background(), "missing", "{}")
	if !strings.Contains(got, "Unknown tool") {
		t.Errorf("Execute on missing tool: %q", got)
	}
}

func TestRegistry_InvalidJSON(t *testing.T) {
	r := NewRegistry()
	r.Register(Tool{
		Name: "noop",
		Run: func(ctx context.Context, args string) string {
			t.Fatalf("Run should not be invoked when args fail JSON parse")
			return ""
		},
	})
	got := r.Execute(context.Background(), "noop", "{not json")
	if !strings.Contains(got, "Invalid arguments") {
		t.Errorf("Execute with bad JSON: %q", got)
	}
}

func TestRegistry_DefinitionsShape(t *testing.T) {
	r := NewRegistry()
	r.Register(Tool{
		Name:        "echo",
		Description: "d",
		Parameters:  map[string]any{"type": "object"},
		Run:         func(ctx context.Context, _ string) string { return "" },
	})
	defs := r.Definitions()
	if len(defs) != 1 {
		t.Fatalf("Definitions len = %d, want 1", len(defs))
	}
	if defs[0]["type"] != "function" {
		t.Errorf("type = %v, want function", defs[0]["type"])
	}
	fn, ok := defs[0]["function"].(map[string]any)
	if !ok {
		t.Fatalf("function key not a map: %T", defs[0]["function"])
	}
	if fn["name"] != "echo" || fn["description"] != "d" {
		t.Errorf("function = %+v", fn)
	}
}

func TestRegistry_Names_PreservesOrder(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"alpha", "bravo", "charlie"} {
		r.Register(Tool{Name: n, Run: func(ctx context.Context, _ string) string { return "" }})
	}
	got := r.Names()
	want := []string{"alpha", "bravo", "charlie"}
	if len(got) != len(want) {
		t.Fatalf("Names len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Names[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRegistry_RegisterDuplicateOverwrites(t *testing.T) {
	r := NewRegistry()
	r.Register(Tool{Name: "x", Run: func(context.Context, string) string { return "first" }})
	r.Register(Tool{Name: "x", Run: func(context.Context, string) string { return "second" }})
	if got := r.Execute(context.Background(), "x", "{}"); got != "second" {
		t.Errorf("after re-register: %q, want second", got)
	}
	// Order list should still only have "x" once.
	if names := r.Names(); len(names) != 1 || names[0] != "x" {
		t.Errorf("Names = %v, want [x]", names)
	}
}
