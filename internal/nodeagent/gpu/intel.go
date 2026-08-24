package gpu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// intelReader reads the Intel Arc cards (B50 on euclid, 2x B70 on talos).
//
// Preferred source: /run/gpu-vram/gpus.json, written by the repo's
// gpu-exporter (cmd/gpu-exporter) — a root fdinfo bridge for the xe driver,
// which has no global VRAM/busy sysfs counters and whose per-process fdinfo
// is owner-readable only. It carries PER-CARD used KiB and busy %.
//
// Fallback: xpu-smi text parsing ("stats" for used MiB, "discovery" for
// physical size). On talos xpu-smi is a shim over the old aggregate
// exporter; on euclid the real tool's utilisation fields read N/A. Either
// way the fallback yields aggregate-only, no busy figure.
//
// Total VRAM comes from the registry's per-node vram_gb in both paths
// (discovery is empty/zero on these cards); per-card total is that figure
// split evenly across the cards the exporter reports — exact for the
// homogeneous cards we run (2x32GB, 1x16GB).
type intelReader struct {
	opts ReaderOptions
}

// gpusJSONPath is where cmd/gpu-exporter publishes per-card figures.
const gpusJSONPath = "/run/gpu-vram/gpus.json"

// gpusJSONMaxAge rejects a stale file (exporter dead) so the agent falls
// back to xpu-smi instead of reporting frozen numbers. The exporter writes
// every 5s.
const gpusJSONMaxAge = 30 * time.Second

// exporterSnapshot mirrors cmd/gpu-exporter's output document.
type exporterSnapshot struct {
	TS   int64 `json:"ts"`
	GPUs []struct {
		PDev    string `json:"pdev"`
		UsedKiB int64  `json:"used_kib"`
		BusyPct *int   `json:"busy_pct"`
	} `json:"gpus"`
}

func (r *intelReader) Read(ctx context.Context) (Info, error) {
	if info, ok := r.readExporter(); ok {
		return info, nil
	}
	return r.readXPUSMI(ctx)
}

// readExporter builds per-card Info from the gpu-exporter file. ok=false
// (missing, unparsable, stale, or empty) means fall back to xpu-smi.
func (r *intelReader) readExporter() (Info, bool) {
	raw, err := r.readFile(gpusJSONPath)
	if err != nil {
		return Info{}, false
	}
	var snap exporterSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil || len(snap.GPUs) == 0 {
		return Info{}, false
	}
	if time.Since(time.Unix(snap.TS, 0)) > gpusJSONMaxAge {
		return Info{}, false
	}

	info := Info{GpuType: "intel", Unified: false}
	perCardTotalGB := 0.0
	if r.opts.FallbackTotalVRAMGB > 0 {
		info.TotalVRAMGB = float64(r.opts.FallbackTotalVRAMGB)
		perCardTotalGB = info.TotalVRAMGB / float64(len(snap.GPUs))
	}

	usedTotalGB := 0.0
	var maxBusy *int
	for i, g := range snap.GPUs {
		usedGB := float64(g.UsedKiB) / (1024 * 1024)
		usedTotalGB += usedGB
		if g.BusyPct != nil {
			if maxBusy == nil || *g.BusyPct > *maxBusy {
				b := *g.BusyPct
				maxBusy = &b
			}
		}
		info.Devices = append(info.Devices, Device{
			Index:       i,
			PDev:        g.PDev,
			UsedVRAMGB:  usedGB,
			TotalVRAMGB: perCardTotalGB,
			BusyPct:     g.BusyPct,
		})
	}
	if info.TotalVRAMGB > 0 {
		info.FreeVRAMGB = info.TotalVRAMGB - usedTotalGB
		if info.FreeVRAMGB < 0 {
			info.FreeVRAMGB = 0
		}
	}
	info.GPUBusyPct = maxBusy
	return info, true
}

// readXPUSMI is the legacy aggregate path (no busy figure).
func (r *intelReader) readXPUSMI(ctx context.Context) (Info, error) {
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

	// xpu-smi's utilisation fields read N/A on these cards; the busy
	// figure comes only from the gpu-exporter path above.
	return info, nil
}

func (r *intelReader) readFile(path string) ([]byte, error) {
	if r.opts.ReadFile != nil {
		return r.opts.ReadFile(path)
	}
	return defaultReadFile(path)
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
