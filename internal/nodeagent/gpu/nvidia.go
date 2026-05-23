package gpu

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// nvidiaReader queries nvidia-smi for memory + utilisation. On Sparks
// (GB10 unified memory) nvidia-smi reports [N/A] for memory; falls
// back to /proc/meminfo.
type nvidiaReader struct {
	opts ReaderOptions
}

func (r *nvidiaReader) Read(ctx context.Context) (Info, error) {
	info := Info{GpuType: "nvidia"}

	freeMB, totalMB, unified, memErr := r.readMemory(ctx)
	if memErr == nil {
		info.TotalVRAMGB = mbToGB(totalMB)
		info.FreeVRAMGB = mbToGB(freeMB)
		info.Unified = unified
	}

	if pct, err := r.readUtilisation(ctx); err == nil {
		info.GPUBusyPct = intPtr(pct)
	}

	if memErr != nil {
		return info, memErr
	}
	return info, nil
}

// readMemory returns (free_mb, total_mb, unified, error).
func (r *nvidiaReader) readMemory(ctx context.Context) (int, int, bool, error) {
	out, err := r.exec(ctx, "nvidia-smi",
		"--query-gpu=memory.free,memory.total",
		"--format=csv,noheader,nounits")
	if err == nil {
		// Sample: "1024, 16384" or "[N/A], [N/A]" for unified memory.
		line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
		if !strings.Contains(line, "[N/A]") {
			parts := strings.Split(line, ",")
			if len(parts) == 2 {
				free, errF := strconv.Atoi(strings.TrimSpace(parts[0]))
				total, errT := strconv.Atoi(strings.TrimSpace(parts[1]))
				if errF == nil && errT == nil && total > 0 {
					return free, total, false, nil
				}
			}
		}
	}
	// Either nvidia-smi failed, or returned [N/A] (unified memory).
	// Fall back to /proc/meminfo.
	return r.readMeminfo()
}

// readMeminfo reads MemTotal + MemAvailable from /proc/meminfo. Matches
// the Python agent's unified-memory fallback exactly: returns MB.
func (r *nvidiaReader) readMeminfo() (int, int, bool, error) {
	data, err := r.readFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false, fmt.Errorf("gpu/nvidia: read /proc/meminfo: %w", err)
	}
	var totalKB, availKB int
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			totalKB = scanKB(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			availKB = scanKB(line)
		}
	}
	if totalKB == 0 {
		return 0, 0, false, fmt.Errorf("gpu/nvidia: /proc/meminfo missing MemTotal")
	}
	return availKB / 1024, totalKB / 1024, true, nil
}

func (r *nvidiaReader) readUtilisation(ctx context.Context) (int, error) {
	out, err := r.exec(ctx, "nvidia-smi",
		"--query-gpu=utilization.gpu",
		"--format=csv,noheader,nounits")
	if err != nil {
		return 0, err
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if line == "[N/A]" {
		return 0, fmt.Errorf("utilisation not reported")
	}
	return strconv.Atoi(line)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// scanKB parses a /proc/meminfo line shaped "Key:   12345 kB".
func scanKB(line string) int {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, _ := strconv.Atoi(fields[1])
	return v
}

func (r *nvidiaReader) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	if r.opts.Exec != nil {
		return r.opts.Exec(ctx, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	return cmd.Output()
}

func (r *nvidiaReader) readFile(name string) ([]byte, error) {
	if r.opts.ReadFile != nil {
		return r.opts.ReadFile(name)
	}
	return defaultReadFile(name)
}
