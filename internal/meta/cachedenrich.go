package meta

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/pkg/match"
)

// cachedAnswer is an identifier's answer from the cache alone, never the
// upstream: memory's enrichment, else memory's lookup (WorkIDs'), else the
// enrichment's persisted row, fresh or stale, as CachedWorkID reads them. env
// is the enrichment when one answered with a match (nil for a lookup's answer
// and a "no match"; shared with the cache: never modify it), id its work ("" for
// a "no match"), at when it was had (answeredAt: at the earliest) and ok
// whether there is an answer at all (a miss, a cached failure and no identifier
// are none). A fresh row warms memory for the rest of its TTL, as Enrich's read
// of it does.
type cachedAnswer struct {
	env *Enrichment
	id  string
	at  time.Time
	ok  bool
}

// cachedAnswerOf is the cache's answer for a book's identifiers (cachedAnswer):
// what both the estimate (Coverages) and the Bridge's book facts (CachedWork)
// read, so they never name different works.
func (s *Service) cachedAnswerOf(ctx context.Context, asin, isbn string) cachedAnswer {
	key := cacheKey(match.NormalizeASIN(asin), match.NormalizeISBN(isbn))
	if key == "" {
		return cachedAnswer{}
	}
	if e, until, hit, err := cacheGetUntil[Enrichment](s.cache, key); hit {
		switch {
		case err == nil && e.Work != nil && e.Work.ID != "":
			return cachedAnswer{env: e, id: e.Work.ID, at: answeredAt(until, true), ok: true}
		case errors.Is(err, ErrNotFound):
			return cachedAnswer{at: answeredAt(until, false), ok: true}
		}
	}
	if l, until, hit, err := cacheGetUntil[upstreamLookup](s.cache, nsLookup.key(key)); hit {
		switch {
		case err == nil && l.Work != nil && l.Work.ID != "":
			return cachedAnswer{id: l.Work.ID, at: answeredAt(until, true), ok: true}
		case errors.Is(err, ErrNotFound):
			return cachedAnswer{at: answeredAt(until, false), ok: true}
		}
	}
	stored, expires, fresh := readStored[Enrichment](ctx, s, key)
	switch {
	case expires.IsZero(): // no row, or none this build reads
		return cachedAnswer{}
	case stored == nil:
		return cachedAnswer{at: answeredAt(expires, false), ok: true}
	case stored.Work == nil || stored.Work.ID == "":
		return cachedAnswer{}
	}
	if fresh {
		cachePutUntil(s.cache, key, stored, expires)
	}
	return cachedAnswer{env: stored, id: stored.Work.ID, at: answeredAt(expires, true), ok: true}
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
// in the console with no ASIN or ISBN has, server#375) or else when the
// upcoming job resolved it from those same identifiers (ResolvedAt,
// upcoming_owned.checked_at; zero when not known).
type CoverageBook struct {
	ASIN, ISBN string
	WorkID     string
	Matched    bool
	ResolvedAt time.Time
}

// workOf is the work b is asked about by (/meta's work rule, api.bookEnrichment),
// given its identifiers' cached answer: its recorded match's work (Matched),
// else the newer of the cached answer and the upcoming job's resolution
// (WorkID), else "". Both answer the same identifiers: the cached one can be a
// day old or, read back from the store after a restart, older than the job's
// monthly recheck, and the job's a month older than a lookup since (a work
// since merged away would read as "no content"). A tie goes to the cache, as
// does a resolution of unknown age. A newer cached "no match" is no work.
func (b CoverageBook) workOf(c cachedAnswer) string {
	switch {
	case b.Matched && b.WorkID != "":
		return b.WorkID
	case b.WorkID != "" && (!c.ok || c.at.Before(b.ResolvedAt)):
		return b.WorkID
	}
	return c.id
}

// CachedWork is the work b is asked about by, from the cache alone, never the
// upstream (workOf over cachedAnswerOf): Coverages' order, for a caller that
// needs only the id (the Bridge's book facts). "" when none is known.
func (s *Service) CachedWork(ctx context.Context, b CoverageBook) string {
	if b.Matched && b.WorkID != "" {
		return b.WorkID
	}
	return b.workOf(s.cachedAnswerOf(ctx, b.ASIN, b.ISBN))
}

// Coverages answers Coverage for many books at once (the Bridge's estimate: a
// series batch's brief answers, a book's earlier books), each in input order,
// without a full Enrich per book, by /meta's own work rule (api.bookEnrichment),
// so the estimate never prices content /meta would not show (workOf's order):
//  1. a recorded match's work (Matched): the identifiers' cached enrichment
//     when it is of that work, else the work fetched by id; a work upstream
//     no longer has leaves the identifiers' cached enrichment standing, as on
//     /meta;
//  2. else the newer of the identifiers' cached answer (cachedAnswerOf: fresh
//     or stale) and the work the upcoming job resolved from them (workOf): the
//     cached enrichment when it is of that work, else the work fetched by id;
//  3. else the console's bounded path, the lookups batched by WorkIDs (under
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
		c := s.cachedAnswerOf(ctx, b.ASIN, b.ISBN)
		switch id := b.workOf(c); {
		case id == "":
			ask, at = append(ask, BookIDs{ASIN: b.ASIN, ISBN: b.ISBN}), append(at, i)
		case c.env.OfWork(id):
			out[i] = coverageOf(c.env)
		default:
			works[id] = append(works[id], i)
			if b.Matched {
				standing[i] = c.env
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
