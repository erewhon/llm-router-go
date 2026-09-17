package gpu

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/erewhon/llm-router-go/internal/config"
)

func nvidiaStub(mem, util, thermal string) ReaderOptions {
	return ReaderOptions{
		Exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			switch {
			case strings.HasPrefix(args[0], "--query-gpu=memory"):
				return []byte(mem), nil
			case strings.HasPrefix(args[0], "--query-gpu=utilization"):
				return []byte(util), nil
			case strings.HasPrefix(args[0], "--query-gpu=temperature"):
				return []byte(thermal), nil
			}
			return nil, fmt.Errorf("unexpected call: %s %v", name, args)
		},
	}
}

func TestNvidia_ThermalsReported(t *testing.T) {
	r := NewReader(config.GpuNvidia, nvidiaStub("1024, 16384\n", "37\n", "41, 12.34\n"))
	info, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if info.TempC == nil || *info.TempC != 41 {
		t.Errorf("temp = %v, want 41", info.TempC)
	}
	if info.PowerW == nil || *info.PowerW != 12.34 {
		t.Errorf("power = %v, want 12.34", info.PowerW)
	}
}

func TestNvidia_ThermalsMultiCardHottestAndSummed(t *testing.T) {
	// Two cards: the aggregate is the hottest temperature and the summed draw.
	r := NewReader(config.GpuNvidia, nvidiaStub("1024, 16384\n", "37\n", "41, 12.5\n67, 250.0\n"))
	info, _ := r.Read(context.Background())
	if info.TempC == nil || *info.TempC != 67 {
		t.Errorf("temp = %v, want the hottest card (67)", info.TempC)
	}
	if info.PowerW == nil || *info.PowerW != 262.5 {
		t.Errorf("power = %v, want 262.5 summed", info.PowerW)
	}
}

func TestNvidia_ThermalsNAStayNil(t *testing.T) {
	// A part that reports temperature but no power (or neither) must not
	// invent zeros; and a thermal failure must not fail the whole read.
	r := NewReader(config.GpuNvidia, nvidiaStub("1024, 16384\n", "37\n", "55, [N/A]\n"))
	info, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if info.TempC == nil || *info.TempC != 55 || info.PowerW != nil {
		t.Errorf("temp/power = %v/%v, want 55/nil", info.TempC, info.PowerW)
	}
	r = NewReader(config.GpuNvidia, nvidiaStub("1024, 16384\n", "37\n", "[N/A], [N/A]\n"))
	info, err = r.Read(context.Background())
	if err != nil || info.TempC != nil || info.PowerW != nil {
		t.Errorf("all-N/A: err=%v temp=%v power=%v, want nil/nil and no error", err, info.TempC, info.PowerW)
	}
	if info.TotalVRAMGB != 16.0 {
		t.Errorf("memory read must be unaffected: %v", info.TotalVRAMGB)
	}
}

func TestAMD_HwmonThermals(t *testing.T) {
	const gb = uint64(1024) * 1024 * 1024
	files := map[string][]byte{
		"/sys/class/drm/card0/device/mem_info_vram_total":         []byte(fmt.Sprintf("%d\n", 64*gb)),
		"/sys/class/drm/card0/device/mem_info_vram_used":          []byte(fmt.Sprintf("%d\n", 8*gb)),
		"/sys/class/drm/card0/device/hwmon/hwmon3/temp1_input":    []byte("61000\n"),
		"/sys/class/drm/card0/device/hwmon/hwmon3/power1_average": []byte("87500000\n"),
	}
	r := NewReader(config.GpuAMD, ReaderOptions{
		Glob: func(pattern string) ([]string, error) {
			switch pattern {
			case "/sys/class/drm/card[0-9]*/device/mem_info_vram_total":
				return []string{"/sys/class/drm/card0/device/mem_info_vram_total"}, nil
			case "/sys/class/drm/card0/device/hwmon/hwmon*/temp1_input":
				return []string{"/sys/class/drm/card0/device/hwmon/hwmon3/temp1_input"}, nil
			case "/sys/class/drm/card0/device/hwmon/hwmon*/power1_average":
				return []string{"/sys/class/drm/card0/device/hwmon/hwmon3/power1_average"}, nil
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
	if info.TempC == nil || *info.TempC != 61 {
		t.Errorf("temp = %v, want 61 (millidegrees / 1000)", info.TempC)
	}
	if info.PowerW == nil || *info.PowerW != 87.5 {
		t.Errorf("power = %v, want 87.5 (microwatts / 1e6)", info.PowerW)
	}
}

func TestAMD_NoHwmonStaysNil(t *testing.T) {
	const gb = uint64(1024) * 1024 * 1024
	files := map[string][]byte{
		"/sys/class/drm/card0/device/mem_info_vram_total": []byte(fmt.Sprintf("%d\n", 64*gb)),
		"/sys/class/drm/card0/device/mem_info_vram_used":  []byte(fmt.Sprintf("%d\n", 8*gb)),
	}
	r := NewReader(config.GpuAMD, ReaderOptions{
		Glob: func(pattern string) ([]string, error) {
			if pattern == "/sys/class/drm/card[0-9]*/device/mem_info_vram_total" {
				return []string{"/sys/class/drm/card0/device/mem_info_vram_total"}, nil
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
	if err != nil || info.TempC != nil || info.PowerW != nil {
		t.Errorf("no hwmon: err=%v temp=%v power=%v, want nil/nil", err, info.TempC, info.PowerW)
	}
}
