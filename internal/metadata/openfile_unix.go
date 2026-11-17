//go:build unix

package metadata

import (
	"os"
	"syscall"
)

// openFlags opens a FIFO without waiting for a writer (O_NONBLOCK), so
// openRegular can turn it away; a regular file's reads ignore the flag, which
// blockingReads clears all the same.
const openFlags = os.O_RDONLY | syscall.O_NONBLOCK

// blockingReads clears openFlags' O_NONBLOCK on f, a regular file.
func blockingReads(f *os.File) error { return syscall.SetNonblock(int(f.Fd()), false) }
