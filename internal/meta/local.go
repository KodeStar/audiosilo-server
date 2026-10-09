package meta

import (
	"cmp"
	"context"
	"math"
	"slices"

	"github.com/kodestar/audiosilo-server/pkg/match"
)

// Placing the caller's own books on an envelope's series rails, so the player
// can open "book 3" from the rail instead of searching for it. The rails are
// shared with the cache and every caller; who owns which entry is per caller,
// so it is worked out per request on a copy (PlaceLocal) and never cached.

// MetaLocal is a book the caller can open: the (library, path) handle of every
// content endpoint.
type MetaLocal struct {
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"`
}

// LocalBook is one of the caller's books PlaceLocal may place: its handle, its
// local series and index, its identifiers, and the community work it is when
// known ("" when not; PlaceOwned fills it from the identifiers).
type LocalBook struct {
	MetaLocal
	Series      string
	SeriesIndex float64
	ASIN, ISBN  string
	WorkID      string
}

// SeriesNames is every name rails go by, each rail's and each of its orderings':
// the names the caller's books' series are matched against for PlaceOwned.
func SeriesNames(rails []MetaSeries) []string {
	var names []string
	for _, rail := range rails {
		names = append(names, rail.Name)
		for _, o := range rail.Orderings {
			names = append(names, o.Name)
		}
	}
	return names
}

// PlaceOwned is PlaceLocal over env's rails for the caller's books, each book's
// work id read from the cache by its identifiers (CachedWorkID: memory, else the
// store; never upstream), once per identifiers: a book in several series is a
// candidate in each. requested is the book env is for. env and books are never
// modified.
func (s *Service) PlaceOwned(ctx context.Context, env *Enrichment, requested MetaLocal, books []LocalBook) []MetaSeries {
	cands := slices.Clone(books)
	type idents struct{ asin, isbn string }
	works := map[idents]string{}
	for i := range cands {
		if cands[i].WorkID != "" {
			continue
		}
		k := idents{cands[i].ASIN, cands[i].ISBN}
		id, seen := works[k]
		if !seen {
			id, _ = s.CachedWorkID(ctx, k.asin, k.isbn)
			works[k] = id
		}
		cands[i].WorkID = id
	}
	current := ""
	if env.Work != nil {
		current = env.Work.ID
	}
	return PlaceLocal(env.Series, current, requested, cands)
}

