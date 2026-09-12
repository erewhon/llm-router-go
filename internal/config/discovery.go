package config

// Discovery: the models.yaml `discovery:` block.
//
// A hand-written models.yaml entry says what the operator believes a provider
// serves. The live inventory (internal/health/inventory.go) asks the provider
// itself, and this block is the policy for what to do with ids the operator
// never wrote down: which providers to watch, under what prefix an adopted id
// is exposed, and which ids are worth adopting at all.
//
// The adopt policy exists because catalogues differ by two orders of
// magnitude. OpenCode Zen lists a few dozen ids, all of them on the account,
// so "adopt everything" is the right default there — when Zen turns on a new
// model it should appear on the router without anyone editing YAML.
// OpenRouter lists several hundred, most of which nobody here will ever call;
// adopting those wholesale would bury the catalogue and the OpenCode
// well-known under noise, so that source takes an allowlist of patterns.
//
// What discovery never does is touch a role or a chain. A discovered id is
// routable by name, listed, and priced from the provider's own metadata when
// it offers any; seat decisions — what "coder" means, which providers back
// "kimi-k3" — stay explicit in models.yaml. And a hand-written entry with the
// same id always wins over a discovered one, so pinned pricing, aliases and
// chains stay authoritative even while the provider lists the id too.

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// DiscoverySource is one provider whose live listing the router watches.
type DiscoverySource struct {
	// APIBase is the OpenAI-compatible base the listing is fetched from
	// (GET <api_base>/models, with a trailing "/v1" tolerated either way) and
	// the base every adopted entry routes to.
	APIBase string `yaml:"api_base"`
	// Prefix is prepended to every adopted provider id to form the registry
	// id ("zen/" + "claude-opus-5" → "zen/claude-opus-5"). Required, and
	// unique across sources: it is what keeps two providers that both list
	// "claude-opus-5" from colliding, and what keeps a discovered id out of
	// the bare namespace roles and aliases live in.
	Prefix string `yaml:"prefix"`
	// APIKey / APIKeyHeader follow the model-entry convention: a literal
	// "sk-" key, or the name of an environment variable holding one; and
	// optionally the header to carry it in instead of Authorization: Bearer.
	APIKey       string `yaml:"api_key,omitempty"`
	APIKeyHeader string `yaml:"api_key_header,omitempty"`
	// Adopt decides which listed ids become entries: `all`, or a list of
	// glob patterns — `*` matches any run of characters (slashes included: a
	// provider id is an opaque string, not a path), `?` one character,
	// `[a-z]` a class. "deepseek/*" matches "deepseek/deepseek-v4-pro",
	// "anthropic/claude-*" every Claude id, "*:batch" every batch tier.
	Adopt AdoptPolicy `yaml:"adopt"`
	// Exclude vetoes ids Adopt accepted, same glob syntax. A catalogue
	// lists variants nobody routes to interactively — OpenRouter's ":batch"
	// ids, "-exp" previews, dated snapshots — and a glob cannot say "not":
	// this is where that goes.
	Exclude []string `yaml:"exclude,omitempty"`
	// Tags are added to every adopted entry alongside the automatic
	// "discovered" tag — the provider tag ("zen", "openrouter") the dashboard
	// filters on, typically.
	Tags []string `yaml:"tags,omitempty"`
	// ContextLength and MaxOutputTokens are the defaults for adopted entries
	// when the provider's listing carries neither (Zen's does not). Zero
	// leaves them unset, i.e. the well-known's endpoint default applies.
	ContextLength   int `yaml:"context_length,omitempty"`
	MaxOutputTokens int `yaml:"max_output_tokens,omitempty"`
}

// AdoptPolicy is the `adopt:` field: the scalar `all`, or a list of patterns.
type AdoptPolicy struct {
	All      bool
	Patterns []string
}

// UnmarshalYAML accepts `adopt: all` or `adopt: [pattern, ...]`.
func (p *AdoptPolicy) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if strings.EqualFold(strings.TrimSpace(node.Value), "all") {
			*p = AdoptPolicy{All: true}
			return nil
		}
		return fmt.Errorf("adopt: want `all` or a list of patterns, got %q", node.Value)
	case yaml.SequenceNode:
		var pats []string
		if err := node.Decode(&pats); err != nil {
			return err
		}
		*p = AdoptPolicy{Patterns: pats}
		return nil
	}
	return fmt.Errorf("adopt: want `all` or a list of patterns")
}

