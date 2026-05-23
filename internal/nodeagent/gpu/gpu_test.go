package gpu

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/erewhon/llm-router-go/internal/config"
)

// ---------------------------------------------------------------------------
// NVIDIA
// ---------------------------------------------------------------------------

func TestNvidia_DiscreteFromNvidiaSmi(t *testing.T) {
	r := NewReader(config.GpuNvidia, ReaderOptions{
		Exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "nvidia-smi" && len(args) > 0 && strings.HasPrefix(args[0], "--query-gpu=memory") {
				return []byte("1024, 16384\n"), nil
			}
			if name == "nvidia-smi" && len(args) > 0 && strings.HasPrefix(args[0], "--query-gpu=utilization") {
				return []byte("37\n"), nil
			}
			return nil, fmt.Errorf("unexpected call: %s %v", name, args)
		},
	})

	info, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if info.GpuType != config.GpuNvidia {
		t.Errorf("gpu_type = %q, want nvidia", info.GpuType)
	}
	if info.TotalVRAMGB != 16.0 || info.FreeVRAMGB != 1.0 {
		t.Errorf("vram = %v/%v, want 1.0/16.0", info.FreeVRAMGB, info.TotalVRAMGB)
	}
	if info.Unified {
		t.Errorf("unified = true, want false (discrete card)")
	}
	if info.GPUBusyPct == nil || *info.GPUBusyPct != 37 {
		t.Errorf("busy_pct = %v, want 37", info.GPUBusyPct)
	}
}

func TestNvidia_UnifiedFallsBackToMeminfo(t *testing.T) {
	// nvidia-smi returns [N/A] for unified memory (GB10 Sparks).
	// Reader must read /proc/meminfo and report unified=true.
	r := NewReader(config.GpuNvidia, ReaderOptions{
		Exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			switch {
			case strings.HasPrefix(args[0], "--query-gpu=memory"):
				return []byte("[N/A], [N/A]\n"), nil
			case strings.HasPrefix(args[0], "--query-gpu=utilization"):
				return []byte("12\n"), nil
			}
			return nil, fmt.Errorf("unexpected")
		},
		ReadFile: func(name string) ([]byte, error) {
			if name != "/proc/meminfo" {
				return nil, fmt.Errorf("unexpected read %s", name)
			}
			return []byte(`
MemTotal:       125443032 kB
MemFree:         3778892 kB
MemAvailable:   20217156 kB
Buffers:         123456 kB
`), nil
		},
	})

	info, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !info.Unified {
		t.Errorf("unified = false, want true (GB10 unified memory path)")
	}
	// 125443032 / 1024 = 122503 MB; / 1024 ≈ 119.6 GB.
	wantTotal := float64(125443032/1024) / 1024.0
	if info.TotalVRAMGB != wantTotal {
		t.Errorf("total = %v, want %v", info.TotalVRAMGB, wantTotal)
	}
	wantFree := float64(20217156/1024) / 1024.0
	if info.FreeVRAMGB != wantFree {
		t.Errorf("free = %v, want %v", info.FreeVRAMGB, wantFree)
	}
	if info.GPUBusyPct == nil || *info.GPUBusyPct != 12 {
		t.Errorf("busy = %v, want 12", info.GPUBusyPct)
	}
}

func TestNvidia_UtilisationNAStaysNil(t *testing.T) {
	r := NewReader(config.GpuNvidia, ReaderOptions{
		Exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if strings.HasPrefix(args[0], "--query-gpu=memory") {
				return []byte("100, 200\n"), nil
			}
			return []byte("[N/A]\n"), nil
		},
	})
	info, _ := r.Read(context.Background())
	if info.GPUBusyPct != nil {
		t.Errorf("busy = %v, want nil ([N/A])", info.GPUBusyPct)
	}
}

func TestNvidia_NoSmiAndNoMeminfo(t *testing.T) {
	r := NewReader(config.GpuNvidia, ReaderOptions{
		Exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return nil, errors.New("not found")
		},
		ReadFile: func(name string) ([]byte, error) {
			return nil, errors.New("not found")
		},
	})
	_, err := r.Read(context.Background())
	if err == nil {
		t.Fatal("expected error when both nvidia-smi and meminfo fail")
	}
}

// ---------------------------------------------------------------------------
// AMD
// ---------------------------------------------------------------------------

