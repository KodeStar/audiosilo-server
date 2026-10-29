//go:build !darwin && !linux

package library

import (
	"os"
	"time"
)

// birthTime has no portable source on other platforms; callers fall back to mtime.
func birthTime(_ string, _ os.FileInfo) (time.Time, bool) {
	return time.Time{}, false
}

// lstatAdded is addedAt's time read from an lstat already done: the mtime.
func lstatAdded(info os.FileInfo) int64 { return info.ModTime().Unix() }

// addedFrom is a file's addedAt from discovery's lstat of it (found): no
// system call.
func addedFrom(_ string, found int64) (string, bool) { return formatAdded(found), true }
