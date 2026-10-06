package meta

import (
	"context"
	"errors"
	"sync"
)

// Work ids for the admin console's Series cards. A card draws the community rail
// of one book of a local series and places the series' books on it. A book whose
// series_index is missing or numbered differently locally must still land on its
// own entry, so each owned book's asin/isbn is resolved to its work id and
// compared with the rail's work ids. Only the work id is needed (not the work,
// not the series), and each identifier is answered from the first of:
//
//  1. its cached enrichment (the a:/i: key space): a book a player (or the card's
//     own rail) has enriched carries its work id there, and a cached "no match"
//     is a miss here too, so such a book costs nothing;
//  2. its own lookup entry (the "l:" key space), which only WorkIDs writes;
//  3. one upstream lookup (sharedLookup), whose answer fetchLookup records in "l:".
//
// The traffic only flows one way: WorkIDs reads what Enrich cached, but Enrich
// never reads "l:" nor waits on lookupSem, so a console batch can neither delay a
// player's /meta nor stretch the age of its enrichment past its own TTL. Enrich
// never writes "l:" either, so the lookup quota is left to the console.

// BookIDs is one owned book's identifiers, as the catalog holds them.
type BookIDs struct {
	ASIN string
	ISBN string
}

// WorkID is one book's answer from WorkIDs.
type WorkID struct {
	// ID is the community work id; "" when the book has no identifier, has no
	// match upstream, or could not be looked up.
	ID string
	// Failed is true when the lookup failed or ran out of time, so asking again
	// later may resolve it; never for a resolved book or a plain "no match".
	Failed bool
}

// WorkIDs resolves each book's asin (preferred) or isbn to its community work
// id, in input order, each with whether its own lookup failed (see WorkID).
//
// The whole batch runs under composeBudget, so a slow upstream can't eat the
// API's request budget: lookups still queued when it fires are left unresolved
// and failed. Cached answers (a book's enrichment, else its lookup entry; see
// cachedWorkID) are read inline; only the misses start a lookup each (see
// sharedLookup), and a book listed twice (two copies with one ASIN) is looked up
// once.
func (s *Service) WorkIDs(ctx context.Context, books []BookIDs) []WorkID {
	cctx, cancel := context.WithTimeout(ctx, s.composeBudget)
	defer cancel()

	out := make([]WorkID, len(books))
	type miss struct {
		asin, isbn string
		at         []int // the books with this identifier
	}
	misses := map[string]*miss{}
	for i, b := range books {
		asin, isbn := normalizeASIN(b.ASIN), normalizeISBN(b.ISBN)
		key := cacheKey(asin, isbn)
		if key == "" {
			continue
		}
		if m := misses[key]; m != nil {
			m.at = append(m.at, i)
			continue
		}
		if w, hit := s.cachedWorkID(key); hit {
			out[i] = w
			continue
		}
		misses[key] = &miss{asin: asin, isbn: isbn, at: []int{i}}
	}

	var wg sync.WaitGroup
	for _, m := range misses {
		wg.Go(func() {
			// Each miss owns its own indexes: no two goroutines write one entry.
			w := workIDOf(s.sharedLookup(cctx, m.asin, m.isbn))
			for _, i := range m.at {
				out[i] = w
			}
		})
	}
	wg.Wait()
	return out
}

// cachedWorkID answers one identifier's enrichment key (see cacheKey) from the
// cache: its cached enrichment first (a positive one carries the work id, a
// cached "no match" is a miss), else its own "l:" entry. hit is false when
// neither holds an answer. A cached enrichment failure is not an answer: it may
// have come from the work or series fetch rather than the lookup, so the lookup
// entry (or a lookup of its own) decides.
func (s *Service) cachedWorkID(key string) (w WorkID, hit bool) {
	if e, hit, err := cacheGet[Enrichment](s.cache, key); hit {
		switch {
		case err == nil && e.Work != nil && e.Work.ID != "":
			return WorkID{ID: e.Work.ID}, true
		case errors.Is(err, ErrNotFound):
			return WorkID{}, true
		}
	}
	if l, hit, err := cacheGet[upstreamLookup](s.cache, nsLookup.key(key)); hit {
		return workIDOf(l, err), true
	}
	return WorkID{}, false
}

// lookupFlight is one WorkIDs lookup in flight upstream, shared by every caller
// that misses its identifier meanwhile (see sharedLookup).
type lookupFlight struct {
	done chan struct{} // closed once l and err are set
	l    *upstreamLookup
	err  error
	// cancel ends the flight's own context; refs counts the callers still
	// waiting on it. Both are guarded by Service.flightMu.
	cancel context.CancelFunc
	refs   int
}

