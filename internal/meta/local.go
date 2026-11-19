package meta

import (
	"cmp"
	"context"
	"math"
	"slices"

	"github.com/kodestar/audiosilo-server/internal/metadata"
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
// local series and index (SeriesIndexEnd: a range's end, an omnibus of books 1-3;
// else 0), its identifiers, and the community work it is when known ("" when
// not; WithWorkIDs fills it from the identifiers). Narrator, Started and
// Abridged decide which of a work's recordings is the pick (pickRank).
type LocalBook struct {
	MetaLocal
	Series         string
	SeriesIndex    float64
	SeriesIndexEnd float64
	ASIN, ISBN     string
	WorkID         string
	// Matched: WorkID is the book's recorded console match's, so on a view
	// that doesn't list that work the book holds its own position (an edition
	// the community files apart), as upcoming's slot holding does.
	Matched bool
	// Narrator is the book's narrator credit.
	Narrator string
	// Started: the caller has started the book and not finished it.
	Started bool
	// Abridged: the book is known to be abridged (unknown counts as not).
	Abridged bool
}

// last is the last position b holds in its series: a range's end, else its index.
func (b LocalBook) last() float64 { return max(b.SeriesIndex, b.SeriesIndexEnd) }

// SeriesNames is every name rails go by, each rail's and each of its orderings':
// the names the caller's books' series are matched against (api.localRails).
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

// WithWorkIDs returns a copy of books, the caller's books for PlaceLocal, with
// each work id the catalog doesn't know (WorkID "") read from the cache by the
// book's identifiers (CachedWorkID: memory, else the store; never upstream),
// once per identifiers: a book in several series is a candidate in each.
// requested, the book the envelope is for, is placed on its work's entry
// whatever its own work id, so it is never looked up. books is never modified.
func (s *Service) WithWorkIDs(ctx context.Context, requested MetaLocal, books []LocalBook) []LocalBook {
	cands := slices.Clone(books)
	type idents struct{ asin, isbn string }
	works := map[idents]string{}
	for i := range cands {
		if cands[i].WorkID != "" || cands[i].MetaLocal == requested {
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
	return cands
}

// Contested is the books (with their work ids, WithWorkIDs) the pick has to
// choose between, each once, in books' order: two or more books of one of
// PlaceLocal's groups (newPlacer): of a work id, or with no work id the books
// of a series (match.SeriesKey) starting at one position, the only ones
// place chooses between. requested and its copies are in none (requested is
// placed by its work's entry alone). Only these need Started (pickRank's
// second test), so the caller's progress is read for them alone, and not at
// all for the usual envelope, one book a work or a position (server#413).
func Contested(currentWork string, requested MetaLocal, books []LocalBook) []MetaLocal {
	p := newPlacer(currentWork, requested, books)
	contested := map[MetaLocal]bool{}
	for _, g := range p.byWork {
		if len(g) > 1 { // each book once: one alone leaves no choice
			for _, b := range g {
				contested[b.MetaLocal] = true
			}
		}
	}
	for _, g := range p.byIndex {
		// A book is listed once per position it holds in the series.
		for i, a := range g {
			for _, b := range g[i+1:] {
				if SamePosition(a.SeriesIndex, b.SeriesIndex) && a.MetaLocal != b.MetaLocal {
					contested[a.MetaLocal], contested[b.MetaLocal] = true, true
				}
			}
		}
	}
	var out []MetaLocal
	for _, b := range books {
		if contested[b.MetaLocal] {
			out = append(out, b.MetaLocal)
			delete(contested, b.MetaLocal)
		}
	}
	return out
}

// SamePosition reports whether a series index and a rail position name the same
// place (the console's samePosition tolerance, so 2.5 matches "2.5").
func SamePosition(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// ParseSpan reads a rail position as the positions it holds: a number ("3", 3 to
// 3) or a range ("1-3", an omnibus of books 1 to 3: metadata.ParseSeriesRange,
// the rule the series_index field stores one by). ok is false for anything else.
func ParseSpan(s string) (start, end float64, ok bool) {
	if p, ok := parsePosition(s); ok {
		return p, p, true
	}
	return metadata.ParseSeriesRange(s)
}

// RailWorks is the work id of every entry on rails, in each rail's main view and
// its orderings, each once: the works the caller's recordings are found by
// (catalog.WorkBooks).
func RailWorks(rails []MetaSeries) []string {
	var out []string
	add := func(works []MetaSeriesWork) {
		for _, w := range works {
			if w.ID != "" && !slices.Contains(out, w.ID) {
				out = append(out, w.ID)
			}
		}
	}
	for _, rail := range rails {
		add(rail.Works)
		for _, o := range rail.Orderings {
			add(o.Works)
		}
	}
	return out
}

// PlaceLocal returns a copy of rails with Local set on every entry the caller
// owns, in the main view and in each alternate ordering, and LocalOthers on an
// entry the caller holds more than one recording of. rails is never modified.
// Each entry gets at most one book as Local:
//  1. the entry whose work is currentWork is requested, the book the envelope
//     is for;
//  2. else the book whose work id (Book.work_id, else the cache's) is the entry's;
//  3. else, in an alternate ordering, the book its main view placed for the
//     same work, so the two views of one work never disagree;
//  4. else a book of a local series named like this view (match.SeriesKey)
//     whose series index equals the entry's numeric position: a range entry
//     ("1-3") holds a book at that same range, and an entry at one number holds
//     a book at it, else a book whose range starts there (an omnibus of 1-3
//     owned, entry 1 not).
//
// A book whose work id is known is placed by that id only, never by its index:
// a book that is another work (a novella numbered like the next volume) holds
// no slot. One matched in the console (Matched) to a work a view doesn't list
// is placed by its index there too, by rule 4, as upcoming holds its slot.
// requested is placed by rule 1 only; when it is among books, its narrator is
// the one the pick prefers. Of several books of one work id (other
// recordings: another narrator, an abridgement, a copy in another library) the
// pick is Local by pickRank and the rest, in that order, LocalOthers, one per
// recording: never a copy of the pick or of a book before it
// (otherRecordings). An entry only ever lists recordings of its own work: rule
// 1's entry lists currentWork's books but requested and its copies, whatever
// requested's own WorkID, so no book is on two entries; and a copy of requested
// is placed on no entry at all (its own recorded match may name another work),
// so one recording never shows under two handles (server#413). Rule 1's entry
// tells the copies apart in /item's other_recordings' order, not the pick's
// (currentOthers), so both list the same recordings, and lists them in the
// pick's.
// books are in preference order (library sort order, then path, as
// catalog.SeriesBooks returns them), the pick's last tiebreak.
func PlaceLocal(rails []MetaSeries, currentWork string, requested MetaLocal, books []LocalBook) []MetaSeries {
	if len(rails) == 0 {
		return rails
	}
	p := newPlacer(currentWork, requested, books)
	out := make([]MetaSeries, len(rails))
	for i, rail := range rails {
		r := rail
		var mains map[string]placement
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
	current string
	// self is requested among the books (its narrator and identifiers), only
	// its handle when it isn't one: the current work's entry holds it as Local,
	// the current work's other books beside it (its copies left out).
	self LocalBook
	// byWork is every book of each known work id (each once), the pick first
	// (pickRank); of the current work's, only those its entry lists
	// (currentOthers).
	byWork map[string][]LocalBook
	// byIndex are the numbered books with no known work id, and the matched
	// ones (LocalBook.Matched, for a view that doesn't list their work), by
	// series key, in preference order.
	byIndex map[string][]LocalBook
}

// placement is the books an entry is placed on: Local and LocalOthers.
type placement struct {
	local  MetaLocal
	others []MetaLocal
}

func newPlacer(currentWork string, requested MetaLocal, books []LocalBook) *placer {
	self := LocalBook{MetaLocal: requested}
	if i := slices.IndexFunc(books, func(b LocalBook) bool { return b.MetaLocal == requested }); i >= 0 {
		self = books[i]
	}
	narrator := match.Fold(self.Narrator)
	type ranked struct {
		book LocalBook
		rank int
	}
	ordered := make([]ranked, len(books))
	for i, b := range books {
		ordered[i] = ranked{b, pickRank(b, requested, narrator)}
	}
	slices.SortStableFunc(ordered, func(a, b ranked) int { return cmp.Compare(a.rank, b.rank) })
	p := &placer{current: currentWork, self: self, byWork: map[string][]LocalBook{}, byIndex: map[string][]LocalBook{}}
	listed := currentOthers(currentWork, self, books)
	for _, o := range ordered {
		b := o.book
		switch {
		case b.MetaLocal == requested:
			// Placed on the current work's entry only.
		case b.WorkID != currentWork && copyOf(self, b):
			// requested's recording again: requested is its one handle, so it
			// is placed on no other entry, whatever its own work id. (On the
			// current work's entry currentOthers drops it, learning its ISBN.)
		case b.WorkID != "" && b.WorkID == currentWork && !listed[b.MetaLocal]:
			// A copy of requested or of a recording the entry lists already.
		case b.WorkID != "":
			// A book in several series is a candidate in each: listed once.
			if !slices.ContainsFunc(p.byWork[b.WorkID], func(o LocalBook) bool { return o.MetaLocal == b.MetaLocal }) {
				p.byWork[b.WorkID] = append(p.byWork[b.WorkID], b)
			}
			if b.Matched {
				p.index(b)
			}
		default:
			p.index(b)
		}
	}
	return p
}

// index adds b to byIndex under its series' key, when it is numbered there.
func (p *placer) index(b LocalBook) {
	if b.SeriesIndex <= 0 {
		return
	}
	if k := match.SeriesKey(b.Series); k != "" {
		p.byIndex[k] = append(p.byIndex[k], b)
	}
}

// pickRank is which of a work's recordings the caller is given first, lower
// first (server#241): one read by the narrator of the book just finished
// (requested's, narrator, folded by match.Fold so case, spacing and
// punctuation aside; none when it has none), then one started and not
// finished, then one in requested's library, then one not known to be
// abridged; the books' own order (library sort order, then path) breaks a tie.
// Each test is one bit, the first the highest.
func pickRank(b LocalBook, requested MetaLocal, narrator string) int {
	rank := 0
	for _, pass := range []bool{
		narrator != "" && match.Fold(b.Narrator) == narrator,
		b.Started,
		b.LibraryID == requested.LibraryID,
		!b.Abridged,
	} {
		rank <<= 1
		if !pass {
			rank |= 1
		}
	}
	return rank
}

// view returns a copy of one view's works (named name) with Local and
// LocalOthers set, and what it placed on each work id. mains is the main view's
// placements, for an alternate ordering (nil for the main view itself).
func (p *placer) view(works []MetaSeriesWork, name string, mains map[string]placement) ([]MetaSeriesWork, map[string]placement) {
	if works == nil {
		return nil, nil
	}
	byIndex := p.byIndex[match.SeriesKey(name)]
	if slices.ContainsFunc(byIndex, func(b LocalBook) bool { return b.WorkID != "" }) {
		// A matched book whose work this view lists is placed by that work alone.
		byIndex = slices.DeleteFunc(slices.Clone(byIndex), func(b LocalBook) bool {
			return b.WorkID != "" && slices.ContainsFunc(works, func(w MetaSeriesWork) bool { return w.ID == b.WorkID })
		})
	}
	out := make([]MetaSeriesWork, len(works))
	placed := map[string]placement{}
	for i, w := range works {
		out[i] = w
		pl, ok := p.place(w, byIndex, mains)
		if !ok {
			continue
		}
		out[i].Local = &pl.local
		out[i].LocalOthers = pl.others
		if w.ID != "" {
			placed[w.ID] = pl
		}
	}
	return out, placed
}

// place is the books for one entry, by PlaceLocal's rules.
func (p *placer) place(w MetaSeriesWork, byIndex []LocalBook, mains map[string]placement) (placement, bool) {
	if w.ID != "" {
		if w.ID == p.current {
			var others []MetaLocal
			for _, b := range p.byWork[p.current] {
				others = append(others, b.MetaLocal)
			}
			return placement{local: p.self.MetaLocal, others: others}, true
		}
		if books := p.byWork[w.ID]; len(books) > 0 {
			return placement{local: books[0].MetaLocal, others: otherRecordings(books[1:], books[0])}, true
		}
		if pl, ok := mains[w.ID]; ok {
			return pl, true
		}
	}
	start, end, ok := ParseSpan(w.Position)
	if !ok {
		return placement{}, false
	}
	// A book at exactly this place, else (a single entry) the first omnibus
	// starting there.
	var omnibus MetaLocal
	found := false
	for _, b := range byIndex {
		if !SamePosition(b.SeriesIndex, start) {
			continue
		}
		if SamePosition(b.last(), end) {
			return placement{local: b.MetaLocal}, true
		}
		if start == end && !found && b.SeriesIndexEnd > b.SeriesIndex {
			omnibus, found = b.MetaLocal, true
		}
	}
	return placement{local: omnibus}, found
}

// copyOf reports whether b is a copy of the recording self is (match.Recordings:
// self's ASIN, or with no ASIN of its own self's ISBN, normalized).
func copyOf(self, b LocalBook) bool {
	var seen match.Recordings
	seen.Seen(self.ASIN, self.ISBN)
	return seen.Seen(b.ASIN, b.ISBN)
}

// currentOthers is the books the current work's entry lists beside self (the
// requested book): currentWork's books, one per recording (match.Recordings),
// never self or a copy of it. The rule depends on the order (a book with an
// ISBN alone is a copy of a recording seen before it that carries the ISBN,
// never of one seen after), so it is run in catalog.OtherRecordings' order,
// not the pick's: self's library first, then books' own order, which for a
// book whose own work is currentWork is that list's (api.localRails). Every
// recording /item lists is listed here, whichever the pick's order puts first.
func currentOthers(currentWork string, self LocalBook, books []LocalBook) map[MetaLocal]bool {
	listed := map[MetaLocal]bool{}
	if currentWork == "" {
		return listed
	}
	var seen match.Recordings
	seen.Seen(self.ASIN, self.ISBN)
	judged := map[MetaLocal]bool{self.MetaLocal: true}
	for _, own := range []bool{true, false} {
		for _, b := range books {
			// A book in several series is a candidate in each: judged once.
			if b.WorkID != currentWork || (b.LibraryID == self.LibraryID) != own || judged[b.MetaLocal] {
				continue
			}
			judged[b.MetaLocal] = true
			if !seen.Seen(b.ASIN, b.ISBN) {
				listed[b.MetaLocal] = true
			}
		}
	}
	return listed
}

// otherRecordings is books as LocalOthers, one per recording: a book sharing
// an ASIN with pick, or with a book before it, or, with no ASIN of its own, an
// ISBN (match.Recordings, compared normalized, a tag's case, spaces and hyphens
// aside; books with different ASINs never are), is the same recording again
// and left out, as catalog.OtherRecordings leaves it out, so of
// a recording's copies the first in books' order is the one listed; nil when
// none is left.
func otherRecordings(books []LocalBook, pick LocalBook) []MetaLocal {
	var seen match.Recordings
	seen.Seen(pick.ASIN, pick.ISBN)
	var out []MetaLocal
	for _, b := range books {
		if !seen.Seen(b.ASIN, b.ISBN) {
			out = append(out, b.MetaLocal)
		}
	}
	return out
}

// nextEntry is the entry after the current work on rail, read as the series
// reads, as an index into rail.Works: of the entries skip doesn't report (nil
// skips none), the one with the smallest numeric position (a range's start)
// above the current work's last (currentPosition: a range's end, so an omnibus
// of 1-3 is followed by 4), ties in rail order, but a single position before a
// range starting there (an omnibus never takes the place of the book it starts
// with). Unnumbered entries (blank, not a number) and currentWork's own entries
// are passed over. ok is false when the current position is not a number, so
// there is nothing to count from; next is -1 with ok true when no entry is left
// after the current work.
//
// Pass the rail's MAIN view (MetaSeries.Works): an alternate reading order's
// "next" is a different book, and the main view is the order a listener is in.
func nextEntry(rail MetaSeries, currentWork string, skip func(MetaSeriesWork) bool) (next int, ok bool) {
	at, ok := currentPosition(rail, currentWork)
	if !ok {
		return -1, false
	}
	next = -1
	var best, bestEnd float64
	for i, w := range rail.Works {
		p, end, numbered := ParseSpan(w.Position)
		if !numbered || p <= at || SamePosition(p, at) || w.ID == currentWork {
			continue
		}
		better := next < 0 || p < best || (p == best && end == p && bestEnd > best)
		if better && (skip == nil || !skip(w)) {
			next, best, bestEnd = i, p, end
		}
	}
	return next, true
}

// currentPosition is the last numeric position the current work holds on rail
// (a range's end: an omnibus of 1-3 is at 3): the rail's Position, else its own
// entry's. ok is false when neither is a number or a range.
func currentPosition(rail MetaSeries, currentWork string) (at float64, ok bool) {
	_, at, ok = ParseSpan(rail.Position)
	for i := 0; !ok && i < len(rail.Works); i++ {
		if rail.Works[i].ID == currentWork {
			_, at, ok = ParseSpan(rail.Works[i].Position)
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
// indexes into rails), where both positions are numbers (a range: nextWork by
// its start, currentWork by its end, so a work inside an omnibus the listener
// is in steps back): in a series read in two
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
			if p, _, numbered := ParseSpan(w.Position); w.ID == nextWork && numbered && (p < at || SamePosition(p, at)) {
				return true
			}
		}
	}
	return false
}