// samePosition reports whether a series index and a rail position name the same
// place (the console's samePosition tolerance, so 2.5 matches "2.5").
func samePosition(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// PlaceLocal returns a copy of rails with Local set on every entry the caller
// owns, in the main view and in each alternate ordering. rails is never
// modified. Each entry gets at most one book:
//  1. the entry whose work is currentWork is requested, the book the envelope
//     is for;
//  2. else the book whose work id (from the cache) is the entry's;
//  3. else, in an alternate ordering, the book its main view placed for the
//     same work, so the two views of one work never disagree;
//  4. else a book of a local series named like this view (match.SeriesKey)
//     whose series index equals the entry's numeric position.
//
// A book whose work id is known is placed by that id only, never by its index:
// a book that is another work (a novella numbered like the next volume) holds
// no slot. requested is placed by rule 1 only. books are in preference order
// (library sort order, then path, as catalog.SeriesBooks returns them); a book
// in requested's library is preferred over that order.
func PlaceLocal(rails []MetaSeries, currentWork string, requested MetaLocal, books []LocalBook) []MetaSeries {
	if len(rails) == 0 {
		return rails
	}
	p := newPlacer(currentWork, requested, books)
	out := make([]MetaSeries, len(rails))
	for i, rail := range rails {
		r := rail
		var mains map[string]MetaLocal
		r.Works, mains = p.view(rail.Works, rail.Name, nil)
		if len(rail.Orderings) > 0 {
			r.Orderings = make([]MetaSeriesOrdering, len(rail.Orderings))
			for j, o := range rail.Orderings {
				o.Works, _ = p.view(o.Works, o.Name, mains)
				r.Orderings[j] = o
			}
		}
		out[i] = r
	}
	return out
}

// placer holds the caller's books indexed for PlaceLocal.
type placer struct {
	current   string
	requested MetaLocal
	// byWork is the preferred book of each known work id.
	byWork map[string]MetaLocal
	// byIndex are the numbered books with no known work id, by series key, in
	// preference order.
	byIndex map[string][]LocalBook
}

func newPlacer(currentWork string, requested MetaLocal, books []LocalBook) *placer {
	ordered := slices.Clone(books)
	slices.SortStableFunc(ordered, func(a, b LocalBook) int {
		return cmp.Compare(rankLibrary(a, requested), rankLibrary(b, requested))
	})
	p := &placer{current: currentWork, requested: requested, byWork: map[string]MetaLocal{}, byIndex: map[string][]LocalBook{}}
	for _, b := range ordered {
		switch {
		case b.MetaLocal == requested:
			// Placed on the current work's entry only.
		case b.WorkID != "":
			if _, ok := p.byWork[b.WorkID]; !ok {
				p.byWork[b.WorkID] = b.MetaLocal
			}
		case b.SeriesIndex > 0:
			if k := match.SeriesKey(b.Series); k != "" {
				p.byIndex[k] = append(p.byIndex[k], b)
			}
		}
	}
	return p
}

// rankLibrary sorts the requested book's library first.
func rankLibrary(b LocalBook, requested MetaLocal) int {
	if b.LibraryID == requested.LibraryID {
		return 0
	}
	return 1
}

// view returns a copy of one view's works (named name) with Local set, and the
// book it placed on each work id. mains is the main view's placements, for an
// alternate ordering (nil for the main view itself).
func (p *placer) view(works []MetaSeriesWork, name string, mains map[string]MetaLocal) ([]MetaSeriesWork, map[string]MetaLocal) {
	if works == nil {
		return nil, nil
	}
	byIndex := p.byIndex[match.SeriesKey(name)]
	out := make([]MetaSeriesWork, len(works))
	placed := map[string]MetaLocal{}
	for i, w := range works {
		out[i] = w
		l, ok := p.place(w, byIndex, mains)
		if !ok {
			continue
		}
		out[i].Local = &l
		if w.ID != "" {
			placed[w.ID] = l
		}
	}
	return out, placed
}

// place is the book for one entry, by PlaceLocal's rules.
func (p *placer) place(w MetaSeriesWork, byIndex []LocalBook, mains map[string]MetaLocal) (MetaLocal, bool) {
	if w.ID != "" {
		if w.ID == p.current {
			return p.requested, true
		}
		if l, ok := p.byWork[w.ID]; ok {
			return l, true
		}
		if l, ok := mains[w.ID]; ok {
			return l, true
		}
	}
	pos, ok := parsePosition(w.Position)
	if !ok {
		return MetaLocal{}, false
	}
	for _, b := range byIndex {
		if samePosition(b.SeriesIndex, pos) {
			return b.MetaLocal, true
		}
	}
	return MetaLocal{}, false
}

// nextEntry is the entry after the current work on rail, read as the series
// reads, as an index into rail.Works: of the entries skip doesn't report (nil
// skips none), the one with the smallest numeric position above the current
// work's (the rail's Position, else the current work's own entry), ties in rail
// order. Unnumbered entries ("1-3", blank) and currentWork's own entries are
// passed over. ok is false when the current position is not a number, so there
// is nothing to count from; next is -1 with ok true when no entry is left after
// the current work.
//
// Pass the rail's MAIN view (MetaSeries.Works): an alternate reading order's
// "next" is a different book, and the main view is the order a listener is in.
func nextEntry(rail MetaSeries, currentWork string, skip func(MetaSeriesWork) bool) (next int, ok bool) {
	at, ok := currentPosition(rail, currentWork)
	if !ok {
		return -1, false
	}
	next = -1
	var best float64
	for i, w := range rail.Works {
		p, numbered := parsePosition(w.Position)
		if !numbered || p <= at || samePosition(p, at) || w.ID == currentWork {
			continue
		}
		if (next < 0 || p < best) && (skip == nil || !skip(w)) {
			next, best = i, p
		}
	}
	return next, true
}

// currentPosition is the current work's numeric position on rail: the rail's
// Position, else its own entry's. ok is false when neither is a number.
func currentPosition(rail MetaSeries, currentWork string) (at float64, ok bool) {
	at, ok = parsePosition(rail.Position)
	for i := 0; !ok && i < len(rail.Works); i++ {
		if rail.Works[i].ID == currentWork {
			at, ok = parsePosition(rail.Works[i].Position)
		}
	}
	return at, ok
}

// railOrder is the order to follow rails in for a book in the local series names
// (its main series first, then its others): the indexes of rails, first a rail
// going by the book's first series (its name or one of its orderings', folded by
// match.SeriesKey), then a rail going by its second, and so on, then every rail
// going by none of them, each group in rails order. So a book that is Discworld
// #8 and City Watch #1 follows Discworld first whatever order the envelope lists
// the two rails in.
func railOrder(rails []MetaSeries, names []string) []int {
	rank := map[string]int{}
	for i, n := range names {
		if k := match.SeriesKey(n); k != "" {
			if _, seen := rank[k]; !seen {
				rank[k] = i
			}
		}
	}
	ranks := make([]int, len(rails))
	order := make([]int, len(rails))
	for i, r := range rails {
		order[i], ranks[i] = i, len(names)
		goesBy := func(n string) {
			if at, ok := rank[match.SeriesKey(n)]; ok && at < ranks[i] {
				ranks[i] = at
			}
		}
		goesBy(r.Name)
		for _, o := range r.Orderings {
			goesBy(o.Name)
		}
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(ranks[a], ranks[b]) })
	return order
}

