// Package gpu reads VRAM and utilisation from the local GPU. Each vendor
// has its own quirks: NVIDIA falls through to /proc/meminfo on Sparks
// (nvidia-smi reports [N/A] for unified memory); AMD reads sysfs
// directly; Intel parses xpu-smi text output with a registry-supplied
// fallback for total VRAM.
//
// Phase 1c: read-only metrics. No subprocess management lives here —
// that belongs in backends/.
package gpu

import (
	"context"
	"fmt"

	"github.com/erewhon/llm-router-go/internal/config"
)

// Info is the GPU snapshot the agent embeds in /health.
type Info struct {
	GpuType     config.GpuType
	TotalVRAMGB float64
	FreeVRAMGB  float64
	Unified     bool
	GPUBusyPct  *int // nil if not available
}

// Reader extracts GPU info for a single host. Vendor-specific
// implementations live in nvidia.go / amd.go / intel.go and are picked
// by NewReader based on the node's configured GpuType.
type Reader interface {
	Read(ctx context.Context) (Info, error)
}

// FallbackVRAMGB lets the registry's configured per-node vram_gb act as
// a last resort when the vendor query fails outright (especially Intel
// xpu-smi which can return zero on older Arc drivers).
type ReaderOptions struct {
	// FallbackTotalVRAMGB is used when the vendor query fails to
	// determine total memory. Zero means no fallback.
	FallbackTotalVRAMGB int
	// Exec hook for tests; nil uses os/exec.
	Exec ExecFunc
	// ReadFile hook for tests; nil uses os.ReadFile.
	ReadFile ReadFileFunc
	// Glob hook for tests; nil uses filepath.Glob.
	Glob GlobFunc
}

// NewReader returns a Reader for the given GPU vendor. An unknown
// vendor returns a Reader whose Read always errors.
func NewReader(gpuType config.GpuType, opts ReaderOptions) Reader {
	switch gpuType {
	case config.GpuNvidia:
		return &nvidiaReader{opts: opts}
	case config.GpuAMD:
		return &amdReader{opts: opts}
	case config.GpuIntel:
		return &intelReader{opts: opts}
	default:
		return &unknownReader{gpuType: gpuType}
	}
}

type unknownReader struct{ gpuType config.GpuType }

func (u *unknownReader) Read(ctx context.Context) (Info, error) {
	return Info{}, fmt.Errorf("gpu: unsupported gpu type %q", u.gpuType)
}

// ---------------------------------------------------------------------------
// Test seams
// ---------------------------------------------------------------------------

// ExecFunc runs a command and returns its stdout. It mirrors
// exec.CommandContext().Output() so tests can stub nvidia-smi / xpu-smi.
type ExecFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// ReadFileFunc reads the named file. Defaults to os.ReadFile.
type ReadFileFunc func(name string) ([]byte, error)

// GlobFunc returns paths matching a shell glob. Defaults to filepath.Glob.
type GlobFunc func(pattern string) ([]string, error)

// mbToGB converts MB → GB as an unrounded float. The Python agent
// emits these as full-precision floats (`free_vram_mb / 1024`); we
// match that so dashboard comparisons line up.
func mbToGB(mb int) float64 {
	return float64(mb) / 1024.0
}

func intPtr(i int) *int { return &i }
