package config

import "testing"

func loadErr(t *testing.T, yaml string) error {
	t.Helper()
	_, err := LoadBytes([]byte(yaml))
	return err
}

const balanceBase = `
nodes:
  n: {host: n.local, gpu: nvidia, vram_gb: 16}
models:
  a: {hf_repo: a, node: n, capabilities: [text]}
  b: {hf_repo: b, node: n, capabilities: [text]}
roles:
  r:
`

func TestBalanceValidation(t *testing.T) {
	cases := []struct {
		name    string
		role    string
		wantErr string // substring; "" = must load
	}{
		{"default order loads", "    require: {locality: local}\n    candidates: [a, b]\n", ""},
		{"pressure loads", "    require: {locality: local}\n    balance: pressure\n    candidates: [a, b]\n", ""},
		{"grouped loads", "    require: {locality: local}\n    balance: pressure\n    balance_groups: [[a, b]]\n    candidates: [a, b]\n", ""},
		{"unknown balance", "    require: {locality: local}\n    balance: spread\n    candidates: [a, b]\n", "unknown balance"},
		{"group id not a candidate", "    require: {locality: local}\n    balance: pressure\n    balance_groups: [[a, c]]\n    candidates: [a, b]\n", "not one of its candidates"},
		{"id in two groups", "    require: {locality: local}\n    balance: pressure\n    balance_groups: [[a], [a, b]]\n    candidates: [a, b]\n", "belongs to one rank"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := loadErr(t, balanceBase+c.role)
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("expected load to succeed, got: %v", err)
			case c.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q, got nil", c.wantErr)
			case c.wantErr != "" && !contains(err.Error(), c.wantErr):
				t.Fatalf("error %q does not contain %q", err.Error(), c.wantErr)
			}
		})
	}
}

// RolesForMode must prune balance-group members that drop out of the active
// mode, so a mode filter never leaves a group pointing at an absent candidate.
func TestBalanceGroupsPrunedByMode(t *testing.T) {
	yaml := `
nodes:
  n: {host: n.local, gpu: nvidia, vram_gb: 16}
models:
  a: {hf_repo: a, node: n, capabilities: [text]}
  b: {hf_repo: b, node: n, capabilities: [text], tags: [mode:big]}
roles:
  r:
    require: {locality: local}
    balance: pressure
    balance_groups: [[a, b]]
    candidates: [a, b]
`
	reg, err := LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	roles := reg.RolesForMode("default") // b (mode:big) drops out
	r := roles["r"]
	if len(r.Candidates) != 1 || r.Candidates[0] != "a" {
		t.Fatalf("candidates after mode prune = %v, want [a]", r.Candidates)
	}
	for _, g := range r.BalanceGroups {
		for _, id := range g {
			if id == "b" {
				t.Fatalf("balance group still references pruned candidate b: %v", r.BalanceGroups)
			}
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
