//go:build benchharness

package library

import (
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// SetNamesForBench sets b's names from md as a scan does (setNames): for the
// matcher's benchmark harness (internal/matchrun, build tag benchharness).
// Never built into the server.
func SetNamesForBench(b *catalog.Book, md *metadata.Metadata) { setNames(b, md) }
