package upcoming

import (
	"cmp"
	"math"
	"slices"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

// What a local series lacks, worked out from the community series it is (pure:
// no I/O). The rules:
//
//   - preorder: a member whose first release is still ahead of today (at the
//     precision the date states), and the series doesn't hold;
//   - released: a member released within the window, not held;
//   - missing (owner decision 2): a member the series doesn't hold below the
//     highest volume it does (an interior gap), plus the single next volume
//     after that highest one that is out (not a preorder), when it is no
//     preorder or recent release already.
//
// Holding follows meta.PlaceLocal: a book whose community work is known holds
// that work's entry (a range: every entry inside it) and nothing else, whatever
// its own number says; a book whose work isn't known holds the entry at its own
// position (a range at that range, a single entry the range starts on). Entries
// are the console's railEntries: one per position, the first member there, and
// a range entry ("1-3", an omnibus) only on a rail that numbers none of the
// books inside it. Only whole-number positions (and such ranges) can be missing,
// as the player's localGaps counts them: a novella at 2.5 is never a gap,
// though it is a preorder or a release like any member.

// OwnedBook is one book of a local series: its community work ("" when not
// known) and its position in the series (a range's end, 0 for none).
type OwnedBook struct {
	WorkID      string
	Position    float64
	PositionEnd float64
}

// Series is one community series with its members, in series order.
type Series struct {
	ID      string
	Members []catalog.MetaSeriesMember
}

// span is a place in a series: a position and, for a range, its end (== start
// for a single position).
type span struct{ start, end float64 }

// samePos is the rails' position tolerance (meta.SamePosition).
var samePos = meta.SamePosition

func atOrBefore(a, b float64) bool { return a < b || samePos(a, b) }

// holds reports whether a book placed at p holds entry e.
func (p span) holds(e span) bool { return atOrBefore(p.start, e.start) && atOrBefore(e.end, p.end) }

// whole reports whether s may be a gap: a whole-number position, or a range
// starting on one.
func (s span) whole() bool { return samePos(s.start, math.Round(s.start)) && s.start > 0 }

// parseSpan reads a member's position as the rails do (meta.ParseSpan): a
// number, or a range ("1-3").
func parseSpan(pos string) (span, bool) {
	a, b, ok := meta.ParseSpan(pos)
	return span{a, b}, ok
}

// entry is one numbered slot of a series.
type entry struct {
	span
	member int // index into the series' members
}

// entries is a series' numbered slots, ascending (railEntries).
func entries(members []catalog.MetaSeriesMember) []entry {
	type parsed struct {
		s  span
		ok bool
	}
	ps := make([]parsed, len(members))
	var singles []float64
	for i, m := range members {
		s, ok := parseSpan(m.Position)
		ps[i] = parsed{s, ok}
		if ok && samePos(s.start, s.end) {
			singles = append(singles, s.start)
		}
	}
	var out []entry
	for i, p := range ps {
		if !p.ok || slices.ContainsFunc(out, func(e entry) bool { return samePos(e.start, p.s.start) }) {
			continue
		}
		if !samePos(p.s.start, p.s.end) && slices.ContainsFunc(singles, func(n float64) bool { return p.s.holds(span{n, n}) }) {
			continue
		}
		out = append(out, entry{span: p.s, member: i})
	}
	slices.SortFunc(out, func(a, b entry) int { return cmp.Compare(a.start, b.start) })
	return out
}

// Classify works out a local series' items from its community series (in the
// order they are tried: a work two of them list is the first's) and the books
// it holds, as of now with a recent-release window of window days. Each work is
// one item at most; the items come in series order.
func Classify(series []Series, owned []OwnedBook, now time.Time, window int) []catalog.UpcomingItem {
	ownedWorks := map[string]bool{}
	for _, b := range owned {
		if b.WorkID != "" {
			ownedWorks[b.WorkID] = true
		}
	}
	seen := map[string]bool{}
	var out []catalog.UpcomingItem
	for _, s := range series {
		for _, it := range classifyOne(s, owned, ownedWorks, now, window) {
			if !seen[it.WorkID] {
				seen[it.WorkID] = true
				out = append(out, it)
			}
		}
	}
	return out
}

func classifyOne(s Series, owned []OwnedBook, ownedWorks map[string]bool, now time.Time, window int) []catalog.UpcomingItem {
	members := s.Members
	slots := entries(members)
	// Each work's span on this series (its first numbered one).
	workSpan := map[string]span{}
	for _, m := range members {
		if _, ok := workSpan[m.WorkID]; ok {
			continue
		}
		if sp, ok := parseSpan(m.Position); ok {
			workSpan[m.WorkID] = sp
		}
	}
	// Where the owned books sit.
	var held []span
	for _, b := range owned {
		switch {
		case b.WorkID != "":
			if sp, ok := workSpan[b.WorkID]; ok {
				held = append(held, sp)
			}
		case b.Position > 0:
			held = append(held, span{b.Position, max(b.Position, b.PositionEnd)})
		}
	}
	top := 0.0
	for _, h := range held {
		top = max(top, h.end)
	}
	isHeld := func(sp span) bool {
		return slices.ContainsFunc(held, func(h span) bool { return h.holds(sp) })
	}
	isSlot := map[int]span{}
	for _, e := range slots {
		isSlot[e.member] = e.span
	}

	kinds := map[int]string{}
	done := map[string]bool{}
	for i, m := range members {
		if m.WorkID == "" || done[m.WorkID] {
			continue
		}
		done[m.WorkID] = true
		if ownedWorks[m.WorkID] {
			continue
		}
		if sp, ok := workSpan[m.WorkID]; ok && isHeld(sp) {
			continue
		}
		switch {
		case releaseIsFuture(m.ReleaseDate, now):
			kinds[i] = catalog.UpcomingPreorder
		case releasedWithin(m.ReleaseDate, now, window):
			kinds[i] = catalog.UpcomingReleased
		default:
			if sp, ok := isSlot[i]; ok && sp.whole() && sp.start < top && !samePos(sp.start, top) {
				kinds[i] = catalog.UpcomingMissing
			}
		}
	}
	// The single next volume after the highest one held that is out.
	if top > 0 {
		for _, e := range slots {
			if !e.whole() || atOrBefore(e.start, top) {
				continue
			}
			m := members[e.member]
			if ownedWorks[m.WorkID] || isHeld(e.span) || releaseIsFuture(m.ReleaseDate, now) {
				continue
			}
			if _, set := kinds[e.member]; !set {
				kinds[e.member] = catalog.UpcomingMissing
			}
			break
		}
	}

	var out []catalog.UpcomingItem
	for i, m := range members {
		kind, ok := kinds[i]
		if !ok {
			continue
		}
		out = append(out, catalog.UpcomingItem{
			WorkID:      m.WorkID,
			SeriesID:    s.ID,
			Kind:        kind,
			ReleaseDate: m.ReleaseDate,
			Position:    m.Position,
		})
	}
	return out
}

// The release-date rules are metaserve's watch feed's (audiosilo-meta
// pkg/query/watchfeed.go: releaseIsFuture, parseReleaseDate and its window),
// so the server and the feed agree about which works are news.

// parseReleaseDate reads YYYY, YYYY-MM or YYYY-MM-DD as the first instant it
// names.
func parseReleaseDate(v string) (time.Time, bool) {
	var layout string
	switch len(v) {
	case 4:
		layout = "2006"
	case 7:
		layout = "2006-01"
	case 10:
		layout = time.DateOnly
	default:
		return time.Time{}, false
	}
	t, err := time.Parse(layout, v)
	return t.UTC(), err == nil
}

func dayStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// releaseIsFuture reports whether a stated release date is still ahead of now,
// compared at its own precision ("2026" against this year), strictly: a book
// out today is out. An unreadable date is never in the future.
func releaseIsFuture(v string, now time.Time) bool {
	if _, ok := parseReleaseDate(v); !ok {
		return false
	}
	today := dayStart(now).Format(time.DateOnly)
	return v > today[:len(v)]
}

// releasedWithin reports whether a stated release date is out and no older
// than window days (from the start of the day window days ago).
func releasedWithin(v string, now time.Time, window int) bool {
	t, ok := parseReleaseDate(v)
	if !ok || releaseIsFuture(v, now) {
		return false
	}
	return !t.Before(dayStart(now).AddDate(0, 0, -window))
}
