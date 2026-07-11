package meta

import (
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

// cacheEntry is one memoised composition outcome. Exactly one of result/err is
// meaningful: a non-nil err marks a cached transport error, a nil result with
// nil err marks a cached "no match" (ErrNotFound), and a non-nil result marks a
// cached positive match.
type cacheEntry struct {
	result *Enrichment
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

// get returns the live entry for key, or ok=false when it is absent or expired
// (an expired entry is dropped on read).
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

// putResult caches a positive match (24h) or a "no match" (1h, result nil).
func (c *cache) putResult(key string, result *Enrichment, ttl time.Duration) {
	c.store(key, cacheEntry{result: result, expiry: c.now().Add(ttl)})
}

// putError caches a transport error marker (2min) so a down upstream isn't
// re-hit on every request.
func (c *cache) putError(key string, err error) {
	c.store(key, cacheEntry{err: err, expiry: c.now().Add(errorTTL)})
}

func (c *cache) store(key string, e cacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.m[key]; !exists && len(c.m) >= cacheCap {
		c.evictLocked()
	}
	c.m[key] = e
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
