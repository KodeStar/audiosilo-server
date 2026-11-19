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
