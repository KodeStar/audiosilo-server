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

// maxLookupEntries bounds the lookup key space: an identifier's lookup answer
// (its work and the recording it matched), written and read only by the admin
// console's Series cards (Service.WorkIDs), for books no enrichment already
// answers. Enrich never writes here (its enrichment carries the work id), so the
// quota is the console's alone. Those keys come from books the server holds, but
// one Series page can resolve every matched book of a large library, and that
// must not push out the enrichments the players read. It leaves at least
// cacheCap - maxWorkEntries - maxLookupEntries entries to the enrichment key
// spaces, and a lookup evicted early only costs one small GET again.
const maxLookupEntries = 1024

// keyspace namespaces the one shared cache so the feature's lookup kinds - an
// asin/isbn enrichment, a work-id fetch and an identifier's lookup - can never
// read each other's entries. Every key in the cache is minted through
// keyspace.key, so these prefixes are declared in exactly one place.
type keyspace string

const (
	nsASIN keyspace = "a:"
	nsISBN keyspace = "i:"
	nsWork keyspace = "w:"
	// nsLookup holds an identifier's lookup answer, keyed by its enrichment key
	// ("l:a:<asin>" / "l:i:<isbn>").
	nsLookup keyspace = "l:"
)

// quotas are the key spaces with their own share of the cache (see
// maxWorkEntries, maxLookupEntries). A new entry in one of them first makes room
// WITHIN its own key space, and at cacheCap gives up its own entries first (see
// evictLocked), so a flood of keys there evicts its own entries - it reaches the
// enrichment entries, which are bounded by cacheCap alone, only when its key
// space holds nothing left to give up.
var quotas = map[keyspace]int{nsWork: maxWorkEntries, nsLookup: maxLookupEntries}

// key mints the cache key for one lookup id in this key space.
func (ns keyspace) key(id string) string { return string(ns) + id }

// owns reports whether key was minted in this key space. The prefixes are
// distinct single letters, so this is an exact partition of the key set.
func (ns keyspace) owns(key string) bool { return strings.HasPrefix(key, string(ns)) }

// quotaSpace returns the key space with a quota that owns key, ok=false when
// none does. Every prefix is one letter and a colon (see owns), so the key's
// first two bytes name its key space: one map lookup per store or delete, not a
// scan of quotas.
func quotaSpace(key string) (ns keyspace, ok bool) {
	if len(key) < 2 {
		return "", false
	}
	ns = keyspace(key[:2])
	if _, ok = quotas[ns]; !ok {
		return "", false
	}
	return ns, true
}

// cacheEntry is one memoised lookup outcome, in exactly one of three states: a
// non-nil err marks a cached transport error; a nil value with a nil err marks a
// cached "no match" (ErrNotFound); a non-nil value marks a cached positive
// result. value holds *Enrichment, *MetaWork or *upstreamLookup depending on the
// key space, and is read only through cacheGet (which treats a wrong-typed hit
// as a miss), so the kinds share one bounded memory budget without the payload's
// meaning depending on a key-prefix convention.
type cacheEntry struct {
	value  any
	err    error
	expiry time.Time
}

// cache is a bounded in-memory TTL cache. The clock is injectable so tests can
// drive TTL expiry deterministically.
type cache struct {
	mu sync.Mutex
	m  map[string]cacheEntry
	// counts is how many entries (live or not yet dropped) each key space of
	// quotas holds, so a store only scans the map once its space is full.
	counts map[keyspace]int
	now    func() time.Time
}

func newCache(now func() time.Time) *cache {
	if now == nil {
		now = time.Now
	}
	return &cache{m: make(map[string]cacheEntry), counts: make(map[keyspace]int), now: now}
}

// cacheGet resolves a cached lookup of a *T payload. hit is false on a miss, on
// an expired entry, and - defensively - on a live entry whose payload is not a
// *T: a mis-namespaced key must degrade to an extra upstream fetch, not
// masquerade as a cached negative. On a hit the error is nil (positive result),
// ErrNotFound (cached "no match") or the cached transport error.
func cacheGet[T any](c *cache, key string) (result *T, hit bool, err error) {
	e, ok := c.get(key)
	switch {
	case !ok:
		return nil, false, nil
	case e.err != nil:
		return nil, true, e.err
	case e.value == nil:
		return nil, true, ErrNotFound
	}
	v, ok := e.value.(*T)
	if !ok {
		return nil, false, nil
	}
	return v, true, nil
}

// cachePut caches a positive result for ttl (positiveTTL, or errorTTL for an
// incomplete one). A nil result is a "no match" and is stored as such (see
// payload).
func cachePut[T any](c *cache, key string, result *T, ttl time.Duration) {
	c.store(key, cacheEntry{value: payload(result), expiry: c.now().Add(ttl)})
}

// get returns the live entry for key, or ok=false when it is absent or expired
// (an expired entry is dropped on read). Callers go through cacheGet.
func (c *cache) get(key string) (cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return cacheEntry{}, false
	}
	if !c.now().Before(e.expiry) {
		c.deleteLocked(key)
		return cacheEntry{}, false
	}
	return e, true
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
	if _, exists := c.m[key]; !exists {
		ns, quota := quotaSpace(key)
		if quota && c.counts[ns] >= quotas[ns] {
			c.enforceQuotaLocked(ns)
		}
		if len(c.m) >= cacheCap {
			c.evictLocked(ns)
		}
		if quota {
			c.counts[ns]++
		}
	}
	c.m[key] = e
}

// deleteLocked drops key, which must be present, keeping counts in step. Every
// delete goes through here. Callers hold c.mu.
func (c *cache) deleteLocked(key string) {
	delete(c.m, key)
	if ns, ok := quotaSpace(key); ok {
		c.counts[ns]--
	}
}

// enforceQuotaLocked frees a slot in the full ns key space for one new entry: it
// drops expired entries of ns first, then arbitrary live ones until fewer than
// its quota remain. Only keys of ns are ever touched. Callers hold c.mu.
func (c *cache) enforceQuotaLocked(ns keyspace) {
	now := c.now()
	for k, e := range c.m {
		if ns.owns(k) && !now.Before(e.expiry) {
			c.deleteLocked(k)
		}
	}
	for k := range c.m {
		if c.counts[ns] < quotas[ns] {
			break
		}
		if ns.owns(k) {
			c.deleteLocked(k)
		}
	}
}

// evictLocked frees room: it first drops every expired entry, and if the map is
// still at capacity, deletes arbitrary entries until it is under the cap. A new
// entry of a quota key space (ns) frees the room from its own key space first,
// so its flood never reaches the enrichment entries while it has entries of its
// own to give up; ns is "" for the enrichment key spaces. Callers hold c.mu.
func (c *cache) evictLocked(ns keyspace) {
	now := c.now()
	for k, e := range c.m {
		if !now.Before(e.expiry) {
			c.deleteLocked(k)
		}
	}
	if _, quota := quotas[ns]; quota {
		for k := range c.m {
			if len(c.m) < cacheCap || c.counts[ns] == 0 {
				break
			}
			if ns.owns(k) {
				c.deleteLocked(k)
			}
		}
	}
	for k := range c.m {
		if len(c.m) < cacheCap {
			break
		}
		c.deleteLocked(k)
	}
}
