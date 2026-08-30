package config

import (
	"fmt"
	"sort"
	"strings"
)

// Lint is the ADVISORY layer over a registry. It is deliberately separate from
// Validate, and the separation is a safety property, not a style choice:
//
//	Validate() is on the boot path. Every Go binary calls it via Load, and an
//	error there means the process refuses to start. Its contract must stay
//	exactly as strict as it is today and no stricter.
//
//	Lint() has NO caller on the boot path. It is reachable only from
//	`llm-router-go --validate`. A lint finding therefore cannot prevent a
//	binary from starting, no matter how severe it looks.
//
// That is what makes it safe to ship checks which already fire against the
// live models.yaml. As of 2026-08-06 the production file trips dup-alias,
// alias-shadows-model, role-empty-in-mode and route-category-unresolvable —
// all benign today, all worth surfacing, none of them worth an outage. If
// these were folded into Validate the fleet would stop booting.
//
// Promote findings to failures at the CLI boundary (--validate-strict /
// --validate-block), never here.

// Severity ranks a Diagnostic. It has no effect on process startup; see the
// package note above.
type Severity string

const (
	// SevWarn is an advisory finding: the config loads and the router will
	// serve it, but something is ambiguous, unreachable, or one edit away
	// from breaking.
	SevWarn Severity = "warning"
	// SevError marks a finding severe enough that the current behaviour is
	// already nondeterministic or wrong. Still advisory — still cannot stop
	// a boot — but callers should treat it as a failure by default.
	SevError Severity = "error"
)

// Diagnostic is one lint finding.
type Diagnostic struct {
	Severity Severity `json:"severity"`
	// Code is stable and greppable so scripts can promote or ignore specific
	// findings (--validate-block=enabled-port-collision). Never reword these
	// without treating it as a breaking change to the CLI contract.
	Code    string `json:"code"`
	Subject string `json:"subject"`
	Mode    string `json:"mode,omitempty"`
	Message string `json:"message"`
}

// Lint codes. Stable identifiers — see Diagnostic.Code.
const (
	LintEnabledPortCollision    = "enabled-port-collision"
	LintDupAlias                = "dup-alias"
	LintAliasShadowsModel       = "alias-shadows-model"
	LintDupHFRepo               = "dup-hf-repo"
	LintRoleEmptyInMode         = "role-empty-in-mode"
	LintRouteCategoryUnresolved = "route-category-unresolvable"
)

// ToolProxyRouteCategories mirrors the categories the tool proxy's auto-router
// advertises (internal/toolproxy/autorouter.go, var routeCategories).
//
// It is duplicated rather than imported because internal/toolproxy already
// imports internal/config, so importing back would be a cycle — and dragging
// the toolproxy package (and its gval / x/net/html dependencies) into the
// router binary for five strings is not worth it. The duplication is held
// honest by TestRouteCategoriesMatchConfig in the toolproxy package, which
// fails the build if the two lists drift.
var ToolProxyRouteCategories = []string{
	"coder",
	"thinker",
	"research",
	"vision",
}

// Lint returns advisory findings for the registry as it would behave in the
// given mode. Pass "" for no mode filtering.
//
// Findings are returned sorted by (code, subject) so output is stable across
// runs — map iteration order must never leak into a diff an operator reads.
func (r *ModelRegistry) Lint(mode string) []Diagnostic {
	var out []Diagnostic
	out = append(out, r.lintPortCollisions()...)
	out = append(out, r.lintNameCollisions()...)
	out = append(out, r.lintRolesForMode(mode)...)
	out = append(out, r.lintRouteCategories(mode)...)

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Code != out[j].Code {
			return out[i].Code < out[j].Code
		}
		return out[i].Subject < out[j].Subject
	})
	return out
}

