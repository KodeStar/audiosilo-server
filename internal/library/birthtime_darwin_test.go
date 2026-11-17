//go:build darwin

package library

import (
	"io/fs"
	"syscall"
	"testing"
	"time"
)

// statInfo is an lstat's answer with the birth time a share gave.
type statInfo struct {
	fs.FileInfo
	mtime time.Time
	birth int64
}

func (i statInfo) ModTime() time.Time { return i.mtime }
func (i statInfo) Sys() any {
	return &syscall.Stat_t{Birthtimespec: syscall.Timespec{Sec: i.birth}}
}

// Darwin's lstat carries the birth time, but one a share keeps none of reads 0
// (an rclone NFS share: every book added 1970-01-01, SRV-15c): that, or any
// time before 1980, is none, and the mtime stands in, so added_at is when the
// file was last written rather than the epoch.
func TestLstatAddedTakesAZeroBirthTimeAsNone(t *testing.T) {
	t.Parallel()
	mtime := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	for _, birth := range []int64{0, -1, time.Date(1979, 6, 1, 0, 0, 0, 0, time.UTC).Unix()} {
		if got := lstatAdded(statInfo{mtime: mtime, birth: birth}); got != mtime.Unix() {
			t.Errorf("lstatAdded with birth time %d = %d, want the mtime %d", birth, got, mtime.Unix())
		}
	}
	born := time.Date(2020, 3, 1, 9, 0, 0, 0, time.UTC).Unix()
	if got := lstatAdded(statInfo{mtime: mtime, birth: born}); got != born {
		t.Errorf("lstatAdded with a real birth time = %d, want it (%d)", got, born)
	}
}