// NextRail is the entry that decides what follows currentWork for a book in the
// local series names (its main series first, then its others): rail, an index
// into rails, and entry, an index into that rail's MAIN view (Works). On each
// rail in railOrder the deciding entry is the next entry (nextEntry) with every
// entry that steps back passed over (railStepsBack: at or before the current
// work on a rail ranked above), and the first rail that has one decides.
// So after The Silver Chair the publication order passes over The Horse and His
// Boy and The Magician's Nephew (earlier chronologically) and decides with The
// Last Battle. That entry is THE next work: the caller follows it when it is one
// of their books, and otherwise the local steps answer with it alongside - a
// later rail's placed entry never jumps the series the book is read in. Both are
// -1 when no rail decides: every series has ended, no current position is a
// number, or every later entry steps back. Read off the shared rails, since
// placing the caller's books moves no entry (PlaceLocal keeps every rail and
// entry at its index), so a caller learns there is no next entry without looking
// up what they own, and reads the same entry off its placed rails by the indexes.
func NextRail(rails []MetaSeries, currentWork string, names []string) (rail, entry int) {
	order := railOrder(rails, names)
	for k, i := range order {
		at, _ := nextEntry(rails[i], currentWork, func(w MetaSeriesWork) bool {
			return railStepsBack(rails, order[:k], currentWork, w.ID)
		})
		if at >= 0 {
			return i, at
		}
	}
	return -1, -1
}

// railStepsBack reports whether the work nextWork, the next entry on a rail,
// sits at or before currentWork on one of the rails ranked above it (earlier,
// indexes into rails), where both positions are numbers: in a series read in two
// orders (Narnia's chronological and publication rails) following the second
// order's next would loop back through the first's. An entry with no id is
// judged on no other rail.
func railStepsBack(rails []MetaSeries, earlier []int, currentWork, nextWork string) bool {
	if nextWork == "" {
		return false
	}
	for _, j := range earlier {
		at, ok := currentPosition(rails[j], currentWork)
		if !ok {
			continue
		}
		for _, w := range rails[j].Works {
			if p, numbered := parsePosition(w.Position); w.ID == nextWork && numbered && (p < at || samePosition(p, at)) {
				return true
			}
		}
	}
	return false
}
