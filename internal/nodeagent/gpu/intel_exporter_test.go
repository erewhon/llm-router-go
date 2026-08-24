package gpu

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/erewhon/llm-router-go/internal/config"
)

func exporterJSON(ts int64) []byte {
	return []byte(fmt.Sprintf(`{"ts":%d,"gpus":[
		{"pdev":"0000:05:00.0","used_kib":25165824,"busy_pct":72},
		{"pdev":"0000:08:00.0","used_kib":15728640,"busy_pct":31}
	]}`, ts))
}

// Fresh gpus.json → per-card devices, summed used, split total, max busy.
func TestIntel_ExporterPerCard(t *testing.T) {
	r := NewReader(config.GpuIntel, ReaderOptions{
		FallbackTotalVRAMGB: 64,
		ReadFile: func(path string) ([]byte, error) {
			if path != gpusJSONPath {
				return nil, fmt.Errorf("unexpected read: %s", path)
			}
			return exporterJSON(time.Now().Unix()), nil
		},
		Exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			t.Fatalf("xpu-smi must not be invoked when gpus.json is fresh (got %s %v)", name, args)
			return nil, nil
		},
	})
	info, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(info.Devices) != 2 {
		t.Fatalf("devices = %d, want 2", len(info.Devices))
	}
	// 25165824 KiB = 24 GB, 15728640 KiB = 15 GB.
	if info.Devices[0].UsedVRAMGB != 24 || info.Devices[1].UsedVRAMGB != 15 {
		t.Errorf("per-card used = %.1f/%.1f, want 24/15",
			info.Devices[0].UsedVRAMGB, info.Devices[1].UsedVRAMGB)
	}
	if info.Devices[0].TotalVRAMGB != 32 || info.Devices[1].TotalVRAMGB != 32 {
		t.Errorf("per-card total = %.1f/%.1f, want 32/32 (64 split evenly)",
			info.Devices[0].TotalVRAMGB, info.Devices[1].TotalVRAMGB)
	}
	if info.Devices[0].PDev != "0000:05:00.0" {
		t.Errorf("pdev = %q", info.Devices[0].PDev)
	}
	if info.TotalVRAMGB != 64 {
		t.Errorf("aggregate total = %.1f, want 64", info.TotalVRAMGB)
	}
	if got := info.TotalVRAMGB - info.FreeVRAMGB; got != 39 {
		t.Errorf("aggregate used = %.1f, want 39", got)
	}
	if info.GPUBusyPct == nil || *info.GPUBusyPct != 72 {
		t.Errorf("aggregate busy = %v, want 72 (busiest card)", info.GPUBusyPct)
	}
	if b := info.Devices[1].BusyPct; b == nil || *b != 31 {
		t.Errorf("card 1 busy = %v, want 31", b)
	}
}

// A stale gpus.json (exporter dead) must fall back to xpu-smi instead of
// reporting frozen numbers.
func TestIntel_ExporterStaleFallsBackToXpuSmi(t *testing.T) {
	execCalled := false
	r := NewReader(config.GpuIntel, ReaderOptions{
		FallbackTotalVRAMGB: 16,
		ReadFile: func(path string) ([]byte, error) {
			return exporterJSON(time.Now().Add(-2 * time.Minute).Unix()), nil
		},
		Exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			execCalled = true
			if args[0] == "stats" {
				return []byte("| GPU Memory Used (MiB) | 1024 |\n"), nil
			}
			return []byte(""), nil // discovery: quiet, like the shim
		},
	})
	info, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !execCalled {
		t.Fatal("stale gpus.json did not fall back to xpu-smi")
	}
	if len(info.Devices) != 0 {
		t.Errorf("fallback path should report no per-card devices, got %d", len(info.Devices))
	}
	if info.TotalVRAMGB != 16 {
		t.Errorf("total = %.1f, want registry fallback 16", info.TotalVRAMGB)
	}
	if info.GPUBusyPct != nil {
		t.Errorf("fallback busy = %v, want nil", *info.GPUBusyPct)
	}
}
