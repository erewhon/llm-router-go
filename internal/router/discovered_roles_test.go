package router

import (
	"strings"
	"testing"

	"github.com/erewhon/llm-router-go/internal/config"
)

// A role may name a discovered id exactly. Two sources, as on a laptop that
// discovers from LM Studio and from an internal gateway: prefixes keep the
// ids apart, and the gateway's capabilities floor vouches for tool calling
// its listing never mentions.
const discoveredRolesYAML = `
nodes:
  archimedes: {host: archimedes.local, gpu: nvidia, vram_gb: 128}
roles:
  chat:
    candidates: [lms/qwen3-8b, seat]
  tools:
    require: {capabilities: [tool_calling]}
    candidates: [lms/qwen3-8b, corp/gpt-x]
models:
  seat:
    hf_repo: Qwen/Seat
    node: archimedes
    api_port: 5391
discovery:
  - api_base: http://lms.example/v1
    prefix: lms/
    adopt: all
  - api_base: https://corp.example/v1
    prefix: corp/
    adopt: ["gpt-*"]
    capabilities: [text, tool_calling]
`

const (
	lmsRoot  = "http://lms.example"
	corpRoot = "https://corp.example"
)

func TestDiscoveredRoleMembers(t *testing.T) {
	h := newInventoryHarnessYAML(t, discoveredRolesYAML)
	h.listings.set(invSeatRoot, listed("other")) // the seat is absent
	h.listings.set(lmsRoot)
	h.listings.set(corpRoot)
	h.refresh()

	// Nothing listed yet: the role fails and says why, naming the source.
	_, err := h.rt.resolveRole("chat", "chat", false, 0, tierNone)
	var roleErr *roleUnavailableError
	if err == nil || !asRoleErr(err, &roleErr) {
		t.Fatalf("chat should be unavailable, got %v", err)
	}
	if got := strings.Join(roleErr.Reasons, "; "); !strings.Contains(got, `lms/qwen3-8b: not listed by lms/ (http://lms.example/v1)`) {
		t.Fatalf("reasons = %s", got)
	}

	// LM Studio loads the model: one poll later the role binds to it, and a
	// request to the role reaches LM Studio under the bare provider id.
	h.listings.set(lmsRoot, listed("qwen3-8b"))
	h.listings.set(corpRoot, listed("gpt-x"), listed("embed-1"))
	h.refresh()
	res, err := h.rt.resolveRole("chat", "chat", false, 0, tierNone)
	if err != nil || res.ModelID != "lms/qwen3-8b" || !res.Discovered {
		t.Fatalf("chat → %+v, %v", res, err)
	}
	if rec := postChat(t, h.rt, `{"model":"chat","messages":[{"role":"user","content":"hi"}]}`); rec.Code != 200 {
		t.Fatalf("chat via role: %d %s", rec.Code, rec.Body.String())
	}
	h.upstream.mu.Lock()
	got := append([]string(nil), h.upstream.models...)
	h.upstream.mu.Unlock()
	if len(got) != 1 || got[0] != "qwen3-8b" {
		t.Fatalf("upstream saw %v", got)
	}

	// tools needs tool_calling: LM Studio's listing does not say, so its
	// entry is skipped with a reason; the gateway's floor vouches.
	res, err = h.rt.resolveRole("tools", "tools", false, 0, tierNone)
	if err != nil || res.ModelID != "corp/gpt-x" {
		t.Fatalf("tools → %+v, %v", res, err)
	}
	var chatCands []string
	for _, b := range h.rt.roleBindings() {
		if b.Role == "tools" && (!b.Available || b.Target != "corp/gpt-x") {
			t.Fatalf("binding %+v", b)
		}
		if b.Role == "chat" {
			chatCands = b.Candidates
		}
	}
	if strings.Join(chatCands, ",") != "lms/qwen3-8b,seat" {
		t.Fatalf("chat candidates %v", chatCands)
	}
	h.listings.set(corpRoot)
	h.refresh()
	h.refresh() // two misses retire the id
	_, err = h.rt.resolveRole("tools", "tools", false, 0, tierNone)
	if err == nil || !asRoleErr(err, &roleErr) {
		t.Fatalf("tools should be unavailable, got %v", err)
	}
	joined := strings.Join(roleErr.Reasons, "; ")
	if !strings.Contains(joined, `lms/qwen3-8b: lacks required capability "tool_calling"`) || !strings.Contains(joined, "corp/gpt-x: not listed by corp/") {
		t.Fatalf("reasons = %s", joined)
	}

	// adopt still decides what exists: embed-1 was listed but never adopted.
	if _, ok := h.rt.lookupModel("corp/embed-1"); ok {
		t.Error("corp/embed-1 is outside adopt and must not be an entry")
	}
}

func asRoleErr(err error, target **roleUnavailableError) bool {
	e, ok := err.(*roleUnavailableError)
	if ok {
		*target = e
	}
	return ok
}

func TestDiscoveredRoleMembersSurviveModeFilter(t *testing.T) {
	reg, err := config.LoadBytes([]byte(discoveredRolesYAML))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"", "big"} {
		roles := reg.RolesForMode(mode)
		if got := strings.Join(roles["tools"].Candidates, ","); got != "lms/qwen3-8b,corp/gpt-x" {
			t.Errorf("mode %q: tools candidates %q", mode, got)
		}
	}
}
