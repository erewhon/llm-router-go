package nodeagent

import "syscall"

// diskUsageGB returns free and total disk space (in GB) for the filesystem
// containing path. Linux-only; both binaries and the dev env are Linux.
func diskUsageGB(path string) (free, total float64, err error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, 0, err
	}
	block := uint64(s.Bsize) //nolint:unconvert // narrow on some archs
	const gb = 1024 * 1024 * 1024
	free = roundGB(float64(s.Bavail*block) / gb)
	total = roundGB(float64(s.Blocks*block) / gb)
	return free, total, nil
}

// roundGB matches the Python agent's `round(..., 1)`.
func roundGB(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}
