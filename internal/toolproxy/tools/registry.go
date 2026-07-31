// Package tools holds the registry of tools the tool proxy can execute
// on behalf of a model. Each tool exposes an OpenAI function-style
// definition (used to inform the model of available tools) and a Run
// function that consumes the model's chosen arguments and returns a
// string result for the next assistant turn.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Tool is a single callable a model may invoke.
type Tool struct {
	// Name is the function name the model sees and emits in tool_calls.
	Name string
	// Description is shown to the model as part of the tool list.
	Description string
	// Parameters is the JSON-schema description of the arguments,
	// matching OpenAI's "function.parameters" shape.
	Parameters map[string]any
	// Run executes the tool. arguments is the raw JSON string the model
	// emitted (the "function.arguments" field). The return value is what
	// the next turn's "tool" message will contain.
	Run func(ctx context.Context, arguments string) string
}

// Registry holds the tool set available to a proxy instance.
type Registry struct {
	tools map[string]Tool
	order []string // preserve registration order for deterministic Definitions()
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// Register adds a tool. A duplicate name overwrites the previous entry.
func (r *Registry) Register(t Tool) {
	if _, dup := r.tools[t.Name]; !dup {
		r.order = append(r.order, t.Name)
	}
	r.tools[t.Name] = t
}

// Names returns registered tool names in registration order.
func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Has reports whether a tool with the given name is registered.
func (r *Registry) Has(name string) bool {
	_, ok := r.tools[name]
	return ok
}

// Definitions returns the OpenAI-format `tools` array suitable for the
// chat-completions request body.
func (r *Registry) Definitions() []map[string]any {
	out := make([]map[string]any, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			},
		})
	}
	return out
}

// failurePrefixes are the leading strings every tool uses to report that it
// could not do its job. Tools return their errors in-band as the `tool`
// message content (the model needs to read them), so this is the one place
// that knows how to tell a failure from a real result.
//
// String matching is admittedly brittle: a structured signal (Run returning
// (string, error)) would be better, but it changes the Tool contract every
// tool and test depends on. Centralising the prefixes here at least keeps the
// knowledge in one auditable list — keep it in sync when adding a tool.
var failurePrefixes = []string{
	"Search failed:",
	"Fetch failed:",
	"Tavily search failed:",
	"Invalid arguments:",
	"Invalid expression:",
	"Unknown tool:",
	"Non-text content type:",
	"Could not extract text content from:",
}

// IsFailure reports whether a tool's output is one of its error replies rather
// than a usable result. "No results found." counts: an empty result set is not
// a transport error, but for a research agent it is indistinguishable from one
// and is exactly what a dead egress produces.
func IsFailure(output string) bool {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" || trimmed == "No results found." {
		return true
	}
	for _, p := range failurePrefixes {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}
	return false
}

// Execute runs a registered tool. arguments is the raw JSON string the
// model emitted. The return value is the tool's reply (always a string,
// possibly an error message).
func (r *Registry) Execute(ctx context.Context, name, arguments string) string {
	t, ok := r.tools[name]
	if !ok {
		return fmt.Sprintf("Unknown tool: %s", name)
	}
	// Sanity-check the JSON so misbehaved tool runtimes don't see garbage.
	if arguments != "" {
		var probe any
		if err := json.Unmarshal([]byte(arguments), &probe); err != nil {
			return fmt.Sprintf("Invalid arguments: %s", arguments)
		}
	}
	return t.Run(ctx, arguments)
}
