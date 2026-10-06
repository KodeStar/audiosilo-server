package catalog

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// statsFixture is the session fixture (ann = f.user, clock Thu 2026-10-01 09:00
// UTC, "Author/Book" by Ann Author read by Ned Narrator) plus a series book both
// users listen to and a book only bob listens to.
type statsFixture struct {
	*sessionFixture
	bob        int64
	saga, priv Ref
}

func newStatsFixture(t *testing.T) *statsFixture {
	t.Helper()
	f := &statsFixture{sessionFixture: newSessionFixture(t)}
	f.bob = f.addUser(t, "bob", "2025-01-01T00:00:00Z", false)
	f.saga = Ref{LibraryID: f.lib, Path: "Saga/One"}
	f.priv = Ref{LibraryID: f.lib, Path: "Bob/Secret"}
	for _, b := range []*Book{
		{LibraryID: f.lib, RelPath: f.saga.Path, IsFolder: true, Title: "Saga One", Author: "Sam Saga",
			Narrator: "Nora Narrator", Series: "The Saga", Duration: 3600, AddedAt: "2026-09-20T00:00:00Z"},
		{LibraryID: f.lib, RelPath: f.priv.Path, IsFolder: true, Title: "Bob's Secret", Author: "Bob Writer",
			Narrator: "Bob Reader", Series: "Bob Series", Duration: 3600, AddedAt: "2026-09-20T00:00:00Z"},
	} {
		if _, err := f.c.UpsertBook(f.ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *statsFixture) addDaily(t *testing.T, user int64, ref Ref, day string, listened float64, sessions int) {
	t.Helper()
	if _, err := f.c.db.ExecContext(f.ctx,
		`INSERT INTO listening_daily(day, user_id, library_id, rel_path, listened, sessions) VALUES(?, ?, ?, ?, ?, ?)`,
		day, user, ref.LibraryID, ref.Path, listened, sessions); err != nil {
		t.Fatal(err)
	}
}

// finish records user finishing ref at the given time.
func (f *statsFixture) finish(t *testing.T, user int64, ref Ref, at time.Time) {
	t.Helper()
	saved := f.clock
	f.clock = at
	if _, err := f.c.SaveProgress(f.ctx, user, Progress{Ref: ref, Position: 3600, Duration: 3600, Finished: true,
		UpdatedAt: at.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	f.clock = saved
}

// seedAnn is ann's listening: in the period (7 days to the clock) two raw
// sessions and a rolled-up day, and a finish; before it, one session and a finish.
func (f *statsFixture) seedAnn(t *testing.T) {
	t.Helper()
	at := func(m time.Month, d, h int) time.Time { return time.Date(2026, m, d, h, 0, 0, 0, time.UTC) }
	f.addSession(t, f.user, 1, f.book, at(time.October, 1, 8), 30*time.Minute, 1800, "opus", false)   // a Thursday
	f.addSession(t, f.user, 1, f.saga, at(time.September, 29, 10), 30*time.Minute, 1500, "mp3", true) // a Tuesday
	f.addDaily(t, f.user, f.saga, "2026-09-27", 600, 2)
	f.addSession(t, f.user, 1, f.book, at(time.September, 20, 10), 10*time.Minute, 600, "opus", false) // previous period
	f.finish(t, f.user, f.book, at(time.September, 20, 11))
	f.finish(t, f.user, f.saga, at(time.September, 30, 12))
}

// seedBob is bob's listening on the same books and on his own, in both periods,
// raw and rolled up, with finishes in both.
func (f *statsFixture) seedBob(t *testing.T) {
	t.Helper()
	at := func(d, h int) time.Time { return time.Date(2026, 9, d, h, 0, 0, 0, time.UTC) }
	f.addSession(t, f.bob, 2, f.book, at(30, 8), 30*time.Minute, 1200, "aac", false)
	f.addSession(t, f.bob, 2, f.saga, at(29, 10), 20*time.Minute, 1100, "mp3", true) // the same hour as ann's
	f.addSession(t, f.bob, 3, f.priv, at(28, 7), 30*time.Minute, 1700, "flac", false)
	f.addDaily(t, f.bob, f.book, "2026-09-27", 900, 3)
	f.addDaily(t, f.bob, f.priv, "2026-09-26", 500, 1)
	f.addSession(t, f.bob, 2, f.priv, at(20, 9), 10*time.Minute, 300, "flac", false) // previous period
	f.addDaily(t, f.bob, f.saga, "2026-09-19", 400, 1)
	f.finish(t, f.bob, f.book, at(30, 9))
	f.finish(t, f.bob, f.priv, at(21, 9))
	f.finish(t, f.bob, f.saga, at(30, 10))
}

func (f *statsFixture) annStats(t *testing.T, scopes []Scope) *UserStats {
	t.Helper()
	s, err := f.c.UserStatsFor(f.ctx, "7d", f.clock.AddDate(0, 0, -7), f.clock, time.UTC, f.user, scopes)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *statsFixture) allLibrary() []Scope { return []Scope{{LibraryID: f.lib, AllowAll: true}} }

// The caller's stats are exactly their own listening, and another user's
// listening (raw sessions and rolled-up days, on the same books and on others,
// and their finishes) never changes them.
func TestUserStatsOnlyTheCaller(t *testing.T) {
	f := newStatsFixture(t)
	f.seedAnn(t)
	alone := f.annStats(t, f.allLibrary())
	f.seedBob(t)
	s := f.annStats(t, f.allLibrary())
	if !reflect.DeepEqual(alone, s) {
		t.Fatalf("another user's listening changed the caller's stats:\nalone %+v\nwith bob %+v", alone, s)
	}

	if s.Totals != (UserTotals{Listened: 3900, Sessions: 4, Books: 2, Finished: 1}) {
		t.Fatalf("totals = %+v", s.Totals)
	}
	if s.Previous != (UserTotals{Listened: 600, Sessions: 1, Books: 1, Finished: 1}) {
		t.Fatalf("previous = %+v", s.Previous)
	}
	if s.Range != "7d" || s.Timezone != "UTC" || s.Estimated != 0 {
		t.Fatalf("period = %+v, estimated %v", s.Period, s.Estimated)
	}
	want := map[string]float64{"2026-09-27": 600, "2026-09-29": 1500, "2026-10-01": 1800}
	if len(s.Days) != 8 || s.Days[0].Date != "2026-09-24" {
		t.Fatalf("days = %+v", s.Days)
	}
	for _, d := range s.Days {
		if d.Listened != want[d.Date] {
			t.Fatalf("day %s = %v, want %v", d.Date, d.Listened, want[d.Date])
		}
	}
	if s.HourWeekday[3][8] != 1800 || s.HourWeekday[1][10] != 1500 {
		t.Fatalf("hour x weekday: Thu 08 = %v, Tue 10 = %v", s.HourWeekday[3][8], s.HourWeekday[1][10])
	}
	if len(s.TopBooks) != 2 || s.TopBooks[0] != (UserTopBook{LibraryID: f.lib, Path: "Saga/One", Title: "Saga One",
		Author: "Sam Saga", Listened: 2100}) || s.TopBooks[1].Path != f.book.Path || s.TopBooks[1].Listened != 1800 {
		t.Fatalf("top books = %+v", s.TopBooks)
	}
	if !reflect.DeepEqual(s.TopAuthors, []TopPerson{{"Sam Saga", 2100, 1}, {"Ann Author", 1800, 1}}) ||
		!reflect.DeepEqual(s.TopNarrators, []TopPerson{{"Nora Narrator", 2100, 1}, {"Ned Narrator", 1800, 1}}) ||
		!reflect.DeepEqual(s.TopSeries, []TopPerson{{"The Saga", 2100, 1}}) {
		t.Fatalf("top people = %+v / %+v / %+v", s.TopAuthors, s.TopNarrators, s.TopSeries)
	}
	if len(s.FinishedBooks) != 1 || s.FinishedBooks[0] != (FinishedBook{LibraryID: f.lib, Path: "Saga/One",
		Title: "Saga One", Author: "Sam Saga", FinishedAt: "2026-09-30T12:00:00Z"}) {
		t.Fatalf("finished books = %+v", s.FinishedBooks)
	}
	if !reflect.DeepEqual(s.Playback, []PlaybackShare{{Codec: "opus", Listened: 1800, Sessions: 1},
		{Transcoded: true, Codec: "mp3", Listened: 1500, Sessions: 1}}) {
		t.Fatalf("playback = %+v", s.Playback)
	}
	if len(s.Clients) != 1 || s.Clients[0].Devices != 1 {
		t.Fatalf("clients = %+v (ann listened on one device)", s.Clients)
	}

	// And bob's own stats are his, not ann's.
	b, err := f.c.UserStatsFor(f.ctx, "7d", f.clock.AddDate(0, 0, -7), f.clock, time.UTC, f.bob, f.allLibrary())
	if err != nil {
		t.Fatal(err)
	}
	if b.Totals != (UserTotals{Listened: 5400, Sessions: 7, Books: 3, Finished: 2}) ||
		b.Previous != (UserTotals{Listened: 700, Sessions: 2, Books: 2, Finished: 1}) {
		t.Fatalf("bob's totals = %+v, previous %+v", b.Totals, b.Previous)
	}
}

// A book outside the caller's current access (a revoked share) leaves the rows
// naming books - top books, finished books and the authors, narrators and series
// ranked from them - while the caller's totals, days and hours keep its time.
func TestUserStatsDropRevokedBooks(t *testing.T) {
	f := newStatsFixture(t)
	f.seedAnn(t)
	f.seedBob(t)
	full := f.annStats(t, f.allLibrary())

	// Only "Author" is still granted: Saga/One was shared and is no longer.
	s := f.annStats(t, []Scope{{LibraryID: f.lib, Paths: []string{"Author"}}})
	if s.Totals != full.Totals || s.Previous != full.Previous || !reflect.DeepEqual(s.Days, full.Days) ||
		s.HourWeekday != full.HourWeekday || !reflect.DeepEqual(s.Playback, full.Playback) {
		t.Fatalf("the revoked book's time left the caller's totals: %+v, want %+v", s.Totals, full.Totals)
	}
	if len(s.TopBooks) != 1 || s.TopBooks[0].Path != f.book.Path {
		t.Fatalf("top books = %+v, want only the granted book", s.TopBooks)
	}
	if !reflect.DeepEqual(s.TopAuthors, []TopPerson{{"Ann Author", 1800, 1}}) ||
		!reflect.DeepEqual(s.TopNarrators, []TopPerson{{"Ned Narrator", 1800, 1}}) || len(s.TopSeries) != 0 {
		t.Fatalf("the revoked book's people echoed back: %+v / %+v / %+v", s.TopAuthors, s.TopNarrators, s.TopSeries)
	}
	if len(s.FinishedBooks) != 0 || s.Totals.Finished != 1 {
		t.Fatalf("finished books = %+v (want none listed, still 1 counted: %d)", s.FinishedBooks, s.Totals.Finished)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"Saga", "Sam Saga", "Nora"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("%q echoed back after the share was revoked: %s", leak, b)
		}
	}

	// The same path in another library is not granted by this one's scope, and no
	// access at all lists no book.
	other := f.annStats(t, []Scope{{LibraryID: f.lib + 1, AllowAll: true}})
	none := f.annStats(t, nil)
	for _, x := range []*UserStats{other, none} {
		if len(x.TopBooks)+len(x.TopAuthors)+len(x.TopNarrators)+len(x.TopSeries)+len(x.FinishedBooks) != 0 ||
			x.Totals != full.Totals {
			t.Fatalf("stats without access = %+v", x)
		}
	}
}

// The per-user entry points refuse user 0, which the accumulator reads as
// "everyone".
func TestUserStatsNeedAUser(t *testing.T) {
	f := newStatsFixture(t)
	f.seedBob(t)
	from := f.clock.AddDate(0, 0, -7)
	if _, err := f.c.UserStatsFor(f.ctx, "7d", from, f.clock, time.UTC, 0, f.allLibrary()); !errors.Is(err, errNoUser) {
		t.Fatalf("UserStatsFor(0) = %v, want errNoUser", err)
	}
	if _, err := f.c.UserListeningFor(f.ctx, "7d", from, f.clock, time.UTC, 0); !errors.Is(err, errNoUser) {
		t.Fatalf("UserListeningFor(0) = %v, want errNoUser", err)
	}
	if _, err := f.c.GoalStatusFor(f.ctx, 0, f.clock, time.UTC); !errors.Is(err, errNoUser) {
		t.Fatalf("GoalStatusFor(0) = %v, want errNoUser", err)
	}
}

// UserListeningFor is the caller's days only: the same as their stats' days.
func TestUserListeningFor(t *testing.T) {
	f := newStatsFixture(t)
	f.seedAnn(t)
	f.seedBob(t)
	from := f.clock.AddDate(0, 0, -7)
	l, err := f.c.UserListeningFor(f.ctx, "7d", from, f.clock, time.UTC, f.user)
	if err != nil {
		t.Fatal(err)
	}
	if l.Range != "7d" || !reflect.DeepEqual(l.Days, f.annStats(t, f.allLibrary()).Days) {
		t.Fatalf("ann's days = %+v", l.Days)
	}
	var sum float64
	for _, d := range l.Days {
		sum += d.Listened
	}
	if sum != 3900 {
		t.Fatalf("ann's days sum to %v, want 3900", sum)
	}
}

func TestScopesAllow(t *testing.T) {
	scopes := []Scope{{LibraryID: 1, Paths: []string{"Author"}}, {LibraryID: 2, AllowAll: true}}
	for _, c := range []struct {
		ref  Ref
		want bool
	}{
		{Ref{1, "Author/Book"}, true},
		{Ref{1, "Author"}, true},
		{Ref{2, "Anything/At/All"}, true},
		{Ref{1, "Authority/Book"}, false}, // a sibling sharing the prefix
		{Ref{1, "Other/Book"}, false},
		{Ref{3, "Author/Book"}, false}, // the granted path, in a library not granted
	} {
		if got := scopesAllow(scopes, c.ref); got != c.want {
			t.Errorf("scopesAllow(%+v) = %v, want %v", c.ref, got, c.want)
		}
	}
	if scopesAllow(nil, Ref{1, "Author/Book"}) {
		t.Error("no scopes granted a path")
	}
}

// A goal is per user: set, read, cleared, validated, with this calendar year's
// finishes (server time) of that user only; deleting the user purges it.
func TestListeningGoal(t *testing.T) {
	f := newStatsFixture(t) // clock 2026-10-01
	g, err := f.c.GoalStatusFor(f.ctx, f.user, f.clock, time.UTC)
	if err != nil || g.Goal != nil || g.Year != "2026" || g.Finished != 0 {
		t.Fatalf("no goal = %+v, %v", g, err)
	}
	for _, bad := range []int{0, -1, MaxBooksPerYear + 1} {
		if err := f.c.SetListeningGoal(f.ctx, f.user, bad); !errors.Is(err, ErrInvalidGoal) {
			t.Fatalf("goal %d = %v, want ErrInvalidGoal", bad, err)
		}
	}
	for _, ok := range []int{1, MaxBooksPerYear, 24} {
		if err := f.c.SetListeningGoal(f.ctx, f.user, ok); err != nil {
			t.Fatalf("goal %d = %v", ok, err)
		}
	}
	if err := f.c.SetListeningGoal(f.ctx, f.bob, 5); err != nil {
		t.Fatal(err)
	}
	f.finish(t, f.user, f.book, time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC))
	f.finish(t, f.user, f.saga, time.Date(2025, 12, 31, 10, 0, 0, 0, time.UTC)) // last year
	f.finish(t, f.bob, f.priv, time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC))
	f.finish(t, f.bob, f.book, time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC))

	g, err = f.c.GoalStatusFor(f.ctx, f.user, f.clock, time.UTC)
	if err != nil || g.Goal == nil || g.Goal.BooksPerYear != 24 || g.Goal.UpdatedAt != "2026-10-01T09:00:00Z" || g.Finished != 1 {
		t.Fatalf("ann's goal = %+v (%+v), %v", g, g.Goal, err)
	}
	gb, err := f.c.GoalStatusFor(f.ctx, f.bob, f.clock, time.UTC)
	if err != nil || gb.Goal == nil || gb.Goal.BooksPerYear != 5 || gb.Finished != 2 {
		t.Fatalf("bob's goal = %+v, %v", gb, err)
	}

	for range 2 { // idempotent
		if err := f.c.DeleteListeningGoal(f.ctx, f.user); err != nil {
			t.Fatal(err)
		}
	}
	if g, _ := f.c.GoalStatusFor(f.ctx, f.user, f.clock, time.UTC); g.Goal != nil {
		t.Fatalf("deleted goal = %+v", g.Goal)
	}
	if gb, _ := f.c.GoalStatusFor(f.ctx, f.bob, f.clock, time.UTC); gb.Goal == nil {
		t.Fatal("deleting ann's goal cleared bob's")
	}

	if _, err := f.c.db.ExecContext(f.ctx, `DELETE FROM users WHERE id = ?`, f.bob); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.c.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM listening_goals`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("goals left after the user was deleted = %d, %v", n, err)
	}
}
