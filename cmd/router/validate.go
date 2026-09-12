package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/health"
)

// --validate: check a models.yaml without starting a server.
//
// Why this exists: the authoritative validator — config.Validate — has always
// run at startup in every binary, which meant the only way to find out whether
// an edit was safe was to restart production and watch. Validation happened
// after the disruption it was supposed to prevent. This flag makes the same
// code reachable ahead of a deploy.
//
// It deliberately reports two independent things:
//
//	errors   — config.Validate failures. The binary WOULD NOT BOOT on this
//	           file. Never advisory.
//	warnings — config.Lint findings. Advisory by construction (Lint has no
//	           caller on the boot path); promote them per-code with
//	           --validate-block, or wholesale with --validate-strict.
//	           With --validate-live, health.LiveCheck's findings join them:
//	           every upstream's /v1/models is fetched once and compared to
//	           the file — hand-written entries the provider no longer lists,
//	           bases that could not be reached, and (as info, never promoted)
//	           the ids a discovery source would adopt that nobody has
//	           written down yet.
//
// Exit codes are the scripting contract:
//
//	0  clean (warnings may have printed)
//	1  would not boot, or a promoted warning fired  → do not deploy
//	2  file unreadable / bad flags                  → nothing was checked
//
// The 2-vs-1 split matters: a deploy script must be able to tell "this config
// is bad" from "I failed to check", and must refuse to proceed on both.

type validateOpts struct {
	path   string
	format string
	modes  []string
	strict bool
	block  map[string]bool
	stdout io.Writer
	stderr io.Writer

	// live runs health.LiveCheck per mode: every upstream base's listing is
	// fetched once (with getenv resolving api_key env names, list doing the
	// fetch — tests inject a fake — and timeout bounding each one).
	live    bool
	getenv  func(string) string
	list    health.ListFunc
	timeout time.Duration
}

// modeReport is the per-mode result: what lint said, and what the router would
// actually consider routable in that mode.
type modeReport struct {
	Warnings []config.Diagnostic `json:"warnings"`
	Expected expectedSet         `json:"expected"`
}

// expectedSet is what a caller should see served in a given mode.
//
// This is emitted so post-deploy verification can diff models.yaml against a
// live /v1/models WITHOUT reimplementing the mode-tag rule. That rule lives in
// ModelsForMode/RolesForMode and nowhere else; a shell or Python
// reimplementation would drift, and the first symptom would be false failures
// for every mode:big entry (the router units bake --mode=default).
type expectedSet struct {
	Models []string `json:"models"` // ids + aliases, i.e. every routable name
	Roles  []string `json:"roles"`
	// ModelNames maps each model id to every name it is served under (the
	// id itself first, then its aliases), so a verifier that learns from
	// /v1/availability that an id is absent can excuse its aliases too.
	ModelNames map[string][]string `json:"model_names"`
}

type validateReport struct {
	Path     string                `json:"path"`
	SHA256   string                `json:"sha256"`
	OK       bool                  `json:"ok"`
	Errors   []string              `json:"errors"`
	Counts   map[string]int        `json:"counts"`
	Modes    map[string]modeReport `json:"modes"`
	Promoted []string              `json:"promoted,omitempty"`
}

func runValidate(o validateOpts) int {
	data, err := os.ReadFile(o.path)
	if err != nil {
		fmt.Fprintf(o.stderr, "cannot read %s: %v\n", o.path, err)
		return 2
	}
	sum := sha256.Sum256(data)

	rep := validateReport{
		Path:   o.path,
		SHA256: hex.EncodeToString(sum[:]),
		Errors: []string{},
		Counts: map[string]int{},
		Modes:  map[string]modeReport{},
	}

	// ParseBytes, not LoadBytes: a file that fails Validate should still get
	// linted, so one broken role doesn't hide five other problems and turn
	// the fix into a guessing game.
	reg, err := config.ParseBytes(data)
	if err != nil {
		rep.Errors = append(rep.Errors, err.Error())
		emitValidate(o, rep)
		return 1
	}

	enabled := 0
	for _, m := range reg.Models {
		if m.Enabled {
			enabled++
		}
	}
	rep.Counts["models"] = len(reg.Models)
	rep.Counts["enabled"] = enabled
	rep.Counts["nodes"] = len(reg.Nodes)
	rep.Counts["roles"] = len(reg.Roles)

	if err := reg.Validate(); err != nil {
		// Validate joins every problem with errors.Join, which renders one
		// per line. Split so each lands as its own entry rather than one
		// unreadable blob.
		for _, line := range strings.Split(err.Error(), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				rep.Errors = append(rep.Errors, line)
			}
		}
	}

	promoted := map[string]bool{}
	for _, mode := range o.modes {
		warns := reg.Lint(mode)
		// Live findings only against a file that would boot: a registry that
		// failed Validate may have a broken discovery block, and the point
		// of the pass is the network, not the parse.
		if o.live && len(rep.Errors) == 0 {
			warns = append(warns, o.liveCheck(reg, mode)...)
		}
		for _, w := range warns {
			if w.Severity == config.SevInfo {
				continue // informational: never a failure
			}
			if o.strict || o.block[w.Code] {
				promoted[w.Code] = true
			}
		}
		rep.Modes[mode] = modeReport{
			Warnings: warns,
			Expected: expectedFor(reg, mode),
		}
	}
	for code := range promoted {
		rep.Promoted = append(rep.Promoted, code)
	}
	sort.Strings(rep.Promoted)

	rep.OK = len(rep.Errors) == 0 && len(rep.Promoted) == 0
	emitValidate(o, rep)
	if !rep.OK {
		return 1
	}
	return 0
}

