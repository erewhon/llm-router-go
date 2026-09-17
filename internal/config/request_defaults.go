package config

import (
	"fmt"
	"sort"
)

// reservedRequestFields are body fields request_defaults may never set. The
// caller owns the conversation (messages / prompt / input) and the response
// shape (stream); the router owns "model" and overwrites it on every attempt.
// A default that flipped any of these would change what the client asked for
// rather than fill in what it left unsaid.
var reservedRequestFields = []string{"model", "messages", "stream", "prompt", "input"}

// validateRequestDefaults rejects reserved keys. where names the entry for the
// error ("model \"x\"" or "model \"x\" alias_overrides[\"y\"]").
func validateRequestDefaults(where string, d map[string]any) error {
	for _, k := range reservedRequestFields {
		if _, ok := d[k]; ok {
			return fmt.Errorf("%s: request_defaults may not set %q — the caller or the router owns that field", where, k)
		}
	}
	return nil
}

// RequestDefaultsFor returns the request-body defaults for a request that
// reached this model through alias (or "" for a direct / role / chain match).
// Precedence, most specific first: the alias override's request_defaults,
// the alias override's chat_template_kwargs shorthand (folded in under the
// "chat_template_kwargs" key), then the model's request_defaults. Nested
// objects merge key by key, so an alias that sets
// chat_template_kwargs.enable_thinking keeps the model's other kwargs.
//
// The result is a fresh deep copy on every call — callers may hand it to
// ApplyRequestDefaults and forget about it. Nil when nothing is configured.
func (m ModelDefinition) RequestDefaultsFor(alias string) map[string]any {
	var out map[string]any
	if len(m.RequestDefaults) > 0 {
		out = ApplyRequestDefaults(nil, m.RequestDefaults)
	}
	if alias == "" {
		return out
	}
	ov, ok := m.AliasOverrides[alias]
	if !ok {
		return out
	}
	over := ov.RequestDefaults
	if len(ov.ChatTemplateKwargs) > 0 {
		// Shorthand: chat_template_kwargs on an alias override is the same
		// as request_defaults: {chat_template_kwargs: {...}}, and an explicit
		// request_defaults.chat_template_kwargs on the same override wins
		// key by key.
		over = ApplyRequestDefaults(over, map[string]any{"chat_template_kwargs": ov.ChatTemplateKwargs})
	}
	if len(over) == 0 {
		return out
	}
	// Alias-level keys win: the model's defaults only fill what the alias
	// left unsaid.
	return ApplyRequestDefaults(over, out)
}

// ApplyRequestDefaults returns a copy of body with every field of defaults
// that the body does not already carry filled in. It is FILL-ONLY: a field the
// caller sent always wins, at every nesting level — where both sides hold an
// object the two merge key by key (caller keys win, default keys fill), and
// any other collision keeps the caller's value untouched. Neither input is
// mutated, and default values are deep-copied so a later edit of the
// forwarded body cannot reach back into the registry.
//
// A nil body with defaults yields a deep copy of the defaults; a nil defaults
// yields a shallow copy of the body.
func ApplyRequestDefaults(body, defaults map[string]any) map[string]any {
	out := make(map[string]any, len(body)+len(defaults))
	for k, v := range body {
		out[k] = v
	}
	for k, dv := range defaults {
		bv, present := out[k]
		if !present {
			out[k] = deepCopyValue(dv)
			continue
		}
		bm, bodyIsObject := bv.(map[string]any)
		dm, defaultIsObject := dv.(map[string]any)
		if bodyIsObject && defaultIsObject {
			out[k] = ApplyRequestDefaults(bm, dm)
		}
		// Anything else: the caller's value stands.
	}
	return out
}

// AppliedRequestDefaultKeys lists, sorted, the top-level default keys that
// ApplyRequestDefaults(body, defaults) would add or merge into — i.e. the
// keys the caller did not fully specify. For the forwarding log line.
func AppliedRequestDefaultKeys(body, defaults map[string]any) []string {
	var keys []string
	for k, dv := range defaults {
		bv, present := body[k]
		if !present {
			keys = append(keys, k)
			continue
		}
		bm, bodyIsObject := bv.(map[string]any)
		dm, defaultIsObject := dv.(map[string]any)
		if bodyIsObject && defaultIsObject && len(AppliedRequestDefaultKeys(bm, dm)) > 0 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// deepCopyValue copies the map / slice structure yaml and json produce so a
// default handed to a request body is never shared with the registry.
func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = deepCopyValue(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopyValue(e)
		}
		return out
	default:
		return v
	}
}
