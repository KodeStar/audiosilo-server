package upcoming

import (
	"slices"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// now is the classification clock: 30 October 2026.
var now = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// member builds a series member: work id "w<pos>" unless id is given.
func member(pos, date string) catalog.MetaSeriesMember {
	return catalog.MetaSeriesMember{WorkID: "w" + pos, Position: pos, Title: "Book " + pos, ReleaseDate: date}
}

// kinds is items as "work=kind", in order.
func kinds(items []catalog.UpcomingItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.WorkID + "=" + it.Kind
	}
	return out
}

func check(t *testing.T, got []catalog.UpcomingItem, want ...string) {
	t.Helper()
	if g := kinds(got); !slices.Equal(g, want) {
		t.Fatalf("items = %v, want %v", g, want)
	}
}

func TestClassifyInteriorAndNext(t *testing.T) {
	t.Parallel()
	s := Series{ID: "s", Members: []catalog.MetaSeriesMember{
		member("1", "2010"), member("2", "2011"), member("3", "2012"),
		member("4", "2013"), member("5", "2014"), member("6", "2015"),
	}}
	owned := []OwnedBook{{WorkID: "w1"}, {WorkID: "w2"}, {WorkID: "w4"}}
	// 3 is an interior gap; 5 the next volume after the highest held (4); 6 is not.
	check(t, Classify([]Series{s}, owned, now, 90), "w3=missing", "w5=missing")
}

func TestClassifyNextSkipsPreorders(t *testing.T) {
	t.Parallel()
	s := Series{ID: "s", Members: []catalog.MetaSeriesMember{
		member("1", "2010"), member("2", "2011"),
		member("3", "2027-02-01"), // a preorder: not the next volume out
		member("4", ""),           // undated: out, as far as anyone knows
		member("5", "2016"),
	}}
	check(t, Classify([]Series{s}, []OwnedBook{{WorkID: "w1"}, {WorkID: "w2"}}, now, 90),
		"w3=preorder", "w4=missing")
}

func TestClassifyRecentReleaseIsTheNext(t *testing.T) {
	t.Parallel()
	s := Series{ID: "s", Members: []catalog.MetaSeriesMember{
		member("1", "2010"), member("2", "2011"),
		member("3", "2026-10-01"), // within 90 days: released, and it is the next volume
		member("4", "2026-12"),    // a month still ahead: a preorder
		member("5", "2028"),
	}}
	check(t, Classify([]Series{s}, []OwnedBook{{WorkID: "w1"}, {WorkID: "w2"}}, now, 90),
		"w3=released", "w4=preorder", "w5=preorder")
	// With a 10-day window the release is old news: then it is the missing next.
	check(t, Classify([]Series{s}, []OwnedBook{{WorkID: "w1"}, {WorkID: "w2"}}, now, 10),
		"w3=missing", "w4=preorder", "w5=preorder")
}

func TestClassifyReleaseDatePrecision(t *testing.T) {
	t.Parallel()
	s := Series{ID: "s", Members: []catalog.MetaSeriesMember{
		member("1", "2010"),
		member("2", "2026"),       // this year: out, not a preorder
		member("3", "2026-10-30"), // today: out
		member("4", "2026-10-31"), // tomorrow: a preorder
		member("5", "2026-13"),    // no such month: unreadable, never a preorder
	}}
	got := Classify([]Series{s}, []OwnedBook{{WorkID: "w1"}}, now, 90)
	check(t, got, "w2=missing", "w3=released", "w4=preorder")
}

func TestClassifyRanges(t *testing.T) {
	t.Parallel()
	s := Series{ID: "s", Members: []catalog.MetaSeriesMember{
		{WorkID: "omnibus", Position: "1-3", Title: "Books 1-3", ReleaseDate: "2015"},
		member("1", "2010"), member("2", "2011"), member("3", "2012"), member("4", "2013"),
		member("5", "2014"),
	}}
	// The omnibus (by its work) holds 1, 2 and 3; the next volume is 4.
	check(t, Classify([]Series{s}, []OwnedBook{{WorkID: "omnibus"}}, now, 90), "w4=missing")
	// A book with no known work at the range 1-3 holds them too.
	check(t, Classify([]Series{s}, []OwnedBook{{Position: 1, PositionEnd: 3}}, now, 90), "w4=missing")
	// Owning 4 alone: 1, 2, 3 are gaps (the range entry is no slot of its own,
	// as single books lie inside it), and the omnibus is not one.
	check(t, Classify([]Series{s}, []OwnedBook{{WorkID: "w4"}}, now, 90),
		"w1=missing", "w2=missing", "w3=missing", "w5=missing")
	// On a series numbering only the omnibus, the range is a slot: a gap below 4.
	only := Series{ID: "o", Members: []catalog.MetaSeriesMember{
		{WorkID: "omnibus", Position: "1-3", ReleaseDate: "2015"}, member("4", "2013"),
	}}
	check(t, Classify([]Series{only}, []OwnedBook{{WorkID: "w4"}}, now, 90), "omnibus=missing")
}

