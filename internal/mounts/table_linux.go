package mounts

import (
	"os"
)

// readTable reads the mount points in /proc/self/mountinfo, which lists them
// without asking any of them (a hung NFS or SMB mount included).
func readTable() ([]string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	err = Scan(f, func(point, _ string) { out = append(out, point) })
	return out, err
}
