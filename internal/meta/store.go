package meta

import (
	"context"
	"encoding/json"
	"time"
)

// The persistent second level behind the in-memory cache (the server's SQLite,
// via catalog), so a restarted server answers warm and a positive answer outlives
// an upstream outage. It holds a subset of the memory cache, under the same keys
// and TTLs:
//   - enrichments (a:/i:): positive, "no match" and incomplete envelopes (the
//     last for errorTTL only, as in memory);
//   - works (w:): positive answers only (a work id is the caller's choice, so its
//     misses would let any signed-in user grow the table), except that a later
//     "no match" replaces a stored work's row;
//   - never a transport error, and never the l: key space.
//
// Reads are read-through (readStored): a fresh row is served and warms memory
// for what is left of its TTL; a POSITIVE row past its TTL is served only when
// the upstream fails (not on the caller's own cancellation), then held in memory
// for errorTTL. The rows are a rebuildable cache, not durable user state.

// Store is where the Service persists its cache entries. It is best effort: a
// failed Load reads as a miss and a failed Save is dropped (an implementation logs
// its own failures), so a broken store degrades to memory-only caching and never
// fails a lookup. A nil Store is memory only.
type Store interface {
	// Load returns key's row; ok=false when there is none or it can't be read.
	Load(ctx context.Context, key string) (e StoredEntry, ok bool)
	// Save inserts or replaces key's row.
	Save(ctx context.Context, e StoredEntry)
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

// storeTimeout bounds one store read on the request path (reads never wait on
// the database's single writer, so this only bites when the file is in trouble).
const storeTimeout = 2 * time.Second

// storeWriteTimeout bounds one store write, which the request waits for. A
// write waits on the single writer, which a scan's batch or a long prune can
// hold, and a cache write is never worth holding a /meta response for: the
// answer is already in memory, so a write cut short costs only the row (and
// the warm start after a restart it would have given).
const storeWriteTimeout = 250 * time.Millisecond

// SetStore gives the Service a persistent second level (see Store). Call it
// once, right after NewService and before the Service is used; nil (the
// default) keeps the cache in memory only.
func (s *Service) SetStore(st Store) { s.store = st }

// readStored is the read-through step after a memory miss: key's persisted row
// as a *T (nil for a persisted "no match"), its freshness deadline, and whether it
// is still within it. A row past it is not served while the upstream answers; a
// positive one is the fallback when the upstream fails. No store or row, a row of
// another format version or source, and a payload that does not decode all read
// as (nil, zero, false).
func readStored[T any](ctx context.Context, s *Service, key string) (value *T, expires time.Time, fresh bool) {
	if s.store == nil {
		return nil, time.Time{}, false
	}
	lctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	e, ok := s.store.Load(lctx, key)
	if !ok || e.Version != storeVersion || e.Source != s.baseURL {
		return nil, time.Time{}, false
	}
	if len(e.Payload) > 0 {
		value = new(T)
		if err := json.Unmarshal(e.Payload, value); err != nil {
			return nil, time.Time{}, false
		}
	}
	return value, e.Expires, s.now().Before(e.Expires)
}

// saveStored persists key's answer for ttl: value's JSON, or a "no match" for a
// nil value. The write runs detached from the caller's cancellation (the answer
// is already in memory either way, and a player navigating away must not lose
// it for the next restart), bounded by storeWriteTimeout.
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
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeWriteTimeout)
	defer cancel()
	s.store.Save(sctx, StoredEntry{
		Key:     key,
		Version: storeVersion,
		Source:  s.baseURL,
		Payload: body,
		Expires: s.now().Add(ttl),
	})
}
