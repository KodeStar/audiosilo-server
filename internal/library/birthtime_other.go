//go:build !darwin && !linux

package library

import "os"

// lstatAdded is addedAt's time read from an lstat already done: the mtime (no
// portable birth time on other platforms).
func lstatAdded(info os.FileInfo) int64 { return info.ModTime().Unix() }

// addedFrom is a file's addedAt from discovery's lstat of it (found): no
// system call.
func addedFrom(_ string, found int64) (string, bool) { return formatAdded(found), true }
