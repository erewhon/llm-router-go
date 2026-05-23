package gpu

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// amdReader reads from /sys/class/drm/card*/device/mem_info_vram_*.
// Works on amdgpu-driven cards without root or extra binaries; Strix
// Halo's unified memory is reported as a single allocation here.
type amdReader struct {
	opts ReaderOptions
}

func (r *amdReader) Read(ctx context.Context) (Info, error) {
	totals, err := r.glob("/sys/class/drm/card[0-9]*/device/mem_info_vram_total")
	if err != nil || len(totals) == 0 {
		return Info{GpuType: "amd"}, fmt.Errorf("gpu/amd: no /sys/class/drm/card*/device/mem_info_vram_total")
	}
	// First match wins; the user's nodes have a single GPU each.
	first := totals[0]
	device := filepath.Dir(first)

	totalB, err := readUint(r.readFile, first)
	if err != nil {
		return Info{GpuType: "amd"}, err
	}
	usedB, err := readUint(r.readFile, filepath.Join(device, "mem_info_vram_used"))
	if err != nil {
		return Info{GpuType: "amd"}, err
	}

	const mb = 1024 * 1024
	totalMB := int(totalB / mb)
	freeMB := int((totalB - usedB) / mb)

	info := Info{
		GpuType:     "amd",
		TotalVRAMGB: mbToGB(totalMB),
		FreeVRAMGB:  mbToGB(freeMB),
		Unified:     true, // Strix Halo
	}

	if pct, err := r.readBusy(); err == nil {
		info.GPUBusyPct = intPtr(pct)
	}

	return info, nil
}

func (r *amdReader) readBusy() (int, error) {
	matches, err := r.glob("/sys/class/drm/card[0-9]*/device/gpu_busy_percent")
	if err != nil {
		return 0, err
	}
	for _, p := range matches {
		data, err := r.readFile(p)
		if err != nil {
			continue
		}
		v, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err == nil {
			return v, nil
		}
	}
	return 0, fmt.Errorf("gpu/amd: gpu_busy_percent unavailable")
}

func (r *amdReader) glob(pattern string) ([]string, error) {
	if r.opts.Glob != nil {
		return r.opts.Glob(pattern)
	}
	return filepath.Glob(pattern)
}

func (r *amdReader) readFile(name string) ([]byte, error) {
	if r.opts.ReadFile != nil {
		return r.opts.ReadFile(name)
	}
	return defaultReadFile(name)
}

func readUint(read ReadFileFunc, path string) (uint64, error) {
	data, err := read(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
}
