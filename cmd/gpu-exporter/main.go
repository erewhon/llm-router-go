// gpu-exporter — root fdinfo→/run bridge for Intel xe-driver GPUs.
//
// The xe driver has no global VRAM-used or busy-percent sysfs counters
// (unlike amdgpu's gpu_busy_percent), and per-process DRM fdinfo is
// owner-readable only — the node agent (its own user, NoNewPrivileges)
// cannot read another user's llama-server fdinfo. This tiny root loop
// bridges the gap, the same way nvtop reads these cards:
//
//   - scan /proc/*/fdinfo/*, keep entries whose drm-pdev is an Intel xe
//     card, dedupe by (pdev, drm-client-id) — multiple fds of one DRM
//     client repeat identical counters
//   - per card: VRAM used = sum of drm-resident-vram0 across clients;
//     busy % = max over engine classes of Δdrm-cycles / Δdrm-total-cycles
//     between iterations (drm-total-cycles is the GT timestamp counter)
//   - publish /run/gpu-vram/gpus.json atomically, plus the legacy
//     /run/gpu-vram/used_kib aggregate the old talos bash exporter wrote
//     (kept so the xpu-smi shim / older agents keep working)
//
// Cards with no DRM clients still appear (used 0) — they are discovered
// from /sys/class/drm so an idle second card is reported, not omitted.
//
// Replaces the hand-rolled /usr/local/bin/gpu-vram-exporter bash loop on
// talos (2026-08-23); also deployed to euclid, whose real xpu-smi reports
// N/A for every utilisation field on the Arc B50.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	outDir       = "/run/gpu-vram"
	scanInterval = 5 * time.Second
)

// clientSample is one DRM client's counters at one scan.
type clientSample struct {
	residentKiB int64
	cycles      map[string]uint64 // engine class -> drm-cycles-<class>
	totalCycles map[string]uint64 // engine class -> drm-total-cycles-<class>
}

// cardSample aggregates one card's clients at one scan.
type cardSample struct {
	usedKiB     int64
	cycles      map[string]uint64 // summed across clients
	totalCycles map[string]uint64 // max across clients (same GT counter)
}

type gpuOut struct {
	PDev    string `json:"pdev"`
	UsedKiB int64  `json:"used_kib"`
	BusyPct *int   `json:"busy_pct"`
}

type snapshot struct {
	TS   int64    `json:"ts"`
	GPUs []gpuOut `json:"gpus"`
}

func main() {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "gpu-exporter: %v\n", err)
		os.Exit(1)
	}
	cards := discoverXeCards()
	if len(cards) == 0 {
		fmt.Fprintln(os.Stderr, "gpu-exporter: no Intel xe cards found under /sys/class/drm")
		os.Exit(1)
	}
	fmt.Printf("gpu-exporter: watching %d card(s): %s\n", len(cards), strings.Join(cards, " "))

	prev := map[string]cardSample{}
	for {
		cur := scan(cards)
		publish(cards, cur, prev)
		prev = cur
		time.Sleep(scanInterval)
	}
}

// discoverXeCards returns the sorted PCI addresses of Intel cards bound to
// the xe driver. Sorted so index assignment is stable across restarts.
func discoverXeCards() []string {
	seen := map[string]bool{}
	matches, _ := filepath.Glob("/sys/class/drm/card[0-9]*/device")
	for _, dev := range matches {
		vendor, err := os.ReadFile(filepath.Join(dev, "vendor"))
		if err != nil || strings.TrimSpace(string(vendor)) != "0x8086" {
			continue
		}
		drv, err := os.Readlink(filepath.Join(dev, "driver"))
		if err != nil || filepath.Base(drv) != "xe" {
			continue
		}
		pdev, err := filepath.EvalSymlinks(dev)
		if err != nil {
			continue
		}
		seen[filepath.Base(pdev)] = true
	}
	cards := make([]string, 0, len(seen))
	for c := range seen {
		cards = append(cards, c)
	}
	sort.Strings(cards)
	return cards
}

