package meta

import (
	"context"

	"github.com/kodestar/audiosilo-server/pkg/match"
)

// CachedEnrichment is a book's enrichment from the cache alone, never the
// upstream (CachedWorkID's rule, for the whole envelope): memory first, else its
// persisted row, fresh or stale. Nil for anything but a cached match: a cached
// "no match", a miss, a cached failure and no identifier alike. A fresh row
// warms memory for the rest of its TTL, as Enrich's read of it does. For a
// question over many books at once (the Bridge's estimate looks at a series'
// earlier books), where a lookup each would be one upstream call per book.
//
// The returned *Enrichment is shared with the cache: never modify it.
func (s *Service) CachedEnrichment(ctx context.Context, asin, isbn string) *Enrichment {
	key := cacheKey(match.NormalizeASIN(asin), match.NormalizeISBN(isbn))
	if key == "" {
		return nil
	}
	if e, hit, err := cacheGet[Enrichment](s.cache, key); hit {
		if err != nil {
			return nil
		}
		return e
	}
	stored, expires, fresh := readStored[Enrichment](ctx, s, key)
	if fresh {
		cachePutUntil(s.cache, key, stored, expires)
	}
	return stored
}

// HasCharacters and HasRecaps report whether a matched envelope's work carries
// community characters, and recaps (a recap list or the whole-book summary).
func (e *Enrichment) HasCharacters() bool {
	return e != nil && e.Work != nil && len(e.Work.Characters) > 0
}

func (e *Enrichment) HasRecaps() bool {
	return e != nil && e.Work != nil && (len(e.Work.Recaps) > 0 || e.Work.RecapSummary != nil)
}
