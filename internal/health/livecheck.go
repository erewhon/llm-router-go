package health

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
)

// LiveCheck is `router --validate --validate-live`: one synchronous pass of
// the live inventory against a registry, with no router running, reported as
// lint diagnostics. It answers the two questions an operator has before a
// config push that the static linter cannot: is every hand-written external
// still listed by its provider, and what does each discovery source list that
// nobody has written down yet — the candidates to promote into a chain or a
// role.
//
// It is deliberately the same code path the running router uses (a Tracker
// with the inventory on and everything else off), so the tool and the
// service can never disagree about what "absent" or "adoptable" means.
func LiveCheck(ctx context.Context, reg *config.ModelRegistry, mode string,
	getenv func(string) string, list ListFunc, timeout time.Duration, logger *slog.Logger) []config.Diagnostic {
	if reg == nil {
		return nil
	}
	active := reg.ModelsForMode(mode)
	tr := NewTracker(Config{
		Registry:               reg,
		ProbeFilter:            func(id string) bool { _, ok := active[id]; return ok },
		Getenv:                 getenv,
		List:                   list,
		InventoryTimeout:       timeout,
		DisableGenerationProbe: true,
		// No node poll: only the listing fetches run, and stateLocked reads
		// an absent verdict off an otherwise-unknown poll state.
		Probe:  func(context.Context, string, int) NodeSnapshot { return NodeSnapshot{} },
		Logger: logger,
	})
	tr.RefreshInventory(ctx)

	disc := tr.Discovered()
	var out []config.Diagnostic
	for _, b := range tr.Inventory(false) {
		if b.FetchedAt == nil {
			out = append(out, config.Diagnostic{
				Severity: config.SevWarn, Code: config.LintInventoryUnreachable, Subject: b.Base, Mode: mode,
				Message: fmt.Sprintf("listing could not be fetched (%s); the router would keep routing its %d member(s) on config alone", b.Error, len(b.Members)),
			})
			continue
		}
		for _, id := range b.Absent {
			_, _, reason := tr.State(id)
			out = append(out, config.Diagnostic{
				Severity: config.SevWarn, Code: config.LintNotListed, Subject: id, Mode: mode,
				Message: reason + "; the router would mark it absent and drop it from /v1/models",
			})
		}
		for _, id := range b.Discovered {
			m := disc[id]
			var meta []string
			if m.ContextLength > 0 {
				meta = append(meta, fmt.Sprintf("context %d", m.ContextLength))
			}
			if m.InputCostPerMillion != nil && m.OutputCostPerMillion != nil {
				meta = append(meta, fmt.Sprintf("$%.3g/$%.3g per M", *m.InputCostPerMillion, *m.OutputCostPerMillion))
			}
			detail := ""
			if len(meta) > 0 {
				detail = " (" + strings.Join(meta, ", ") + ")"
			}
			out = append(out, config.Diagnostic{
				Severity: config.SevInfo, Code: config.LintDiscoveredModel, Subject: id, Mode: mode,
				Message: fmt.Sprintf("adoptable from %s%s — the router serves it by this name; write a models.yaml entry to pin pricing/aliases or to seat it in a role", b.Base, detail),
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Code != out[j].Code {
			return out[i].Code < out[j].Code
		}
		return out[i].Subject < out[j].Subject
	})
	return out
}
