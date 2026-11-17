//go:build darwin

package library

import (
	"os"
	"syscall"
)

// lstatAdded is addedAt's time read from an lstat already done, in Unix
// seconds: the birth time Darwin's stat struct carries (no extra system call),
// else the mtime, as for a birth time that is none (birthKnown: 0 over an
// rclone NFS share).
func lstatAdded(info os.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && birthKnown(st.Birthtimespec.Sec) {
		return st.Birthtimespec.Sec
	}
	return info.ModTime().Unix()
}

// addedFrom is a file's addedAt from discovery's lstat of it (found,
// lstatAdded's), which on Darwin is all of it: no system call.
func addedFrom(_ string, found int64) (string, bool) { return formatAdded(found), true }
