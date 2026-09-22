package toolproxycmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/erewhon/llm-router-go/cli/internal/exitcode"
)

const goodRegistry = `
nodes:
  n: {host: n, gpu: nvidia, vram_gb: 32}
models:
  m: {hf_repo: org/Model, node: n}
`

// --validate is what the deploy preflight runs on every reader; it must be
// exactly this binary's own load, and it must exit non-zero on a file the
// binary could not start with.
func TestValidateFlag(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.yaml")
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(good, []byte(goodRegistry), 0o600); err != nil {
		t.Fatal(err)
	}
	// A value this binary's config package does not know — the 2026-09-10 shape.
	if err := os.WriteFile(bad, []byte(goodRegistry+`
roles:
  r: {require: {locality: from_the_future}, candidates: [m]}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc := exitcode.Code(Run(context.Background(), []string{"--validate", "--models-yaml", good})); rc != 0 {
		t.Errorf("--validate on a loadable file: exit %d, want 0", rc)
	}
	if rc := exitcode.Code(Run(context.Background(), []string{"--validate", "--models-yaml", bad})); rc != 1 {
		t.Errorf("--validate on an unloadable file: exit %d, want 1", rc)
	}
	if rc := exitcode.Code(Run(context.Background(), []string{"--validate", "--models-yaml", filepath.Join(dir, "missing.yaml")})); rc != 1 {
		t.Errorf("--validate on a missing file: exit %d, want 1", rc)
	}
}