func TestAMD_SysfsRead(t *testing.T) {
	// Total = 64GB, used = 8GB.
	const totalB = uint64(64) * 1024 * 1024 * 1024
	const usedB = uint64(8) * 1024 * 1024 * 1024

	files := map[string][]byte{
		"/sys/class/drm/card0/device/mem_info_vram_total": []byte(fmt.Sprintf("%d\n", totalB)),
		"/sys/class/drm/card0/device/mem_info_vram_used":  []byte(fmt.Sprintf("%d\n", usedB)),
		"/sys/class/drm/card0/device/gpu_busy_percent":    []byte("42\n"),
	}

	r := NewReader(config.GpuAMD, ReaderOptions{
		Glob: func(pattern string) ([]string, error) {
			switch pattern {
			case "/sys/class/drm/card[0-9]*/device/mem_info_vram_total":
				return []string{"/sys/class/drm/card0/device/mem_info_vram_total"}, nil
			case "/sys/class/drm/card[0-9]*/device/gpu_busy_percent":
				return []string{"/sys/class/drm/card0/device/gpu_busy_percent"}, nil
			}
			return nil, nil
		},
		ReadFile: func(name string) ([]byte, error) {
			if v, ok := files[name]; ok {
				return v, nil
			}
			return nil, fmt.Errorf("not found: %s", name)
		},
	})

	info, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if info.GpuType != config.GpuAMD {
		t.Errorf("gpu_type = %q, want amd", info.GpuType)
	}
	if info.TotalVRAMGB != 64.0 || info.FreeVRAMGB != 56.0 {
		t.Errorf("vram = %v/%v, want 56/64", info.FreeVRAMGB, info.TotalVRAMGB)
	}
	if !info.Unified {
		t.Errorf("AMD reader should report unified=true (Strix Halo)")
	}
	if info.GPUBusyPct == nil || *info.GPUBusyPct != 42 {
		t.Errorf("busy = %v, want 42", info.GPUBusyPct)
	}
}

func TestAMD_NoSysfs(t *testing.T) {
	r := NewReader(config.GpuAMD, ReaderOptions{
		Glob:     func(string) ([]string, error) { return nil, nil },
		ReadFile: func(string) ([]byte, error) { return nil, errors.New("nope") },
	})
	_, err := r.Read(context.Background())
	if err == nil {
		t.Fatal("expected error when sysfs absent")
	}
}

// ---------------------------------------------------------------------------
// Intel
// ---------------------------------------------------------------------------

func TestIntel_XpuSmiWithDiscoveryMemory(t *testing.T) {
	// Real xpu-smi uses two-column tables: `| Field | Value |`. Some
	// driver versions surface "Memory Physical Size" in discovery; this
	// test asserts the happy path where both stats and discovery work.
	r := NewReader(config.GpuIntel, ReaderOptions{
		Exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name != "xpu-smi" {
				return nil, fmt.Errorf("unexpected %s", name)
			}
			switch args[0] {
			case "stats":
				return []byte(`
+-----------------------------+----------------------+
| Device ID                   | 0                    |
| GPU Memory Used (MiB)       | 2048                 |
+-----------------------------+----------------------+
`), nil
			case "discovery":
				return []byte(`
+-----------------------------+----------------------+
| Memory Physical Size        | 16384 MiB            |
+-----------------------------+----------------------+
`), nil
			}
			return nil, fmt.Errorf("unexpected args: %v", args)
		},
	})

	info, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if info.GpuType != config.GpuIntel {
		t.Errorf("gpu_type = %q, want intel", info.GpuType)
	}
	if info.TotalVRAMGB != 16.0 || info.FreeVRAMGB != 14.0 {
		t.Errorf("vram = %v/%v, want 14/16", info.FreeVRAMGB, info.TotalVRAMGB)
	}
	if info.Unified {
		t.Errorf("Intel discrete should report unified=false")
	}
	if info.GPUBusyPct != nil {
		t.Errorf("Intel busy_pct should be nil (xpu-smi util unreliable)")
	}
}

func TestIntel_DiscoveryQuietUsesRegistryFallback(t *testing.T) {
	// Real Arc B50 Pro on euclid: stats returns used MiB but discovery
	// doesn't mention "Memory Physical Size". Fall back to the registry's
	// per-node vram_gb.
	r := NewReader(config.GpuIntel, ReaderOptions{
		FallbackTotalVRAMGB: 16, // from euclid's node definition
		Exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if args[0] == "stats" {
				return []byte("| GPU Memory Used (MiB)       | 1024 |\n"), nil
			}
			return []byte("(no memory size in discovery)\n"), nil
		},
	})

	info, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if info.TotalVRAMGB != 16.0 {
		t.Errorf("total = %v, want 16 (fallback)", info.TotalVRAMGB)
	}
	if info.FreeVRAMGB != 15.0 {
		t.Errorf("free = %v, want 15", info.FreeVRAMGB)
	}
}

func TestIntel_BothFail(t *testing.T) {
	r := NewReader(config.GpuIntel, ReaderOptions{
		Exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return nil, errors.New("xpu-smi not installed")
		},
	})
	_, err := r.Read(context.Background())
	if err == nil {
		t.Fatal("expected error when xpu-smi unavailable and no fallback")
	}
}

// ---------------------------------------------------------------------------
// Unknown
// ---------------------------------------------------------------------------

func TestUnknown(t *testing.T) {
	r := NewReader(config.GpuType("future-vendor"), ReaderOptions{})
	_, err := r.Read(context.Background())
	if err == nil {
		t.Fatal("expected error for unknown gpu type")
	}
}