// A box set of volumes the series holds one by one is held: neither a release
// nor a preorder to tell anyone about, however new.
func TestClassifyBoxSetOfHeldVolumes(t *testing.T) {
	t.Parallel()
	s := Series{ID: "s", Members: []catalog.MetaSeriesMember{
		member("1", "2010"), member("2", "2011"), member("3", "2012"), member("4", "2013"),
		{WorkID: "box", Position: "1-3", Title: "Books 1-3", ReleaseDate: "2026-10-15"},
		{WorkID: "box2", Position: "2-4", Title: "Books 2-4", ReleaseDate: "2027-01"},
	}}
	// Held by work, and by position (books whose work isn't known).
	check(t, Classify([]Series{s}, []OwnedBook{{WorkID: "w1"}, {WorkID: "w2"}, {WorkID: "w3"}}, now, 90),
		"w4=missing", "box2=preorder")
	check(t, Classify([]Series{s}, []OwnedBook{{Position: 1}, {Position: 2}, {Position: 3}, {Position: 4}}, now, 90))
	// One volume inside it not held: the box set is still news.
	check(t, Classify([]Series{s}, []OwnedBook{{WorkID: "w1"}, {WorkID: "w3"}}, now, 90),
		"w2=missing", "w4=missing", "box=released", "box2=preorder")
}

func TestClassifyFractionalPositions(t *testing.T) {
	t.Parallel()
	s := Series{ID: "s", Members: []catalog.MetaSeriesMember{
		member("1", "2010"), member("2", "2011"), member("2.5", "2012"), member("3", "2013"),
		member("3.5", "2026-09-15"), member("4", "2014"),
	}}
	// 2 is a gap, the novella 2.5 never is; 3.5, out last month, is a release;
	// the next volume is 4, not the novella.
	check(t, Classify([]Series{s}, []OwnedBook{{WorkID: "w1"}, {WorkID: "w3"}}, now, 90),
		"w2=missing", "w3.5=released", "w4=missing")
	// Owning only the novella 2.5 puts the top at 2.5: 1 and 2 are gaps, 3 the next.
	check(t, Classify([]Series{s}, []OwnedBook{{WorkID: "w2.5"}}, now, 90),
		"w1=missing", "w2=missing", "w3=missing", "w3.5=released")
}

func TestClassifyOwnedByWorkOrPosition(t *testing.T) {
	t.Parallel()
	s := Series{ID: "s", Members: []catalog.MetaSeriesMember{
		member("1", "2010"), member("2", "2011"), member("3", "2012"), member("4", "2013"),
		member("5", "2014"), member("6", "2015"),
	}}
	// A book whose work is known holds that work's entry (5), not its own
	// number (2); a book with no known work holds its own number (1).
	owned := []OwnedBook{{WorkID: "w5", Position: 2}, {Position: 1}}
	check(t, Classify([]Series{s}, owned, now, 90), "w2=missing", "w3=missing", "w4=missing", "w6=missing")
	// A book known to be another work (not on this series) holds nothing.
	check(t, Classify([]Series{s}, []OwnedBook{{WorkID: "elsewhere", Position: 3}}, now, 90))
}

func TestClassifySeveralSeries(t *testing.T) {
	t.Parallel()
	a := Series{ID: "a", Members: []catalog.MetaSeriesMember{member("1", "2010"), member("2", "2011"), member("3", "2026-12-01")}}
	b := Series{ID: "b", Members: []catalog.MetaSeriesMember{
		{WorkID: "w3", Position: "1", ReleaseDate: "2026-12-01"}, {WorkID: "x2", Position: "2", ReleaseDate: "2026-11-02"},
	}}
	got := Classify([]Series{a, b}, []OwnedBook{{WorkID: "w1"}}, now, 90)
	check(t, got, "w2=missing", "w3=preorder", "x2=preorder")
	if got[1].SeriesID != "a" {
		t.Fatalf("a work in two series is the first's: %+v", got[1])
	}
}

func TestClassifyNothingHeld(t *testing.T) {
	t.Parallel()
	s := Series{ID: "s", Members: []catalog.MetaSeriesMember{member("1", "2010"), member("2", "2026-11-20")}}
	// No book placed on the series: no gaps, no next, but a preorder is still one.
	check(t, Classify([]Series{s}, []OwnedBook{{WorkID: "other"}}, now, 90), "w2=preorder")
}

func TestReleaseDateRules(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		date           string
		future, recent bool
		window         int
		description    string
	}{
		{"2026-10-31", true, false, 90, "tomorrow"},
		{"2026-10-30", false, true, 90, "today"},
		{"2026-08-01", false, true, 90, "within the window"},
		{"2026-08-01", false, false, 30, "before a short window"},
		{"2026-11", true, false, 90, "next month"},
		{"2026-10", false, true, 90, "this month, from its first day"},
		{"2027", true, false, 90, "next year"},
		{"2026", false, false, 90, "this year, from 1 January"},
		{"", false, false, 90, "none"},
		{"soon", false, false, 90, "unreadable"},
	} {
		if got := releaseIsFuture(tc.date, now); got != tc.future {
			t.Errorf("%s: releaseIsFuture(%q) = %v", tc.description, tc.date, got)
		}
		if got := releasedWithin(tc.date, now, tc.window); got != tc.recent {
			t.Errorf("%s: releasedWithin(%q, %d) = %v", tc.description, tc.date, tc.window, got)
		}
	}
}
