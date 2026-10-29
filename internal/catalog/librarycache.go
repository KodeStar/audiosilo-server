package catalog

import "slices"

// The library cache (Catalog.libs, a store.GenMap) keeps GetLibrary's rows at
// one content generation (ContentGeneration): a row read after the generation
// was g is current while it is still g, and any write but a user-state one
// (which never writes libraries) moves it on, emptying the cache.
// Every path-addressed request reads its library (twice: its scope, then the
// library itself), so a cover or a stream skips those reads while nothing is
// written.

// copyLibrary is l with its own ignore patterns, so no caller shares the cache's.
func copyLibrary(l Library) *Library {
	l.IgnorePatterns = slices.Clone(l.IgnorePatterns)
	return &l
}
