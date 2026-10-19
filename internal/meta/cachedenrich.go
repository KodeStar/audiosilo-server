package meta

import (
	"context"
	"errors"
	"strings"
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

// Coverage is what a book's community work carries: characters and a recap
// list (what generated content can't fill: fillable, so a summary alone is no
// recaps and Generate is offered for it), a description (its
// community_description or its core description: either is shown before a
// generated one, so a generated one would never be read), and whether that is
// known. Known is false when it couldn't be had right now (a lookup or
// work fetch that failed): unknown, not "none".
type Coverage struct {
	Characters, Recaps, Description, Known bool
}

// coverageOf is a matched envelope's Coverage (known): what fillable leaves
// unfilled, plus a core description, which fillable doesn't count.
func coverageOf(e *Enrichment) Coverage {
	f := e.Fillable()
	core := e != nil && e.Work != nil && strings.TrimSpace(e.Work.Description) != ""
	return Coverage{Characters: !f.Characters, Recaps: !f.Recaps, Description: !f.Description || core, Known: true}
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
			out[i] = coverageOf(env)
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
					out[i] = coverageOf(&Enrichment{Work: w})
				}
			})
		}
	}
	wg.Wait()
	return out
}