// lintPortCollisions finds enabled models that would listen on the same
// node+port.
//
// External-backend models are EXCLUDED. They carry an explicit api_base and
// never consult api_port, so comparing them by the api_port default (5391)
// produces pure noise: on the live file that alone would report three
// collisions among delphi's creative services, euclid's embed/rerank pair and
// hypatia's TTS, none of which are real. validateModel already returns early
// for external for the same reason.
//
// This check currently fires on nothing — which is the point. archimedes:5391
// has seven entries with six disabled, the "one slot per node, swap the
// resident" pattern. Flipping `enabled: true` on the wrong one is a single
// keystroke, and nothing else in the system would notice.
func (r *ModelRegistry) lintPortCollisions() []Diagnostic {
	type slot struct {
		node string
		port int
	}
	claims := map[slot][]string{}
	for id, m := range r.Models {
		if !m.Enabled || m.Backend == BackendExternal || m.Node == "" {
			continue
		}
		port := m.APIPort
		if port == 0 {
			port = DefaultAPIPort
		}
		s := slot{m.Node, port}
		claims[s] = append(claims[s], id)
	}

	var out []Diagnostic
	for s, ids := range claims {
		if len(ids) < 2 {
			continue
		}
		sort.Strings(ids)
		out = append(out, Diagnostic{
			Severity: SevError,
			Code:     LintEnabledPortCollision,
			Subject:  fmt.Sprintf("%s:%d", s.node, s.port),
			Message: fmt.Sprintf(
				"%d enabled models claim this address (%s); at most one can be serving, so the others are unreachable",
				len(ids), strings.Join(ids, ", ")),
		})
	}
	return out
}

// lintNameCollisions covers the three ways two entries can fight over one
// routable name. Each is scored by how many of the owners are ENABLED, because
// that is what decides whether the ambiguity is live: the router only resolves
// against enabled models, so a name shared with a disabled rollback entry is
// harmless until someone re-enables it.
func (r *ModelRegistry) lintNameCollisions() []Diagnostic {
	var out []Diagnostic

	// --- duplicate aliases -------------------------------------------------
	aliasOwners := map[string][]lintOwner{}
	for id, m := range r.Models {
		for _, a := range m.Aliases {
			aliasOwners[a] = append(aliasOwners[a], lintOwner{id, m.Enabled})
		}
	}
	for alias, owners := range aliasOwners {
		if len(owners) < 2 {
			continue
		}
		sort.Slice(owners, func(i, j int) bool { return owners[i].id < owners[j].id })
		enabled := 0
		for _, o := range owners {
			if o.enabled {
				enabled++
			}
		}
		sev, note := SevWarn, "only one is enabled, so resolution is unambiguous today — re-enabling the other makes it nondeterministic"
		if enabled > 1 {
			// The router's lookupAlias iterates a map, so which model wins
			// is decided by map iteration order and can differ per restart.
			sev = SevError
			note = "MORE THAN ONE IS ENABLED: alias resolution iterates a map, so which model answers can change on every restart"
		}
		out = append(out, Diagnostic{
			Severity: sev,
			Code:     LintDupAlias,
			Subject:  alias,
			Message:  fmt.Sprintf("alias declared on %s; %s", describeOwners(owners), note),
		})
	}

	// --- alias shadowing a model key ---------------------------------------
	// Concrete-name lookup runs before alias lookup, so whenever the shadowed
	// model is active the alias is simply dead. When it is disabled the alias
	// works — which means enabling that model silently steals the name.
	for id, m := range r.Models {
		for _, a := range m.Aliases {
			shadowed, ok := r.Models[a]
			if !ok || a == id {
				continue
			}
			state := "disabled today, so the alias works — enabling it silently steals the name"
			sev := SevWarn
			if shadowed.Enabled {
				state = "ENABLED, so this alias is already dead: concrete-name lookup wins"
				sev = SevError
			}
			out = append(out, Diagnostic{
				Severity: sev,
				Code:     LintAliasShadowsModel,
				Subject:  a,
				Message: fmt.Sprintf(
					"alias of %q collides with the registry key of model %q, which is %s", id, a, state),
			})
		}
	}

	// --- duplicate hf_repo among enabled models ----------------------------
	repoOwners := map[string][]lintOwner{}
	for id, m := range r.Models {
		if m.HFRepo == "" {
			continue
		}
		repoOwners[m.HFRepo] = append(repoOwners[m.HFRepo], lintOwner{id, m.Enabled})
	}
	for repo, owners := range repoOwners {
		enabled := 0
		keyMatchesRepo := false
		for _, o := range owners {
			if o.enabled {
				enabled++
			}
			if o.id == repo {
				keyMatchesRepo = true
			}
		}
		if enabled < 2 {
			continue
		}
		// When one owner's registry KEY equals the shared repo string, exact
		// key lookup wins before any hf_repo scan, so requests naming this
		// string resolve deterministically. This is the normalized-external
		// pattern: the bare chain entry (key kimi-k3, hf_repo kimi-k3) shares
		// its hf_repo with zen/kimi-k3, by design.
		if keyMatchesRepo {
			continue
		}
		sort.Slice(owners, func(i, j int) bool { return owners[i].id < owners[j].id })
		out = append(out, Diagnostic{
			Severity: SevError,
			Code:     LintDupHFRepo,
			Subject:  repo,
			Message: fmt.Sprintf(
				"hf_repo shared by %s; concrete-name lookup iterates a map, so requests naming this repo resolve nondeterministically",
				describeOwners(owners)),
		})
	}

	return out
}

