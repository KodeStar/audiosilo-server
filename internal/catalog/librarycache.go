package catalog

import (
	"slices"
	"sync"
)

// libraryCache keeps GetLibrary's rows at one write generation (Generation): a
// row read after the generation was g is current while it is still g, as the
// api's list cache reasons, and any write moves it on, emptying the cache. Every
// path-addressed request reads its library (twice: its scope, then the library
// itself), so a cover or a stream skips those reads while nothing is written.
type libraryCache struct {
	mu   sync.Mutex
	gen  uint64
	rows map[int64]Library
}

// get is a copy of library id's row cached at gen.
func (lc *libraryCache) get(gen uint64, id int64) (*Library, bool) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if lc.gen != gen {
		return nil, false
	}
	l, ok := lc.rows[id]
	if !ok {
		return nil, false
	}
	return copyLibrary(l), true
}

// put keeps a copy of l, read after the generation was gen. A row read at an
// older generation than the cache's is dropped; a newer one empties the cache.
func (lc *libraryCache) put(gen uint64, l *Library) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	switch {
	case gen < lc.gen:
		return
	case gen > lc.gen || lc.rows == nil:
		lc.gen, lc.rows = gen, map[int64]Library{}
	}
	lc.rows[l.ID] = *copyLibrary(*l)
}

// copyLibrary is l with its own ignore patterns, so no caller shares the cache's.
func copyLibrary(l Library) *Library {
	l.IgnorePatterns = slices.Clone(l.IgnorePatterns)
	return &l
}
