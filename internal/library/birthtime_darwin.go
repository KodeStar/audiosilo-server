//go:build darwin

package library

import (
	"os"
	"syscall"
	"time"
)

// birthTime returns the file's creation (birth) time from the stat struct that
// the os.FileInfo already carries - no extra syscall needed on Darwin.
func birthTime(_ string, info os.FileInfo) (time.Time, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(st.Birthtimespec.Sec, st.Birthtimespec.Nsec), true
}
