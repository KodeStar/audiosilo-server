package mounts

import (
	"bufio"
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
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if point, _, ok := ParseLine(sc.Text()); ok {
			out = append(out, point)
		}
	}
	return out, sc.Err()
}
