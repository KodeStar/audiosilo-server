//go:build !windows

package diskspace

import "golang.org/x/sys/unix"

// Of reports the size of the filesystem holding path and the space free to the
// server on it.
func Of(path string) (total, free uint64, ok bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	// The field types differ by OS (Bsize is int64 on Linux, uint32 on macOS).
	bsize := uint64(st.Bsize)
	return uint64(st.Blocks) * bsize, uint64(st.Bavail) * bsize, true
}
