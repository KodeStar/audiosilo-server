//go:build linux

package library

import (
	"errors"
	"io/fs"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// statxBirth is the file's creation (birth) time via statx. Btime needs a path
// (it isn't in the standard stat struct on Linux) and isn't recorded by every
// filesystem: ok is then false, with no error, and the mtime stands in.
func statxBirth(absPath string) (time.Time, bool, error) {
	var stx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, absPath, 0, unix.STATX_BTIME, &stx); err != nil {
		return time.Time{}, false, err
	}
	if stx.Mask&unix.STATX_BTIME == 0 {
		return time.Time{}, false, nil // filesystem doesn't record a birth time
	}
	return time.Unix(stx.Btime.Sec, int64(stx.Btime.Nsec)), true, nil
}

// lstatAdded is the part of addedAt's time an lstat already done tells on
// Linux, in Unix seconds: the mtime, which stands in for a birth time the
// filesystem doesn't record (Linux's stat struct has none).
func lstatAdded(info os.FileInfo) int64 { return info.ModTime().Unix() }

// addedFrom is a file's addedAt from one statx and discovery's lstat of it
// (found: its mtime), not a second lstat. A file statx finds gone (or a link to
// nothing) is left to addedAt's lstat (ok false), as before; any other statx
// failure reads as no birth time, the mtime standing in.
func addedFrom(absPath string, found int64) (string, bool) {
	bt, ok, err := statxBirth(absPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", false
	case ok && !bt.IsZero():
		return formatAdded(bt.Unix()), true
	}
	return formatAdded(found), true
}
