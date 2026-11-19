//go:build !windows

package metadata

import "syscall"

// platformTransientErrnos are TransientFault's codes of a platform's own
// beside the shared list: none here (Windows' are in transient_windows.go).
var platformTransientErrnos []syscall.Errno
