package nodeagent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMemUsageGB(t *testing.T) {
	// 1.5 TiB total, 1.0 TiB available -> 0.5 TiB used (like hekaton with
	// MiniMax M2.7 resident). Values in kB as /proc/meminfo reports them.
	const meminfo = `MemTotal:       1610612736 kB
MemFree:         100000000 kB
MemAvailable:   1073741824 kB
Buffers:           500000 kB
Cached:          20000000 kB
`
	dir := t.TempDir()
	p := filepath.Join(dir, "meminfo")
	if err := os.WriteFile(p, []byte(meminfo), 0o644); err != nil {
		t.Fatal(err)
	}

	used, total, err := memUsageGB(p)
	if err != nil {
		t.Fatalf("memUsageGB: %v", err)
	}
	// total = 1610612736 / (1024*1024) = 1536.0 GB
	if total != 1536.0 {
		t.Errorf("total = %v, want 1536.0", total)
	}
	// used = (1610612736 - 1073741824) / (1024*1024) = 512.0 GB
	if used != 512.0 {
		t.Errorf("used = %v, want 512.0", used)
	}
}

func TestMemUsageGB_MissingFields(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "meminfo")
	if err := os.WriteFile(p, []byte("MemTotal: 123 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := memUsageGB(p); err == nil {
		t.Error("expected error when MemAvailable is absent")
	}
}
