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

// lstatAdded is addedAt's time read from an lstat already done, in Unix
// seconds: the birth time it carries, else the mtime.
func lstatAdded(info os.FileInfo) int64 {
	if bt, ok := birthTime("", info); ok && !bt.IsZero() {
		return bt.Unix()
	}
	return info.ModTime().Unix()
}

// addedFrom is a file's addedAt from discovery's lstat of it (found,
// lstatAdded's), which on Darwin is all of it: no system call.
func addedFrom(_ string, found int64) (string, bool) { return formatAdded(found), true }
