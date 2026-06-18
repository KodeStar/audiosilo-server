//go:build linux

package library

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// birthTime returns the file's creation (birth) time via statx. Btime needs a
// path (it isn't in the standard stat struct on Linux) and isn't supported by
// every filesystem, so callers fall back to mtime when ok is false.
func birthTime(absPath string, _ os.FileInfo) (time.Time, bool) {
	var stx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, absPath, 0, unix.STATX_BTIME, &stx); err != nil {
		return time.Time{}, false
	}
	if stx.Mask&unix.STATX_BTIME == 0 {
		return time.Time{}, false // filesystem doesn't record a birth time
	}
	return time.Unix(stx.Btime.Sec, int64(stx.Btime.Nsec)), true
}
