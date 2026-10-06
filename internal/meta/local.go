package meta

import (
	"cmp"
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
// work id read from the in-memory cache by its identifiers (CachedWorkID: never
// upstream, never the store). requested is the book env is for. env and books are
// never modified.
func (s *Service) PlaceOwned(env *Enrichment, requested MetaLocal, books []LocalBook) []MetaSeries {
	cands := slices.Clone(books)
	for i := range cands {
		if cands[i].WorkID == "" {
			cands[i].WorkID, _ = s.CachedWorkID(cands[i].ASIN, cands[i].ISBN)
		}
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

// NextOnRail is the entry after the current work on rail, read as the series
// reads: the entry with the smallest numeric position above the current work's
// (the rail's Position, else the current work's own entry), ties in rail order.
// Unnumbered entries ("1-3", blank) and currentWork's own entries are skipped.
// ok is false when the current position is not a number, so there is nothing to
// count from; next is nil with ok true when the current work is the last. The
// returned entry is a copy.
//
// Pass the rail's MAIN view (MetaSeries.Works): an alternate reading order's
// "next" is a different book, and the main view is the order a listener is in.
func NextOnRail(rail MetaSeries, currentWork string) (next *MetaSeriesWork, ok bool) {
	at, ok := parsePosition(rail.Position)
	for i := 0; !ok && i < len(rail.Works); i++ {
		if rail.Works[i].ID == currentWork {
			at, ok = parsePosition(rail.Works[i].Position)
		}
	}
	if !ok {
		return nil, false
	}
	var best float64
	for _, w := range rail.Works {
		p, numbered := parsePosition(w.Position)
		if !numbered || p <= at || samePosition(p, at) || w.ID == currentWork {
			continue
		}
		if next == nil || p < best {
			c := w
			next, best = &c, p
		}
	}
	return next, true
}