// scan walks /proc/*/fdinfo once and aggregates per-card client counters.
// Procfs entries vanish mid-scan; every read error is simply skipped.
func scan(cards []string) map[string]cardSample {
	watch := map[string]bool{}
	for _, c := range cards {
		watch[c] = true
	}
	// (pdev, client-id) -> sample; later fds of the same client overwrite
	// with near-identical counters, which is fine.
	clients := map[string]clientSample{}

	procs, _ := os.ReadDir("/proc")
	for _, p := range procs {
		if !p.IsDir() || !isNumeric(p.Name()) {
			continue
		}
		fdinfoDir := filepath.Join("/proc", p.Name(), "fdinfo")
		fds, err := os.ReadDir(fdinfoDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			raw, err := os.ReadFile(filepath.Join(fdinfoDir, fd.Name()))
			if err != nil {
				continue
			}
			s := parseFdinfo(string(raw))
			if s == nil || !watch[s.pdev] {
				continue
			}
			clients[s.pdev+"/"+s.clientID] = s.clientSample
		}
	}

	out := map[string]cardSample{}
	for key, cs := range clients {
		pdev := strings.SplitN(key, "/", 2)[0]
		agg, ok := out[pdev]
		if !ok {
			agg = cardSample{cycles: map[string]uint64{}, totalCycles: map[string]uint64{}}
		}
		agg.usedKiB += cs.residentKiB
		for class, v := range cs.cycles {
			agg.cycles[class] += v
		}
		for class, v := range cs.totalCycles {
			if v > agg.totalCycles[class] {
				agg.totalCycles[class] = v
			}
		}
		out[pdev] = agg
	}
	return out
}

type parsedClient struct {
	pdev     string
	clientID string
	clientSample
}

// parseFdinfo extracts the DRM fields from one fdinfo blob. Returns nil for
// fds that are not DRM clients.
func parseFdinfo(raw string) *parsedClient {
	c := parsedClient{clientSample: clientSample{
		cycles:      map[string]uint64{},
		totalCycles: map[string]uint64{},
	}}
	for _, line := range strings.Split(raw, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch {
		case key == "drm-pdev":
			c.pdev = val
		case key == "drm-client-id":
			c.clientID = val
		case key == "drm-resident-vram0":
			// "32677152 KiB"
			f := strings.Fields(val)
			if len(f) > 0 {
				if v, err := strconv.ParseInt(f[0], 10, 64); err == nil {
					c.residentKiB = v
				}
			}
		case strings.HasPrefix(key, "drm-cycles-"):
			if v, err := strconv.ParseUint(val, 10, 64); err == nil {
				c.cycles[strings.TrimPrefix(key, "drm-cycles-")] = v
			}
		case strings.HasPrefix(key, "drm-total-cycles-"):
			if v, err := strconv.ParseUint(val, 10, 64); err == nil {
				c.totalCycles[strings.TrimPrefix(key, "drm-total-cycles-")] = v
			}
		}
	}
	if c.pdev == "" || c.clientID == "" {
		return nil
	}
	return &c
}

// busyPct computes one card's utilisation between two scans: the busiest
// engine class's Δcycles / Δtotal-cycles. Nil when there is no usable
// delta yet (first scan, client churn, idle card with no clients).
func busyPct(cur, prev cardSample) *int {
	best := -1
	for class, curTotal := range cur.totalCycles {
		prevTotal, ok := prev.totalCycles[class]
		if !ok || curTotal <= prevTotal {
			continue
		}
		curC, prevC := cur.cycles[class], prev.cycles[class]
		if curC < prevC {
			// Client set changed (a summed counter went backwards):
			// this class's delta is meaningless this round.
			continue
		}
		pct := int(100 * (curC - prevC) / (curTotal - prevTotal))
		if pct > 100 {
			pct = 100
		}
		if pct > best {
			best = pct
		}
	}
	if best < 0 {
		return nil
	}
	return &best
}

func publish(cards []string, cur, prev map[string]cardSample) {
	snap := snapshot{TS: time.Now().Unix()}
	var totalKiB int64
	zero := 0
	for _, pdev := range cards {
		c := cur[pdev] // zero value for idle cards with no clients
		g := gpuOut{PDev: pdev, UsedKiB: c.usedKiB}
		if len(c.totalCycles) == 0 {
			// No DRM clients at all: the card is provably idle.
			g.BusyPct = &zero
		} else {
			g.BusyPct = busyPct(c, prev[pdev])
		}
		totalKiB += c.usedKiB
		snap.GPUs = append(snap.GPUs, g)
	}

	if raw, err := json.Marshal(snap); err == nil {
		writeAtomic(filepath.Join(outDir, "gpus.json"), append(raw, '\n'))
	}
	// Legacy aggregate for the xpu-smi shim / pre-gpus.json agents.
	writeAtomic(filepath.Join(outDir, "used_kib"), []byte(strconv.FormatInt(totalKiB, 10)+"\n"))
}

func writeAtomic(path string, data []byte) {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
