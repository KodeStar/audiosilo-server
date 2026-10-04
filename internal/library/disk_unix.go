//go:build !windows

package library

import "golang.org/x/sys/unix"

// diskSpace reports the size and the space free to the server of the
// filesystem holding path.
func diskSpace(path string) (total, free uint64, ok bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	// The field types differ by OS (Bsize is int64 on Linux, uint32 on macOS).
	bsize := uint64(st.Bsize)
	return uint64(st.Blocks) * bsize, uint64(st.Bavail) * bsize, true
}
