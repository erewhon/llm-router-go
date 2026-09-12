package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/health"
)

const liveYAML = `
nodes:
  archimedes:
    host: archimedes.local
    gpu: nvidia
    vram_gb: 128
models:
  alpha:
    hf_repo: org/alpha
    node: archimedes
    api_port: 5391
    aliases: [a1]
  or/kept:
    hf_repo: vendor/kept
    backend: external
    api_base: https://or.example/api/v1
    api_key: OR_KEY
  or/retired:
    hf_repo: vendor/retired
    backend: external
    api_base: https://or.example/api/v1
    api_key: OR_KEY
  zen/pinned:
    hf_repo: pinned
    backend: external
    api_base: https://zen.example/v1
discovery:
  - api_base: https://zen.example/v1
    prefix: zen/
    adopt: all
  - api_base: https://or.example/api/v1
    prefix: or/
    api_key: OR_KEY
    adopt: ["vendor/*"]
`

func fakeLive(listings map[string][]string, down map[string]bool) health.ListFunc {
	return func(_ context.Context, root, _, _ string) (health.Listing, error) {
		if down[root] {
			return nil, errors.New("dial tcp: connection refused")
		}
		ids, ok := listings[root]
		if !ok {
			return nil, errors.New("no such base")
		}
		l := health.Listing{}
		for _, id := range ids {
			l[id] = config.ListedModel{ID: id}
		}
		return l, nil
	}
}

func TestValidateLive_ReportsDriftAndCandidates(t *testing.T) {
	list := fakeLive(map[string][]string{
		"https://zen.example":    {"pinned", "brand-new"},
		"https://or.example/api": {"vendor/kept", "vendor/other"},
	}, map[string]bool{"http://archimedes.local:5391": true})

	code, out := runV(t, validateOpts{path: writeYAML(t, liveYAML), live: true, list: list, format: "json"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (live findings are advisory)\n%s", code, out)
	}
	var rep validateReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	byCode := map[string][]config.Diagnostic{}
	for _, w := range rep.Modes["default"].Warnings {
		byCode[w.Code] = append(byCode[w.Code], w)
	}
	if got := byCode[config.LintNotListed]; len(got) != 1 || got[0].Subject != "or/retired" || got[0].Severity != config.SevWarn {
		t.Errorf("not-listed = %+v, want one warning for or/retired", got)
	}
	if got := byCode[config.LintInventoryUnreachable]; len(got) != 1 || got[0].Subject != "http://archimedes.local:5391" {
		t.Errorf("inventory-unreachable = %+v, want the seat's base", got)
	}
	disc := byCode[config.LintDiscoveredModel]
	subjects := make([]string, 0, len(disc))
	for _, d := range disc {
		subjects = append(subjects, d.Subject)
		if d.Severity != config.SevInfo {
			t.Errorf("%s should be informational, got %s", d.Subject, d.Severity)
		}
	}
	if strings.Join(subjects, ",") != "or/vendor/other,zen/brand-new" {
		t.Errorf("discovered-model subjects = %v, want the two unregistered adoptable ids", subjects)
	}
	// A hand-written entry the provider still lists is neither absent nor a
	// candidate.
	for _, w := range rep.Modes["default"].Warnings {
		if w.Subject == "or/kept" || w.Subject == "zen/pinned" {
			t.Errorf("unexpected finding for a healthy hand-written entry: %+v", w)
		}
	}
	names := rep.Modes["default"].Expected.ModelNames
	if got := strings.Join(names["alpha"], ","); got != "alpha,a1" {
		t.Errorf("model_names[alpha] = %q, want id then aliases", got)
	}
}

func TestValidateLive_InfoIsNeverPromotedButNotListedIs(t *testing.T) {
	list := fakeLive(map[string][]string{
		"https://zen.example":          {"pinned", "brand-new"},
		"https://or.example/api":       {"vendor/kept", "vendor/retired"},
		"http://archimedes.local:5391": {"org/alpha"},
	}, nil)
	// --validate-strict promotes every WARNING; the fixture has one static
	// warning (route-category-unresolvable) so it fails — but the info
	// findings must not be what failed it, nor be promotable by name.
	_, out := runV(t, validateOpts{path: writeYAML(t, liveYAML), live: true, list: list, strict: true, format: "json"})
	var rep validateReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if strings.Join(rep.Promoted, ",") != "route-category-unresolvable" {
		t.Errorf("promoted = %v, want only the static warning (never discovered-model)", rep.Promoted)
	}
	code, out := runV(t, validateOpts{path: writeYAML(t, liveYAML), live: true, list: list,
		block: map[string]bool{config.LintDiscoveredModel: true}})
	if code != 0 {
		t.Errorf("exit = %d, want 0: --validate-block cannot promote an informational finding\n%s", code, out)
	}
	if !strings.Contains(out, "i discovered-model") || !strings.Contains(out, "informational") {
		t.Errorf("text output should mark info findings and count them:\n%s", out)
	}

	// Drop a hand-written id from the provider: --validate-block=not-listed
	// makes that a deploy blocker.
	list = fakeLive(map[string][]string{
		"https://zen.example":          {"pinned"},
		"https://or.example/api":       {"vendor/kept"},
		"http://archimedes.local:5391": {"org/alpha"},
	}, nil)
	code, out = runV(t, validateOpts{path: writeYAML(t, liveYAML), live: true, list: list,
		block: map[string]bool{config.LintNotListed: true}})
	if code != 1 || !strings.Contains(out, "promoted to failures: not-listed") {
		t.Errorf("exit = %d, want 1 with not-listed promoted\n%s", code, out)
	}
}

func TestValidateLive_SkippedWhenTheFileWouldNotBoot(t *testing.T) {
	fetched := false
	list := func(context.Context, string, string, string) (health.Listing, error) {
		fetched = true
		return health.Listing{}, nil
	}
	broken := liveYAML + "  bad:\n    hf_repo: x\n    backend: external\n"
	code, _ := runV(t, validateOpts{path: writeYAML(t, broken), live: true, list: list})
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if fetched {
		t.Error("no listing should be fetched for a file that fails Validate")
	}
	// And without --validate-live nothing is fetched either.
	code, _ = runV(t, validateOpts{path: writeYAML(t, liveYAML), list: list})
	if code != 0 || fetched {
		t.Errorf("exit = %d fetched = %v; plain --validate must stay offline", code, fetched)
	}
}
