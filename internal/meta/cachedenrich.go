package meta

import (
	"context"
	"errors"

	"github.com/kodestar/audiosilo-server/pkg/match"
)

// CachedEnrichment is a book's enrichment from the cache alone, never the
// upstream (CachedWorkID's rule, for the whole envelope): memory first, else its
// persisted row, fresh or stale. ok is true when the cache holds an answer: the
// envelope, or nil for a cached "no match"; a miss, a cached failure and no
// identifier are (nil, false). For a question over many books at once (the
// Bridge's estimate looks at a series' earlier books), where a lookup each would
// be one upstream call per book.
//
// The returned *Enrichment is shared with the cache: never modify it.
func (s *Service) CachedEnrichment(ctx context.Context, asin, isbn string) (env *Enrichment, ok bool) {
	key := cacheKey(match.NormalizeASIN(asin), match.NormalizeISBN(isbn))
	if key == "" {
		return nil, false
	}
	if e, hit, err := cacheGet[Enrichment](s.cache, key); hit {
		switch {
		case err == nil:
			return e, true
		case errors.Is(err, ErrNotFound):
			return nil, true
		}
	}
	stored, expires, _ := readStored[Enrichment](ctx, s, key)
	if expires.IsZero() { // no row (or one of another source or format)
		return nil, false
	}
	return stored, true
}

// HasCharacters and HasRecaps report whether a matched envelope's work carries
// community characters, and recaps (a recap list or the whole-book summary).
func (e *Enrichment) HasCharacters() bool {
	return e != nil && e.Work != nil && len(e.Work.Characters) > 0
}

func (e *Enrichment) HasRecaps() bool {
	return e != nil && e.Work != nil && (len(e.Work.Recaps) > 0 || e.Work.RecapSummary != nil)
}
