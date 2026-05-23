package gpu

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// intelReader queries xpu-smi for the Intel Arc B50 Pro on euclid.
// xpu-smi's "stats" command reports used MiB; "discovery" reports the
// physical memory size. If discovery returns 0 (Arc B50 Pro drivers can
// be quiet here), fall back to the registry-supplied total.
type intelReader struct {
	opts ReaderOptions
}

func (r *intelReader) Read(ctx context.Context) (Info, error) {
	info := Info{GpuType: "intel"}

	usedMB := r.queryUsedMB(ctx)
	totalMB := r.queryTotalMB(ctx)

	if totalMB == 0 && r.opts.FallbackTotalVRAMGB > 0 {
		totalMB = r.opts.FallbackTotalVRAMGB * 1024
	}
	if totalMB == 0 {
		return info, fmt.Errorf("gpu/intel: unable to determine total VRAM (xpu-smi discovery returned 0 and no fallback)")
	}

	info.TotalVRAMGB = mbToGB(totalMB)
	if usedMB <= totalMB {
		info.FreeVRAMGB = mbToGB(totalMB - usedMB)
	} else {
		info.FreeVRAMGB = 0
	}
	info.Unified = false

	// xpu-smi's utilisation field is unreliable on Arc B50 Pro; the
	// Python agent doesn't expose a number here either. Leave nil.
	return info, nil
}

// queryUsedMB parses `xpu-smi stats -d 0` for "GPU Memory Used (MiB)".
func (r *intelReader) queryUsedMB(ctx context.Context) int {
	out, err := r.exec(ctx, "xpu-smi", "stats", "-d", "0")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "GPU Memory Used (MiB)") {
			parts := strings.Split(line, "|")
			if len(parts) >= 3 {
				if v, err := strconv.Atoi(strings.TrimSpace(parts[2])); err == nil {
					return v
				}
			}
		}
	}
	return 0
}

// queryTotalMB parses `xpu-smi discovery` for "Memory Physical Size".
func (r *intelReader) queryTotalMB(ctx context.Context) int {
	out, err := r.exec(ctx, "xpu-smi", "discovery")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "Memory Physical Size") {
			parts := strings.Split(line, "|")
			if len(parts) >= 3 {
				val := strings.TrimSpace(parts[2])
				val = strings.TrimSuffix(val, "MiB")
				val = strings.TrimSpace(val)
				if v, err := strconv.Atoi(val); err == nil {
					return v
				}
			}
		}
	}
	return 0
}

func (r *intelReader) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	if r.opts.Exec != nil {
		return r.opts.Exec(ctx, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	return cmd.Output()
}