// sharedLookup answers one identifier (normalized; the asin preferred) from the
// "l:" key space, or else from one upstream lookup bounded by lookupSem and shared
// by every caller missing that identifier at the same time, so two Series
// requests asking about one book cost one GET and one slot.
//
// The flight runs under its own context, detached from the caller that started
// it, so one caller going away (or its budget running out) only drops that
// caller: the others keep waiting, and only once the last one has left is the
// flight cancelled. A flight cut short that way, while still queued or in flight,
// fails from its own context, which fetchLookup never records; a caller that
// leaves early gets its own ctx error and caches nothing either. Only the "l:"
// entry is re-read here: the caller has just read the enrichment (cachedWorkID),
// and a flight that lands meanwhile records into "l:".
func (s *Service) sharedLookup(ctx context.Context, asin, isbn string) (*upstreamLookup, error) {
	key := nsLookup.key(cacheKey(asin, isbn))
	s.flightMu.Lock()
	f := s.flights[key]
	if f == nil {
		// Read under flightMu: a flight records its answer before it leaves the
		// map, so a miss here can't race a flight that just landed.
		if l, hit, err := cacheGet[upstreamLookup](s.cache, key); hit {
			s.flightMu.Unlock()
			return l, err
		}
		fctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		f = &lookupFlight{done: make(chan struct{}), cancel: cancel}
		s.flights[key] = f
		go s.fly(fctx, key, f, asin, isbn)
	}
	f.refs++
	s.flightMu.Unlock()

	select {
	case <-f.done:
		return f.l, f.err
	case <-ctx.Done():
		s.flightMu.Lock()
		if f.refs--; f.refs == 0 {
			f.cancel()
			if s.flights[key] == f {
				delete(s.flights, key)
			}
		}
		s.flightMu.Unlock()
		return nil, ctx.Err()
	}
}

// fly runs one shared lookup: a lookupSem slot, then fetchLookup, then the answer
// to its waiters.
func (s *Service) fly(ctx context.Context, key string, f *lookupFlight, asin, isbn string) {
	defer f.cancel()
	f.l, f.err = func() (*upstreamLookup, error) {
		select {
		case s.lookupSem <- struct{}{}:
		case <-ctx.Done():
			// Never sent: every waiter left while it queued behind other
			// lookups. Nothing is recorded (fetchLookup was never called).
			return nil, ctx.Err()
		}
		// Deferred so a panic can never leak a slot.
		defer func() { <-s.lookupSem }()
		return s.fetchLookup(ctx, asin, isbn)
	}()
	s.flightMu.Lock()
	if s.flights[key] == f {
		delete(s.flights, key)
	}
	s.flightMu.Unlock()
	close(f.done)
}

// fetchLookup is a WorkIDs lookup upstream (see Service.lookup), recorded in the
// "l:" key space with Enrich's TTLs, where cachedWorkID finds it. A failure that
// came from ctx itself (every waiter left, or the batch's budget ran out) is not
// the upstream's fault and so is never recorded. Only fly calls it, holding a
// lookupSem slot; a player's compose calls Service.lookup instead, which records
// nothing.
//
// The returned *upstreamLookup is shared with the cache - treat it as immutable.
func (s *Service) fetchLookup(ctx context.Context, asin, isbn string) (*upstreamLookup, error) {
	key := nsLookup.key(cacheKey(asin, isbn))
	l, err := s.lookup(ctx, asin, isbn)
	switch {
	case errors.Is(err, ErrNotFound):
		s.cache.putMiss(key, notFoundTTL)
	case err != nil:
		if ctx.Err() == nil {
			s.cache.putError(key, err)
		}
	default:
		cachePut(s.cache, key, l, positiveTTL)
	}
	return l, err
}

// workIDOf reads one lookup's outcome as WorkIDs answers it: the work id ("" for
// no match), failed for anything but a match or a clean "no match".
func workIDOf(l *upstreamLookup, err error) WorkID {
	switch {
	case err == nil:
		return WorkID{ID: l.Work.ID}
	case errors.Is(err, ErrNotFound):
		return WorkID{}
	default:
		return WorkID{Failed: true}
	}
}

// CachedWorkID is an identifier's community work id from the in-memory cache
// only (cachedWorkID: its enrichment, else its lookup entry), never the upstream
// or the store, so it is a map read. ok is true when the cache holds an answer:
// the work id, or "" for a cached "no match"; a miss, a cached failure and no
// identifier are ("", false).
func (s *Service) CachedWorkID(asin, isbn string) (id string, ok bool) {
	key := cacheKey(normalizeASIN(asin), normalizeISBN(isbn))
	if key == "" {
		return "", false
	}
	w, hit := s.cachedWorkID(key)
	if !hit || w.Failed {
		return "", false
	}
	return w.ID, true
}
