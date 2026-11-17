//go:build !unix

package metadata

import "os"

// openFlags is a plain read: no FIFO here waits for a writer.
const openFlags = os.O_RDONLY

// blockingReads has nothing to clear.
func blockingReads(*os.File) error { return nil }