// liveCheck runs one inventory pass for a mode, with the real fetcher and
// os.Getenv unless the caller injected substitutes.
func (o validateOpts) liveCheck(reg *config.ModelRegistry, mode string) []config.Diagnostic {
	getenv, list, timeout := o.getenv, o.list, o.timeout
	if getenv == nil {
		getenv = os.Getenv
	}
	if list == nil {
		list = health.FetchListing
	}
	if timeout <= 0 {
		timeout = health.DefaultInventoryTimeout
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return health.LiveCheck(context.Background(), reg, mode, getenv, list, timeout, logger)
}

func expectedFor(reg *config.ModelRegistry, mode string) expectedSet {
	models := reg.ModelsForMode(mode)
	names := make([]string, 0, len(models))
	byModel := make(map[string][]string, len(models))
	for id, m := range models {
		names = append(names, id)
		names = append(names, m.Aliases...)
		byModel[id] = append([]string{id}, m.Aliases...)
	}
	sort.Strings(names)

	roles := reg.RolesForMode(mode)
	roleNames := make([]string, 0, len(roles))
	for name := range roles {
		roleNames = append(roleNames, name)
	}
	sort.Strings(roleNames)

	return expectedSet{Models: names, Roles: roleNames, ModelNames: byModel}
}

func emitValidate(o validateOpts, rep validateReport) {
	if o.format == "json" {
		enc := json.NewEncoder(o.stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
		return
	}

	fmt.Fprintf(o.stdout, "%s\n  sha256:%s\n", rep.Path, rep.SHA256)
	if len(rep.Counts) > 0 {
		fmt.Fprintf(o.stdout, "  %d models (%d enabled)  %d nodes  %d roles\n",
			rep.Counts["models"], rep.Counts["enabled"], rep.Counts["nodes"], rep.Counts["roles"])
	}

	fmt.Fprintf(o.stdout, "\nERRORS (would prevent startup): ")
	if len(rep.Errors) == 0 {
		fmt.Fprintln(o.stdout, "none")
	} else {
		fmt.Fprintf(o.stdout, "%d\n", len(rep.Errors))
		for _, e := range rep.Errors {
			fmt.Fprintf(o.stdout, "  %s\n", e)
		}
	}

	warnTotal, infoTotal := 0, 0
	for _, mode := range sortedKeys(rep.Modes) {
		mr := rep.Modes[mode]
		for _, w := range mr.Warnings {
			if w.Severity == config.SevInfo {
				infoTotal++
			} else {
				warnTotal++
			}
		}
		fmt.Fprintf(o.stdout, "\nWARNINGS (mode=%s): ", mode)
		if len(mr.Warnings) == 0 {
			fmt.Fprintln(o.stdout, "none")
			continue
		}
		fmt.Fprintf(o.stdout, "%d\n", len(mr.Warnings))
		for _, w := range mr.Warnings {
			mark := " "
			switch {
			case rep.Promoted != nil && contains(rep.Promoted, w.Code):
				mark = "!" // promoted to a failure by --validate-strict/-block
			case w.Severity == config.SevInfo:
				mark = "i" // informational: nothing to fix
			}
			fmt.Fprintf(o.stdout, " %s %-28s %-22s %s\n", mark, w.Code, w.Subject, w.Message)
		}
	}

	fmt.Fprintln(o.stdout)
	switch {
	case len(rep.Errors) > 0:
		fmt.Fprintf(o.stdout, "FAIL — %d error(s). The router would refuse to start on this file.\n", len(rep.Errors))
	case len(rep.Promoted) > 0:
		fmt.Fprintf(o.stdout, "FAIL — %d warning(s) promoted to failures: %s\n", len(rep.Promoted), strings.Join(rep.Promoted, ", "))
	default:
		if infoTotal > 0 {
			fmt.Fprintf(o.stdout, "OK — 0 errors, %d warning(s), %d informational.\n", warnTotal, infoTotal)
		} else {
			fmt.Fprintf(o.stdout, "OK — 0 errors, %d warning(s).\n", warnTotal)
		}
	}
}

func sortedKeys(m map[string]modeReport) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// parseModes splits --validate-mode. An empty entry is meaningful: it means
// "no mode filter", i.e. every enabled model, so it is preserved rather than
// dropped.
func parseModes(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		out = append(out, strings.TrimSpace(p))
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

func parseBlockList(s string) map[string]bool {
	out := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out[p] = true
		}
	}
	return out
}
