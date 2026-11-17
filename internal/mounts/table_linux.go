package mounts

import (
	"os"
)

// readTable reads the mounts in /proc/self/mountinfo, which lists them without
// asking any of them (a hung NFS or SMB mount included).
func readTable() ([]Mount, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Mount
	err = Scan(f, func(m Mount) { out = append(out, m) })
	return out, err
}
