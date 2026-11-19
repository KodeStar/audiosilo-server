//go:build windows

package metadata

import (
	"io/fs"
	"syscall"
	"testing"
)

// TestTransientFaultWindowsCodes: on Windows a share whose session dropped
// fails with Windows' own codes (ERROR_NETNAME_DELETED, ERROR_SEM_TIMEOUT),
// which are transient; a permission (ERROR_ACCESS_DENIED) and a file not
// there (ERROR_FILE_NOT_FOUND) are not.
func TestTransientFaultWindowsCodes(t *testing.T) {
	t.Parallel()
	for errno, want := range map[syscall.Errno]bool{64: true, 121: true, 53: true, 5: false, 2: false} {
		if got := TransientFault(&fs.PathError{Op: "open", Path: `\\nas\books\x.m4b`, Err: errno}); got != want {
			t.Errorf("TransientFault(%d) = %v, want %v", errno, got, want)
		}
	}
}

// TestBadNetpathIsNoFileGone: Go reads ERROR_BAD_NETPATH (53, a share whose
// server is down) as fs.ErrNotExist, but it is a share's fault a retry may get
// past, never the file gone: a ShareFault, a look given up on (AsGaveUp), not
// NotExist. Allowed: ERROR_FILE_NOT_FOUND (2) stays the file gone.
func TestBadNetpathIsNoFileGone(t *testing.T) {
	t.Parallel()
	down := &fs.PathError{Op: "lstat", Path: `\\nas\books\x.m4b`, Err: syscall.Errno(53)}
	if NotExist(down) || !ShareFault(down) || !GaveUp(AsGaveUp(down)) {
		t.Errorf("ERROR_BAD_NETPATH: NotExist %v, ShareFault %v, AsGaveUp given up on %v; want a share's fault, never the file gone",
			NotExist(down), ShareFault(down), GaveUp(AsGaveUp(down)))
	}
	gone := &fs.PathError{Op: "lstat", Path: `\\nas\books\x.m4b`, Err: syscall.Errno(2)}
	if !NotExist(gone) || ShareFault(gone) {
		t.Errorf("ERROR_FILE_NOT_FOUND: NotExist %v, ShareFault %v; want the file gone", NotExist(gone), ShareFault(gone))
	}
}