// lintRolesForMode catches roles that survive Validate but vanish at runtime.
//
// Validate only guarantees a role has >=1 candidate in the FILE. RolesForMode
// then drops candidates that are disabled or tagged for another mode, and
// drops the role entirely if nothing survives. A role can therefore be
// perfectly valid and still be silently absent from /v1/models and
// /v1/availability — which is what `coder-fim` does today, both of its
// candidates being disabled.
func (r *ModelRegistry) lintRolesForMode(mode string) []Diagnostic {
	live := r.RolesForMode(mode)

	var out []Diagnostic
	for name, role := range r.Roles {
		if _, ok := live[name]; ok {
			continue
		}
		all := append(append([]string{}, role.Candidates...), role.Overflow...)
		sort.Strings(all)
		out = append(out, Diagnostic{
			Severity: SevWarn,
			Code:     LintRoleEmptyInMode,
			Subject:  name,
			Mode:     mode,
			Message: fmt.Sprintf(
				"every candidate is disabled or tagged for another mode (%s); the role will NOT appear in /v1/models or /v1/availability",
				strings.Join(all, ", ")),
		})
	}
	return out
}

// lintRouteCategories checks that everything the tool proxy's auto-router can
// classify a prompt into still resolves to something routable.
//
// The auto-router fails open: it will happily route to a category that no
// longer exists, and the request dies as a 404 from the router in a few
// hundred microseconds — a failure that reads like a backend fault and is
// miserable to trace. `coder-fim` is in exactly that state today.
func (r *ModelRegistry) lintRouteCategories(mode string) []Diagnostic {
	active := r.ModelsForMode(mode)
	roles := r.RolesForMode(mode)

	resolves := func(name string) bool {
		if _, ok := roles[name]; ok {
			return true
		}
		if _, ok := active[name]; ok {
			return true
		}
		for _, m := range active {
			if m.HFRepo == name {
				return true
			}
			for _, a := range m.Aliases {
				if a == name {
					return true
				}
			}
		}
		return false
	}

	var out []Diagnostic
	for _, cat := range ToolProxyRouteCategories {
		if resolves(cat) {
			continue
		}
		out = append(out, Diagnostic{
			Severity: SevWarn,
			Code:     LintRouteCategoryUnresolved,
			Subject:  cat,
			Mode:     mode,
			Message:  "the tool proxy auto-router advertises this category but nothing in this mode resolves it; a prompt classified here 404s",
		})
	}
	return out
}

// lintOwner is one model laying claim to a shared name, plus whether that
// claim is live. Enabled-ness is what decides severity throughout
// lintNameCollisions: the router resolves only against enabled models.
type lintOwner struct {
	id      string
	enabled bool
}

func describeOwners(owners []lintOwner) string {
	parts := make([]string, 0, len(owners))
	for _, o := range owners {
		state := "disabled"
		if o.enabled {
			state = "enabled"
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", o.id, state))
	}
	return strings.Join(parts, ", ")
}
