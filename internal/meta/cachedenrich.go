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

// CoverageBook is one book Coverages answers for: its identifiers, its
// community work when the catalog knows it (Book.WorkID, "" when not), and
// whether that is its recorded match's (book_matches: the work a book matched
// in the console with no ASIN or ISBN has, server#375) rather than the
// upcoming job's resolution of those same identifiers.
type CoverageBook struct {
	ASIN, ISBN string
	WorkID     string
	Matched    bool
}

// workOf is the work b is asked about by (/meta's work rule, api.bookEnrichment),
// given its identifiers' cached work (cached, "" for none: no identifier, a
// miss, a cached "no match" or failure): its recorded match's work (Matched),
// else cached, else the work the upcoming job resolved (WorkID), else "". The
// upcoming job's resolution is of those same identifiers and can be a month
// older than their cached answer, so it never wins over that one.
func (b CoverageBook) workOf(cached string) string {
	switch {
	case b.Matched && b.WorkID != "":
		return b.WorkID
	case cached != "":
		return cached
	}
	return b.WorkID
}

// CachedWork is the work b is asked about by, from the cache alone, never the
// upstream (workOf over CachedWorkID): Coverages' order, for a caller that
// needs only the id (the Bridge's book facts). "" when none is known.
func (s *Service) CachedWork(ctx context.Context, b CoverageBook) string {
	cached, _ := s.CachedWorkID(ctx, b.ASIN, b.ISBN)
	return b.workOf(cached)
}

// Coverages answers Coverage for many books at once (the Bridge's estimate: a
// series batch's brief answers, a book's earlier books), each in input order,
// without a full Enrich per book, by /meta's own work rule (api.bookEnrichment),
// so the estimate never prices content /meta would not show (workOf's order):
//  1. a recorded match's work (Matched): the identifiers' cached enrichment
//     when it is of that work, else the work fetched by id; a work upstream
//     no longer has leaves the identifiers' cached enrichment standing, as on
//     /meta;
//  2. else the identifiers' cached enrichment, whatever its work (fresh or
//     stale, as CachedEnrichment): the upcoming job's resolution is of those
//     same identifiers and can be a month older, so it never answers for them
//     (a work since merged away would read as "no content");
//  3. else the work the upcoming job resolved, fetched by id;
//  4. else the console's bounded path, the lookups batched by WorkIDs (under
//     lookupSem, inside composeBudget) and each work then fetched by id.
//
// A work is fetched once (cached per work, under workSem), never a series
// rail. A book with no identifier and no known work, or no match upstream,
// carries nothing (known).
func (s *Service) Coverages(ctx context.Context, books []CoverageBook) []Coverage {
	out := make([]Coverage, len(books))
	var (
		ask []BookIDs
		at  []int
	)
	works := map[string][]int{} // a work to fetch by id -> the books it answers
	// standing is a matched book's identifiers' cached enrichment (of another
	// work), what answers when its match's work is gone upstream.
	standing := make([]*Enrichment, len(books))
	for i, b := range books {
		env := s.CachedEnrichment(ctx, b.ASIN, b.ISBN)
		cached := ""
		if env != nil && env.Work != nil {
			cached = env.Work.ID
		}
		switch id := b.workOf(cached); {
		case id == "":
			ask, at = append(ask, BookIDs{ASIN: b.ASIN, ISBN: b.ISBN}), append(at, i)
		case env.OfWork(id):
			out[i] = coverageOf(env)
		default:
			works[id] = append(works[id], i)
			if b.Matched {
				standing[i] = env
			}
		}
	}
	if len(ask) > 0 {
		for j, id := range s.WorkIDs(ctx, ask) {
			switch {
			case id.Failed: // unknown: left as the zero Coverage
			case id.ID == "":
				out[at[j]] = Coverage{Known: true}
			default:
				works[id.ID] = append(works[id.ID], at[j])
			}
		}
	}
	var wg sync.WaitGroup
	for id, wanted := range works {
		// Each work is fetched once, its goroutine writing only its books'
		// entries.
		wg.Go(func() {
			w, err := s.Work(ctx, id)
			gone := errors.Is(err, ErrNotFound)
			if err != nil && !gone {
				return // unknown: left as the zero Coverage
			}
			for _, i := range wanted {
				switch {
				case !gone:
					out[i] = coverageOf(&Enrichment{Work: w})
				case standing[i] != nil:
					out[i] = coverageOf(standing[i])
				default:
					out[i] = Coverage{Known: true}
				}
			}
		})
	}
	wg.Wait()
	return out
}
