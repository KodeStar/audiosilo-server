package meta

import (
	"context"
	"encoding/json"
	"time"
)

// The persistent second level behind the in-memory cache. The memory cache
// alone forgets everything on a restart, so every book a player opens after an
// upgrade or a container recycle is a cold fan-out to the shared community
// service again, and a restart during a metaserve outage leaves every book
// without its enrichment until the upstream is back. A Store keeps the same
// answers on disk (the server's SQLite, via catalog) so a restarted server
// serves them warm, and so a positive answer outlives an outage.
//
// What is persisted is a subset of what the memory cache holds, under the same
// keys and with the same TTLs:
//   - enrichments (the a:/i: key spaces): positive, "no match" and incomplete
//     envelopes (the last for errorTTL only, exactly as in memory);
//   - works (the w: key space): positive answers only. A work id is chosen by the
//     caller, so persisting its misses would let any signed-in user grow the
//     table with ids nobody holds;
//   - never a transport error (an outage is not an answer worth keeping across a
//     restart), and never the l: key space (the console's lookups are cheap and
//     its quota is memory's concern).
//
// Reads are read-through: a memory miss consults the store, and a row that is
// still fresh is served and warms memory for what is left of its TTL. A row past
// its TTL is not served while the upstream answers, but a POSITIVE one is the
// fallback when the upstream fails (not the caller's own cancellation): however
// stale, a day-old cast list beats a 502. It is then held in memory for errorTTL
// only, so the upstream is retried soon.
//
// The rows are derived and rebuildable - a cache, not durable user state: losing
// the table costs one upstream fan-out per book, nothing more.

// Store is where the Service persists its cache entries. Implementations are
// best effort: a failed Load reads as a miss and a failed Save is dropped, so a
// broken store degrades to memory-only caching and never fails a lookup (an
// implementation logs its own failures). A nil Store is memory only.
type Store interface {
	// Load returns key's row; ok=false when there is none.
	Load(ctx context.Context, key string) (e StoredEntry, ok bool, err error)
	// Save inserts or replaces key's row.
	Save(ctx context.Context, e StoredEntry) error
}

// StoredEntry is one persisted cache entry.
type StoredEntry struct {
	Key string
	// Version is the payload's format (storeVersion when written by this
	// build); a row of any other version is ignored on read.
	Version int
	// Source is the metaserve base URL the answer came from. A row from another
	// source is ignored on read: its ids and web_urls belong to that service, so
	// an admin pointing base_url elsewhere never sees the old service's answers.
	Source string
	// Payload is the JSON of the cached *Enrichment or *MetaWork; empty for a
	// cached "no match".
	Payload []byte
	// Expires is when the entry stops being fresh (the memory TTL's expiry).
	Expires time.Time
}

// storeVersion is the format of a persisted payload: the JSON of this build's
// Enrichment / MetaWork. Bump it when a change to those shapes, or to what
// compose puts in them, means an old row would serve an envelope this build
// would not produce (a new field that a row written before it would lack, for
// the whole positive TTL or, during an outage, indefinitely).
const storeVersion = 1

// storeTimeout bounds one store read or write on the request path. The store is
// a local SQLite file, so this only bites when its single writer is busy
// (a scan's batch); a cache write is never worth holding a /meta response for.
const storeTimeout = 2 * time.Second

// SetStore gives the Service a persistent second level (see Store). Call it
// once, right after NewService and before the Service is used; nil (the
// default) keeps the cache in memory only.
func (s *Service) SetStore(st Store) { s.store = st }

// loadStored reads key's persisted row as a *T. ok is false when there is no
// store or no row, when the row is of another format version or source, or when
// its payload does not decode (each reads as a plain miss). On ok, a nil value
// is a persisted "no match" and expires is the row's freshness deadline.
func loadStored[T any](ctx context.Context, s *Service, key string) (value *T, expires time.Time, ok bool) {
	if s.store == nil {
		return nil, time.Time{}, false
	}
	lctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	e, found, err := s.store.Load(lctx, key)
	if err != nil || !found || e.Version != storeVersion || e.Source != s.baseURL {
		return nil, time.Time{}, false
	}
	if len(e.Payload) == 0 {
		return nil, e.Expires, true
	}
	var v T
	if err := json.Unmarshal(e.Payload, &v); err != nil {
		return nil, time.Time{}, false
	}
	return &v, e.Expires, true
}

// saveStored persists key's answer for ttl: value's JSON, or a "no match" for a
// nil value. The write runs detached from the caller's cancellation (the answer
// is already in memory either way, and a player navigating away must not lose
// it for the next restart), bounded by storeTimeout.
func saveStored[T any](ctx context.Context, s *Service, key string, value *T, ttl time.Duration) {
	if s.store == nil {
		return
	}
	var body []byte
	if value != nil {
		b, err := json.Marshal(value)
		if err != nil {
			return
		}
		body = b
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()
	_ = s.store.Save(sctx, StoredEntry{
		Key:     key,
		Version: storeVersion,
		Source:  s.baseURL,
		Payload: body,
		Expires: s.now().Add(ttl),
	})
}

// storedHit is loadStored's answer as Enrich and Work consume it: fresh says the
// row is still within its TTL (served and warmed into memory); stale holds a
// POSITIVE row past it, kept as the outage fallback. A stale "no match" is
// neither: it is simply asked again.
type storedHit[T any] struct {
	fresh   bool
	value   *T // the fresh answer (nil for a fresh "no match")
	expires time.Time
	stale   *T
}

// readStored is the read-through step after a memory miss.
func readStored[T any](ctx context.Context, s *Service, key string) storedHit[T] {
	v, expires, ok := loadStored[T](ctx, s, key)
	switch {
	case !ok:
		return storedHit[T]{}
	case s.now().Before(expires):
		return storedHit[T]{fresh: true, value: v, expires: expires}
	default:
		return storedHit[T]{stale: v}
	}
}
