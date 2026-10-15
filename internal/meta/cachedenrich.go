package meta

import (
	"context"
	"errors"
	"sync"

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

// Coverage is what a book's community work carries: characters, recaps (as
// HasCharacters and HasRecaps read them), and whether that is known. Known is
// false when it couldn't be had right now (a lookup or work fetch that failed):
// unknown, not "none".
type Coverage struct {
	Characters, Recaps, Known bool
}

// Coverages answers Coverage for many books at once (the Bridge's estimate: a
// series batch's brief answers, a book's earlier books), each in input order,
// without a full Enrich per book: a cached enrichment answers (fresh or stale,
// as CachedEnrichment), and the rest take the console's bounded path, their
// lookups batched by WorkIDs (under lookupSem, inside composeBudget) and each
// work then fetched by id (cached per work, under workSem), never a series
// rail. A book with no identifier, or no match upstream, carries nothing (known).
func (s *Service) Coverages(ctx context.Context, books []BookIDs) []Coverage {
	out := make([]Coverage, len(books))
	var (
		ask []BookIDs
		at  []int
	)
	for i, b := range books {
		if env := s.CachedEnrichment(ctx, b.ASIN, b.ISBN); env != nil {
			out[i] = Coverage{Characters: env.HasCharacters(), Recaps: env.HasRecaps(), Known: true}
			continue
		}
		ask, at = append(ask, b), append(at, i)
	}
	if len(ask) == 0 {
		return out
	}
	var wg sync.WaitGroup
	for j, id := range s.WorkIDs(ctx, ask) {
		i := at[j]
		switch {
		case id.Failed: // unknown: left as the zero Coverage
		case id.ID == "":
			out[i] = Coverage{Known: true}
		default:
			// Each goroutine writes only its own book's entry.
			wg.Go(func() {
				w, err := s.Work(ctx, id.ID)
				switch {
				case errors.Is(err, ErrNotFound):
					out[i] = Coverage{Known: true}
				case err == nil:
					e := &Enrichment{Work: w}
					out[i] = Coverage{Characters: e.HasCharacters(), Recaps: e.HasRecaps(), Known: true}
				}
			})
		}
	}
	wg.Wait()
	return out
}

// HasCharacters and HasRecaps report whether a matched envelope's work carries
// community characters, and recaps (a recap list or the whole-book summary).
func (e *Enrichment) HasCharacters() bool {
	return e != nil && e.Work != nil && len(e.Work.Characters) > 0
}

func (e *Enrichment) HasRecaps() bool {
	return e != nil && e.Work != nil && (len(e.Work.Recaps) > 0 || e.Work.RecapSummary != nil)
}
