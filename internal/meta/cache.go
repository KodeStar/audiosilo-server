package meta

import (
	"strings"
	"sync"
	"time"
)

// Cache TTLs for a composed lookup result, keyed on the book's asin/isbn. The
// key point is that a down or empty upstream is not hammered: a positive match
// is held a day, a "no match" an hour, and a transport error only a couple of
// minutes (long enough to shield a burst, short enough to recover quickly).
const (
	positiveTTL = 24 * time.Hour
	notFoundTTL = 1 * time.Hour
	errorTTL    = 2 * time.Minute
)

// cacheCap bounds the number of live cache entries. A modest cap keeps memory
// flat on a large library; eviction is best-effort (expired-first, then
// arbitrary) since the cache is a latency optimisation, not a store of record.
const cacheCap = 2048

// maxWorkEntries bounds the share of the cache the work key space may occupy.
// The work id is the ONLY freely enumerable cache key on this feature (any
// signed-in caller picks it: GET /meta/work?id=), so without a per-key-space
// quota a flood of distinct ids would evict every enrichment entry (a:/i:) and
// turn each book view back into a full upstream fan-out. A few hundred entries
// comfortably covers a reader walking a series' rails, while leaving the bulk of
// cacheCap to the enrichment key spaces.
const maxWorkEntries = 256

// keyspace namespaces the one shared cache so the feature's two lookup kinds -
// an asin/isbn enrichment and a work-id fetch - can never read each other's
// entries. Every key in the cache is minted through keyspace.key, so these
// prefixes are declared in exactly one place.
type keyspace string

const (
	nsASIN keyspace = "a:"
	nsISBN keyspace = "i:"
	nsWork keyspace = "w:"
)

// key mints the cache key for one lookup id in this key space.
func (ns keyspace) key(id string) string { return string(ns) + id }

// owns reports whether key was minted in this key space. The prefixes are
// distinct single letters, so this is an exact partition of the key set.
func (ns keyspace) owns(key string) bool { return strings.HasPrefix(key, string(ns)) }

// cacheEntry is one memoised lookup outcome, in exactly one of three states: a
// non-nil err marks a cached transport error; a nil value with a nil err marks a
// cached "no match" (ErrNotFound); a non-nil value marks a cached positive
// result. value holds *Enrichment or *MetaWork depending on the key space, and
// is read only through the typed accessors below (which treat a wrong-typed hit
// as a miss), so the two kinds share one bounded memory budget without the
// payload's meaning depending on a key-prefix convention.
type cacheEntry struct {
	value  any
	err    error
	expiry time.Time
}

// cache is a bounded in-memory TTL cache. The clock is injectable so tests can
// drive TTL expiry deterministically.
type cache struct {
	mu  sync.Mutex
	m   map[string]cacheEntry
	now func() time.Time
}

func newCache(now func() time.Time) *cache {
	if now == nil {
		now = time.Now
	}
	return &cache{m: make(map[string]cacheEntry), now: now}
}

// getEnrichment resolves a cached enrichment lookup. hit is false on a miss, on
// an expired entry, and - defensively - on a live entry whose payload is not an
// *Enrichment: a mis-namespaced key must degrade to an extra upstream fetch, not
// masquerade as a cached negative. On a hit the error is nil (positive result),
// ErrNotFound (cached "no match") or the cached transport error.
func (c *cache) getEnrichment(key string) (result *Enrichment, hit bool, err error) {
	e, ok := c.get(key)
	switch {
	case !ok:
		return nil, false, nil
	case e.err != nil:
		return nil, true, e.err
	case e.value == nil:
		return nil, true, ErrNotFound
	}
	v, ok := e.value.(*Enrichment)
	if !ok {
		return nil, false, nil
	}
	return v, true, nil
}

// getWork resolves a cached work-id lookup. Same contract as getEnrichment,
// including treating a wrong-typed payload as a miss.
func (c *cache) getWork(key string) (work *MetaWork, hit bool, err error) {
	e, ok := c.get(key)
	switch {
	case !ok:
		return nil, false, nil
	case e.err != nil:
		return nil, true, e.err
	case e.value == nil:
		return nil, true, ErrNotFound
	}
	v, ok := e.value.(*MetaWork)
	if !ok {
		return nil, false, nil
	}
	return v, true, nil
}

// get returns the live entry for key, or ok=false when it is absent or expired
// (an expired entry is dropped on read). Callers go through the typed accessors.
func (c *cache) get(key string) (cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return cacheEntry{}, false
	}
	if !c.now().Before(e.expiry) {
		delete(c.m, key)
		return cacheEntry{}, false
	}
	return e, true
}

// putEnrichment caches a positive enrichment match (24h). A nil result is a "no
// match" and is stored as such (see payload).
func (c *cache) putEnrichment(key string, result *Enrichment, ttl time.Duration) {
	c.store(key, cacheEntry{value: payload(result), expiry: c.now().Add(ttl)})
}

// putWork caches a positive work-id match (24h). A nil work is a "no such work"
// and is stored as a miss marker (see payload).
func (c *cache) putWork(key string, work *MetaWork, ttl time.Duration) {
	c.store(key, cacheEntry{value: payload(work), expiry: c.now().Add(ttl)})
}

// putMiss caches a "no match" marker (1h) - a nil payload with a nil error.
func (c *cache) putMiss(key string, ttl time.Duration) {
	c.store(key, cacheEntry{expiry: c.now().Add(ttl)})
}

// putError caches a transport error marker (2min) so a down upstream isn't
// re-hit on every request.
func (c *cache) putError(key string, err error) {
	c.store(key, cacheEntry{err: err, expiry: c.now().Add(errorTTL)})
}

// payload boxes a payload pointer for cacheEntry.value, mapping a nil pointer to
// an untyped nil. Without this a nil *Enrichment/*MetaWork would box as a
// typed-nil interface, which compares non-nil and would read back as a positive
// result carrying a nil payload.
func payload[T any](p *T) any {
	if p == nil {
		return nil
	}
	return p
}

func (c *cache) store(key string, e cacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, exists := c.m[key]
	if !exists {
		// A new work entry first makes room WITHIN its own key space, so a flood
		// of caller-chosen work ids can only ever evict other work entries - it
		// can never reach the enrichment entries (see maxWorkEntries).
		if nsWork.owns(key) {
			c.enforceWorkQuotaLocked()
		}
		if len(c.m) >= cacheCap {
			c.evictLocked()
		}
	}
	c.m[key] = e
}

// enforceWorkQuotaLocked frees a slot in the work key space for one new entry:
// it drops expired work entries first, then arbitrary live ones until fewer than
// maxWorkEntries remain. Only work keys are ever touched. Callers hold c.mu.
func (c *cache) enforceWorkQuotaLocked() {
	now := c.now()
	live := make([]string, 0, maxWorkEntries)
	for k, e := range c.m {
		if !nsWork.owns(k) {
			continue
		}
		if !now.Before(e.expiry) {
			delete(c.m, k)
			continue
		}
		live = append(live, k)
	}
	for i := 0; len(live)-i >= maxWorkEntries; i++ {
		delete(c.m, live[i])
	}
}

// evictLocked frees room: it first drops every expired entry, and if the map is
// still at capacity, deletes arbitrary entries until it is under the cap. Callers
// hold c.mu.
func (c *cache) evictLocked() {
	now := c.now()
	for k, e := range c.m {
		if !now.Before(e.expiry) {
			delete(c.m, k)
		}
	}
	for k := range c.m {
		if len(c.m) < cacheCap {
			break
		}
		delete(c.m, k)
	}
}
