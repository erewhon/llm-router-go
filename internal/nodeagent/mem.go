package nodeagent

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// memUsageGB returns used and total system RAM (in GB) from /proc/meminfo.
// "used" is MemTotal-MemAvailable, the same figure `free`/`top` call used —
// it counts what's genuinely committed (the resident inference process plus
// the OS), excluding reclaimable page cache. On a dedicated CPU inference
// node like hekaton this tracks the memory the loaded LLM is holding.
// Linux-only, matching disk.go.
func memUsageGB(path string) (used, total float64, err error) {
	if path == "" {
		path = "/proc/meminfo"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	var totalKB, availKB int
	haveTotal, haveAvail := false, false
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			totalKB, haveTotal = scanMemKB(line), true
		case strings.HasPrefix(line, "MemAvailable:"):
			availKB, haveAvail = scanMemKB(line), true
		}
	}
	if !haveTotal || !haveAvail {
		return 0, 0, fmt.Errorf("mem: /proc/meminfo missing MemTotal/MemAvailable")
	}
	const kbPerGB = 1024 * 1024
	total = roundGB(float64(totalKB) / kbPerGB)
	used = roundGB(float64(totalKB-availKB) / kbPerGB)
	return used, total, nil
}

// scanMemKB parses a /proc/meminfo line shaped "Key:   12345 kB".
func scanMemKB(line string) int {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, _ := strconv.Atoi(fields[1])
	return v
}