// MarshalYAML renders the policy back the way it was written.
func (p AdoptPolicy) MarshalYAML() (any, error) {
	if p.All {
		return "all", nil
	}
	return p.Patterns, nil
}

// Matches reports whether a provider id is adopted under this policy.
func (p AdoptPolicy) Matches(id string) bool {
	if p.All {
		return true
	}
	for _, pat := range p.Patterns {
		if GlobMatch(pat, id) {
			return true
		}
	}
	return false
}

// Adopts reports whether a listed id becomes an entry under this source:
// accepted by Adopt and vetoed by no Exclude pattern.
func (s DiscoverySource) Adopts(id string) bool {
	if !s.Adopt.Matches(id) {
		return false
	}
	for _, pat := range s.Exclude {
		if GlobMatch(pat, id) {
			return false
		}
	}
	return true
}

// GlobMatch is the pattern language of adopt/exclude: `*` any run of
// characters including "/", `?` one character, `[...]` a character class,
// everything else literal; anchored at both ends. Not path.Match, whose `*`
// stops at a slash — that would make "*:batch" unable to match
// "deepseek/deepseek-v4-pro:batch", and every provider id has a slash.
// A pattern that does not compile matches nothing (validation reports it).
func GlobMatch(pattern, id string) bool {
	re, err := compileGlob(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(id)
}

var globCache sync.Map // pattern → *regexp.Regexp

func compileGlob(pattern string) (*regexp.Regexp, error) {
	if re, ok := globCache.Load(pattern); ok {
		return re.(*regexp.Regexp), nil
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '[':
			end := strings.IndexByte(pattern[i:], ']')
			if end < 0 {
				return nil, fmt.Errorf("glob %q: unterminated character class", pattern)
			}
			class := pattern[i : i+end+1]
			if strings.HasPrefix(class, "[!") {
				class = "[^" + class[2:]
			}
			b.WriteString(class)
			i += end
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("glob %q: %v", pattern, err)
	}
	globCache.Store(pattern, re)
	return re, nil
}

// Root returns the source's api_base with any trailing "/v1" removed — the
// same normalisation the router applies to a model's api_base before it
// appends a request path, so a source written with or without "/v1" lands on
// the same endpoints.
func (s DiscoverySource) Root() string {
	return strings.TrimSuffix(strings.TrimSuffix(s.APIBase, "/"), "/v1")
}

// ListedModel is what a provider's listing said about one id, reduced to the
// fields a discovered entry can use. Zero values mean "the provider did not
// say".
type ListedModel struct {
	ID string
	// ContextLength is the provider's advertised window (OpenRouter's
	// context_length). Zero when absent.
	ContextLength int
	// MaxOutputTokens is the provider's completion cap (OpenRouter's
	// top_provider.max_completion_tokens). Zero when absent.
	MaxOutputTokens int
	// InputCostPerMillion / OutputCostPerMillion are the provider's prices,
	// already scaled to per-million tokens. Nil when the listing carries no
	// pricing at all; a non-nil zero is a real free tier.
	InputCostPerMillion  *float64
	OutputCostPerMillion *float64
	// ToolCalling / Vision come from OpenRouter's supported_parameters and
	// architecture.input_modalities; false when the listing says nothing.
	ToolCalling bool
	Vision      bool
}

// Entry builds the virtual external entry for one adopted id. The result is
// a complete, enabled, chat-class ModelDefinition routable directly by name
// (its Backend is external and its APIBase is the source's), carrying the
// provider's metadata where it gave any and the source's defaults otherwise.
// Capabilities always include text; tool_calling and vision are added only on
// the provider's word — a discovered entry never claims more than the
// listing supports, because a role's require.capabilities would trust it.
func (s DiscoverySource) Entry(lm ListedModel) ModelDefinition {
	m := ModelDefinition{
		HFRepo:       lm.ID,
		Backend:      BackendExternal,
		Enabled:      true,
		AlwaysOn:     true,
		APIBase:      s.APIBase,
		APIKey:       s.APIKey,
		APIKeyHeader: s.APIKeyHeader,
		APIClass:     APIClassChat,
		Capabilities: []ModelCapability{CapText},
	}
	if lm.ToolCalling {
		m.Capabilities = append(m.Capabilities, CapToolCalling)
	}
	if lm.Vision {
		m.Capabilities = append(m.Capabilities, CapVision)
	}
	m.ContextLength = lm.ContextLength
	if m.ContextLength == 0 {
		m.ContextLength = s.ContextLength
	}
	m.MaxOutputTokens = lm.MaxOutputTokens
	if m.MaxOutputTokens == 0 {
		m.MaxOutputTokens = s.MaxOutputTokens
	}
	m.InputCostPerMillion = lm.InputCostPerMillion
	m.OutputCostPerMillion = lm.OutputCostPerMillion

	// Tags: the automatic marker first, then the source's own. "paid" is
	// added unless the provider priced the id at exactly zero both ways —
	// unknown pricing is assumed to cost money, never assumed free.
	tags := []string{TagDiscovered}
	free := lm.InputCostPerMillion != nil && lm.OutputCostPerMillion != nil &&
		*lm.InputCostPerMillion == 0 && *lm.OutputCostPerMillion == 0
	if !free {
		tags = append(tags, "paid")
	}
	for _, t := range s.Tags {
		if t != TagDiscovered && !containsString(tags, t) {
			tags = append(tags, t)
		}
	}
	m.Tags = tags
	return m
}

// TagDiscovered marks an entry the inventory adopted rather than one the
// operator wrote. The dashboard's filter chips and reqlog's `discovered`
// column key off the same fact.
const TagDiscovered = "discovered"

// IsDiscovered reports whether an entry carries the discovery marker.
func (m ModelDefinition) IsDiscovered() bool {
	return containsString(m.Tags, TagDiscovered)
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// validateDiscovery enforces the source contract at load time. Everything
// here is a hard error: a source with no prefix would adopt ids into the bare
// namespace, and a bad glob would silently adopt nothing.
func (r *ModelRegistry) validateDiscovery() error {
	var errs []error
	seenPrefix := map[string]int{}
	for i, s := range r.Discovery {
		subject := fmt.Sprintf("discovery[%d]", i)
		if s.APIBase != "" {
			subject = fmt.Sprintf("discovery[%d] (%s)", i, s.APIBase)
		}
		if s.APIBase == "" {
			errs = append(errs, fmt.Errorf("%s: api_base is required", subject))
		} else if u, err := url.Parse(s.APIBase); err != nil || u.Scheme == "" || u.Host == "" {
			errs = append(errs, fmt.Errorf("%s: api_base must be an absolute URL", subject))
		}
		if s.Prefix == "" {
			errs = append(errs, fmt.Errorf("%s: prefix is required (adopted ids must not land in the bare namespace)", subject))
		} else if j, dup := seenPrefix[s.Prefix]; dup {
			errs = append(errs, fmt.Errorf("%s: prefix %q is already used by discovery[%d]", subject, s.Prefix, j))
		} else {
			seenPrefix[s.Prefix] = i
		}
		if !s.Adopt.All && len(s.Adopt.Patterns) == 0 {
			errs = append(errs, fmt.Errorf("%s: adopt must be `all` or a non-empty list of patterns", subject))
		}
		for _, pat := range s.Adopt.Patterns {
			if _, err := compileGlob(pat); err != nil {
				errs = append(errs, fmt.Errorf("%s: adopt pattern %v", subject, err))
			}
		}
		for _, pat := range s.Exclude {
			if _, err := compileGlob(pat); err != nil {
				errs = append(errs, fmt.Errorf("%s: exclude pattern %v", subject, err))
			}
		}
	}
	return errors.Join(errs...)
}

// ReservedNames returns every name a discovered id must not shadow: model
// ids, aliases and role names. Sorted, for deterministic diagnostics.
func (r *ModelRegistry) ReservedNames() []string {
	seen := map[string]struct{}{}
	for id, m := range r.Models {
		seen[id] = struct{}{}
		for _, a := range m.Aliases {
			seen[a] = struct{}{}
		}
	}
	for name := range r.Roles {
		seen[name] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
