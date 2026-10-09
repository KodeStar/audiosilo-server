package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// houseEnv is a catalog on a clock the test holds, with one library.
type houseEnv struct {
	c     *Catalog
	ctx   context.Context
	now   time.Time
	libID int64
}

func newHouseEnv(t *testing.T) *houseEnv {
	t.Helper()
	e := &houseEnv{ctx: context.Background(), now: time.Date(2026, 10, 9, 20, 0, 0, 0, time.UTC)}
	e.c = New(storetest.Open(t), func() time.Time { return e.now })
	lib, err := e.c.CreateLibrary(e.ctx, Library{Name: "Books", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	e.libID = lib.ID
	return e
}

// user adds an account (disabled and/or demo as asked) sharing its listening when share.
func (e *houseEnv) user(t *testing.T, name string, share, disabled, demo bool) int64 {
	t.Helper()
	stamp := e.now.Format(time.RFC3339Nano)
	res, err := e.c.db.ExecContext(e.ctx,
		`INSERT INTO users(username, password_hash, role, disabled, is_demo, created_at, updated_at) VALUES(?, '', 'user', ?, ?, ?, ?)`,
		name, disabled, demo, stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if share {
		if err := e.c.SetHouseholdSharing(e.ctx, id, true); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// session records a session of userID on path whose newest save was ago, with
// listened seconds in it, from an import when importID > 0.
func (e *houseEnv) session(t *testing.T, userID int64, path string, ago time.Duration, listened float64, importID int64, backfilled bool) {
	t.Helper()
	last := formatSessionTime(e.now.Add(-ago))
	if _, err := e.c.db.ExecContext(e.ctx,
		`INSERT INTO listening_sessions(user_id, library_id, rel_path, token_id, device_name, client_app,
		     started_at, last_at, start_pos, end_pos, duration, listened, import_id, backfilled)
		 VALUES(?, ?, ?, 99, 'Sam''s phone', 'AudioSilo', ?, ?, 0, 1234.5, 0, ?, ?, ?)`,
		userID, e.libID, path, last, last, listened, importID, backfilled); err != nil {
		t.Fatal(err)
	}
}

func (e *houseEnv) house(t *testing.T, caller int64, scopes []Scope) *HouseListening {
	t.Helper()
	h, err := e.c.HouseListening(e.ctx, caller, scopes)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHouseholdSharingSetting(t *testing.T) {
	t.Parallel()
	e := newHouseEnv(t)
	me := e.user(t, "me", false, false, false)
	if on, err := e.c.HouseholdSharing(e.ctx, me); err != nil || on {
		t.Fatalf("default = %v, %v; want off", on, err)
	}
	for _, want := range []bool{true, false, true} {
		if err := e.c.SetHouseholdSharing(e.ctx, me, want); err != nil {
			t.Fatal(err)
		}
		if on, err := e.c.HouseholdSharing(e.ctx, me); err != nil || on != want {
			t.Fatalf("after set %v = %v, %v", want, on, err)
		}
	}
	// The account's delete purges it (FK cascade).
	if _, err := e.c.db.ExecContext(e.ctx, `DELETE FROM users WHERE id = ?`, me); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.c.db.QueryRowContext(e.ctx, `SELECT COUNT(*) FROM household_sharing`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("household rows after the account's delete = %d, %v", n, err)
	}
}

func TestHouseListeningRules(t *testing.T) {
	t.Parallel()
	e := newHouseEnv(t)
	all := []Scope{{LibraryID: e.libID, AllowAll: true}}
	me := e.user(t, "me", false, false, false)
	sam := e.user(t, "sam", true, false, false)   // playing now, newest book wins
	pat := e.user(t, "pat", true, false, false)   // paused
	rae := e.user(t, "rae", true, false, false)   // earlier today
	ann := e.user(t, "ann", false, false, false)  // not sharing
	dis := e.user(t, "dis", true, true, false)    // disabled
	dem := e.user(t, "demo_x", true, false, true) // demo
	old := e.user(t, "old", true, false, false)   // last listened over a day ago
	imp := e.user(t, "imp", true, false, false)   // only an imported session
	bkf := e.user(t, "bkf", true, false, false)   // only a backfilled session
	nul := e.user(t, "nul", true, false, false)   // a session with nothing listened
	hid := e.user(t, "hid", true, false, false)   // newest book outside the caller's scope
	e.session(t, sam, "Sam/Older", 2*time.Hour, 100, 0, false)
	e.session(t, sam, "Sam/Newer", 30*time.Second, 100, 0, false)
	e.session(t, pat, "Pat/Book", 5*time.Minute, 100, 0, false)
	e.session(t, rae, "Rae/Book", 3*time.Hour, 100, 0, false)
	e.session(t, ann, "Ann/Book", time.Second, 100, 0, false)
	e.session(t, dis, "Dis/Book", time.Second, 100, 0, false)
	e.session(t, dem, "Demo/Book", time.Second, 100, 0, false)
	e.session(t, old, "Old/Book", HouseWindow+time.Minute, 100, 0, false)
	e.session(t, imp, "Imp/Book", time.Second, 100, 7, false)
	e.session(t, bkf, "Bkf/Book", time.Second, 100, 0, true)
	e.session(t, nul, "Nul/Book", time.Second, 0, 0, false)
	e.session(t, hid, "Allowed/Older", time.Hour, 100, 0, false)
	e.session(t, hid, "Hidden/Newer", time.Minute, 100, 0, false)
	e.session(t, me, "Me/Book", time.Second, 100, 0, false)

	// Denied: a caller who doesn't share sees nobody (reciprocal).
	if h := e.house(t, me, all); h.Sharing || len(h.Listening) != 0 || h.Listening == nil {
		t.Fatalf("not sharing = %+v; want {false, []}", h)
	}

	if err := e.c.SetHouseholdSharing(e.ctx, me, true); err != nil {
		t.Fatal(err)
	}
	// Allowed: the sharing housemates, newest first, one row each, never the caller.
	h := e.house(t, me, all)
	got := map[string]HouseListeningRow{}
	var order []string
	for _, r := range h.Listening {
		got[r.Username] = r
		order = append(order, r.Username)
	}
	if !h.Sharing || len(order) != 4 || order[0] != "sam" || order[1] != "hid" || order[2] != "pat" || order[3] != "rae" {
		t.Fatalf("house = %v (%+v)", order, h)
	}
	s := got["sam"]
	if s.UserID != sam || s.Path != "Sam/Newer" || s.State != HousePlaying || s.Position != 1234.5 ||
		s.Title != "Newer" || s.Author != nil || s.Duration != nil || s.Chapter != nil || s.ChapterIndex != nil ||
		s.UpdatedAt != formatSessionTime(e.now.Add(-30*time.Second)) || s.LibraryID != e.libID {
		t.Fatalf("sam = %+v", s)
	}
	if got["pat"].State != HousePaused || got["rae"].State != HouseRecent {
		t.Fatalf("states = pat %q, rae %q", got["pat"].State, got["rae"].State)
	}

	// Denied: a newest book outside the caller's scopes drops the person (never an
	// older book in scope instead).
	scoped := []Scope{{LibraryID: e.libID, Paths: []string{"Sam", "Allowed"}}}
	h = e.house(t, me, scoped)
	if len(h.Listening) != 1 || h.Listening[0].Username != "sam" {
		t.Fatalf("scoped house = %+v; want sam only", h.Listening)
	}
	// ... and no scope at all sees nobody.
	if h := e.house(t, me, nil); len(h.Listening) != 0 {
		t.Fatalf("no-scope house = %+v", h.Listening)
	}

	// A housemate who stops sharing leaves the house.
	if err := e.c.SetHouseholdSharing(e.ctx, sam, false); err != nil {
		t.Fatal(err)
	}
	for _, r := range e.house(t, me, all).Listening {
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
		{0, HousePlaying}, {livePlaying, HousePlaying}, {livePlaying + time.Millisecond, HousePaused},
		{SessionGap, HousePaused}, {SessionGap + time.Millisecond, HouseRecent}, {20 * time.Hour, HouseRecent},
	} {
		if got := houseState(c.age); got != c.want {
			t.Errorf("houseState(%v) = %q, want %q", c.age, got, c.want)
		}
	}
}
