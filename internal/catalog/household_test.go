package catalog

import (
	"strings"
	"testing"
	"time"
)

// share turns sharing on for each of ids, as of two windows ago, so every
// session a test records counts as listening done while sharing was on.
func (f *sessionFixture) share(t *testing.T, ids ...int64) {
	t.Helper()
	now := f.clock
	f.clock = now.Add(-2 * houseWindow)
	defer func() { f.clock = now }()
	f.setShare(t, true, ids...)
}

// setShare saves the sharing choice on for each of ids, at the fixture's clock.
func (f *sessionFixture) setShare(t *testing.T, on bool, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		if err := f.c.SetHouseholdSharing(f.ctx, id, on); err != nil {
			t.Fatal(err)
		}
	}
}

// houseSession records a session of user on path (in the fixture's library)
// whose newest save was ago, with listened seconds in it, and returns its id.
func (f *sessionFixture) houseSession(t *testing.T, user int64, path string, ago time.Duration, listened float64) int64 {
	t.Helper()
	return f.addSession(t, user, user, Ref{LibraryID: f.lib, Path: path}, f.clock.Add(-ago), 0, listened, "", false)
}

// exec runs a statement the fixture's helpers don't cover.
func (f *sessionFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.c.db.ExecContext(f.ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func (f *sessionFixture) house(t *testing.T, caller int64, scopes []Scope) []HouseListeningRow {
	t.Helper()
	rows, err := f.c.HouseListening(f.ctx, caller, scopes)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestHouseholdSharingSetting(t *testing.T) {
	t.Parallel()
	f := newSessionFixture(t)
	me := f.addUser(t, "me", "2025-01-01T00:00:00Z", false)
	if on, err := f.c.HouseholdSharing(f.ctx, me); err != nil || on {
		t.Fatalf("default = %v, %v; want off", on, err)
	}
	for _, want := range []bool{true, false, true} {
		if err := f.c.SetHouseholdSharing(f.ctx, me, want); err != nil {
			t.Fatal(err)
		}
		if on, err := f.c.HouseholdSharing(f.ctx, me); err != nil || on != want {
			t.Fatalf("after set %v = %v, %v", want, on, err)
		}
	}
	// The account's delete purges it (FK cascade).
	f.exec(t, `DELETE FROM users WHERE id = ?`, me)
	var n int
	if err := f.c.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM household_sharing`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("household rows after the account's delete = %d, %v", n, err)
	}
}

func TestHouseListeningRules(t *testing.T) {
	t.Parallel()
	f := newSessionFixture(t)
	all := []Scope{{LibraryID: f.lib, AllowAll: true}}
	user := func(name string) int64 { return f.addUser(t, name, "2025-01-01T00:00:00Z", false) }
	me := user("me")
	sam := user("sam") // playing now on the indexed book, newest book wins
	pat := user("pat") // paused, a book not indexed whose session knows its length
	rae := user("rae") // earlier today, a book not indexed
	ann := user("ann") // not sharing
	dis := user("dis") // disabled
	dem := f.addUser(t, "demo_x", "2025-01-01T00:00:00Z", true)
	old := user("old") // last listened over a day ago
	imp := user("imp") // only an imported session
	bkf := user("bkf") // only a backfilled session
	nul := user("nul") // a session with nothing listened
	hid := user("hid") // newest book outside the caller's scope
	f.share(t, sam, pat, rae, dis, dem, old, imp, bkf, nul, hid)
	f.exec(t, `UPDATE users SET disabled = 1 WHERE id = ?`, dis)

	f.houseSession(t, sam, "Sam/Older", 2*time.Hour, 100)
	samNow := f.houseSession(t, sam, f.book.Path, 30*time.Second, 100)
	f.exec(t, `UPDATE listening_sessions SET end_pos = 3700 WHERE id = ?`, samNow) // in chapter "Middle"
	patNow := f.houseSession(t, pat, "Pat/Book", 5*time.Minute, 100)
	f.exec(t, `UPDATE listening_sessions SET end_pos = 12.5, duration = 600 WHERE id = ?`, patNow)
	f.houseSession(t, rae, "Rae/Book", 3*time.Hour, 100)
	f.houseSession(t, ann, "Ann/Book", time.Second, 100)
	f.houseSession(t, dis, "Dis/Book", time.Second, 100)
	f.houseSession(t, dem, "Demo/Book", time.Second, 100)
	f.houseSession(t, old, "Old/Book", houseWindow+time.Minute, 100)
	f.exec(t, `UPDATE listening_sessions SET import_id = 7 WHERE id = ?`, f.houseSession(t, imp, "Imp/Book", time.Second, 100))
	f.exec(t, `UPDATE listening_sessions SET backfilled = 1 WHERE id = ?`, f.houseSession(t, bkf, "Bkf/Book", time.Second, 100))
	f.houseSession(t, nul, "Nul/Book", time.Second, 0)
	f.houseSession(t, hid, "Allowed/Older", time.Hour, 100)
	f.houseSession(t, hid, "Hidden/Newer", time.Minute, 100)
	f.houseSession(t, me, "Me/Book", time.Second, 100)

	// Denied: a caller who doesn't share sees nobody (reciprocal).
	if rows := f.house(t, me, all); rows == nil || len(rows) != 0 {
		t.Fatalf("not sharing = %+v; want []", rows)
	}

	f.share(t, me)
	// Allowed: the sharing housemates, newest first, one row each, never the caller.
	rows := f.house(t, me, all)
	got := map[string]HouseListeningRow{}
	var order []string
	for _, r := range rows {
		got[r.Username] = r
		order = append(order, r.Username)
	}
	if len(order) != 4 || order[0] != "sam" || order[1] != "hid" || order[2] != "pat" || order[3] != "rae" {
		t.Fatalf("house = %v (%+v)", order, rows)
	}
	// The indexed book: its title, author, length and the chapter at the position.
	s := got["sam"]
	if s.UserID != sam || s.LibraryID != f.lib || s.Path != f.book.Path || s.State != SessionPlaying ||
		s.Position != 3700 || s.Title != "The Book" || s.Author == nil || *s.Author != "Ann Author" ||
		s.Duration == nil || *s.Duration != 7200 || s.Chapter == nil || *s.Chapter != "Middle" ||
		s.ChapterIndex == nil || *s.ChapterIndex != 1 || s.UpdatedAt != formatSessionTime(f.clock.Add(-30*time.Second)) {
		t.Fatalf("sam = %+v", s)
	}
	// A book not indexed: the path names it, the session's length if it knows one.
	p, r := got["pat"], got["rae"]
	if p.State != SessionPaused || p.Title != "Book" || p.Author != nil || p.Duration == nil || *p.Duration != 600 ||
		p.Position != 12.5 || p.Chapter != nil || p.ChapterIndex != nil {
		t.Fatalf("pat = %+v", p)
	}
	if r.State != HouseRecent || r.Duration != nil {
		t.Fatalf("rae = %+v", r)
	}

	// Denied: a newest book outside the caller's scopes drops the person (never an
	// older book in scope instead).
	scoped := []Scope{{LibraryID: f.lib, Paths: []string{"Author", "Allowed"}}}
	if rows := f.house(t, me, scoped); len(rows) != 1 || rows[0].Username != "sam" {
		t.Fatalf("scoped house = %+v; want sam only", rows)
	}
	// ... and no scope at all sees nobody.
	if rows := f.house(t, me, nil); len(rows) != 0 {
		t.Fatalf("no-scope house = %+v", rows)
	}

	// A housemate who stops sharing leaves the house.
	if err := f.c.SetHouseholdSharing(f.ctx, sam, false); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.house(t, me, all) {
		if r.UserID == sam {
			t.Fatal("sam stopped sharing but still shows")
		}
	}
}

// The states follow the newest save's age: playing within a minute, paused
// within SessionGap, else recent.
func TestHouseState(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		age  time.Duration
		want string
	}{
		{0, SessionPlaying}, {livePlaying, SessionPlaying}, {livePlaying + time.Millisecond, SessionPaused},
		{SessionGap, SessionPaused}, {SessionGap + time.Millisecond, HouseRecent}, {20 * time.Hour, HouseRecent},
	} {
		if got := houseState(c.age); got != c.want {
			t.Errorf("houseState(%v) = %q, want %q", c.age, got, c.want)
		}
	}
}

// Listening done while sharing was off never shows, even after sharing is turned
// back on within houseWindow (server#221); listening after it is turned on does,
// and saving "on" again while on doesn't move that boundary.
func TestHouseListeningSharingSince(t *testing.T) {
	t.Parallel()
	f := newSessionFixture(t)
	all := []Scope{{LibraryID: f.lib, AllowAll: true}}
	me := f.addUser(t, "me", "2025-01-01T00:00:00Z", false)
	sam := f.addUser(t, "sam", "2025-01-01T00:00:00Z", false)
	f.share(t, me)
	names := func() []string {
		var out []string
		for _, r := range f.house(t, me, all) {
			out = append(out, r.Username+":"+r.Path)
		}
		return out
	}

	// sam shares, listens, stops sharing, listens to another book, turns it back on.
	f.setShare(t, true, sam)
	f.clock = f.clock.Add(time.Minute)
	f.houseSession(t, sam, "Sam/On", 0, 100)
	f.clock = f.clock.Add(time.Minute)
	f.setShare(t, false, sam)
	f.clock = f.clock.Add(time.Minute)
	f.houseSession(t, sam, "Sam/Off", 0, 100)
	f.clock = f.clock.Add(time.Minute)
	f.setShare(t, true, sam)

	// Denied: neither the off-time book nor an older on-time one stands in.
	if got := names(); len(got) != 0 {
		t.Fatalf("after turning sharing back on = %v; want nobody", got)
	}
	// Denied: a session that only ended while sharing was off, the moment before
	// turning it on, stays hidden too.
	f.houseSession(t, sam, "Sam/JustBefore", time.Millisecond, 100)
	if got := names(); len(got) != 0 {
		t.Fatalf("a save just before sharing = %v; want nobody", got)
	}

	// Allowed: listening after sharing is turned on shows (saved the same moment counts).
	f.houseSession(t, sam, "Sam/Now", 0, 100)
	if got := names(); len(got) != 1 || got[0] != "sam:Sam/Now" {
		t.Fatalf("after listening while sharing = %v", got)
	}

	// Allowed: saving "on" again later doesn't hide what sam listened to since.
	f.clock = f.clock.Add(time.Hour)
	f.setShare(t, true, sam)
	if got := names(); len(got) != 1 || got[0] != "sam:Sam/Now" {
		t.Fatalf("after a repeated share-on = %v", got)
	}
	// Denied: turning it off and on again moves the boundary past it.
	f.setShare(t, false, sam)
	f.clock = f.clock.Add(time.Minute)
	f.setShare(t, true, sam)
	if got := names(); len(got) != 0 {
		t.Fatalf("after off and on again = %v; want nobody", got)
	}
}

// The plan HouseListening's comment records: driven from the caller's row, one
// index walk per sharer from the later bound (server#221 kept it).
func TestHouseListeningPlan(t *testing.T) {
	t.Parallel()
	f := newSessionFixture(t)
	got := queryPlan(t, f.c, houseListeningSQL, []any{formatSessionTime(f.clock), int64(1)})
	want := []string{
		"SEARCH me USING INTEGER PRIMARY KEY (rowid=?)",
		"SCAN h",
		"SEARCH u USING INTEGER PRIMARY KEY (rowid=?)",
		"SEARCH w USING INTEGER PRIMARY KEY (rowid=?)",
		"CORRELATED SCALAR SUBQUERY 1",
		"SEARCH s USING INDEX idx_sessions_user_last (user_id=? AND last_at>?)",
		"SEARCH b USING INDEX sqlite_autoindex_books_1 (library_id=? AND rel_path=?) LEFT-JOIN",
		"USE TEMP B-TREE FOR ORDER BY",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("plan:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
