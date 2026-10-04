package catalog

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// sessionFixture is a catalog with a controllable clock, one library, one user
// and a chaptered book at "Author/Book" (two hours, codec opus).
type sessionFixture struct {
	c     *Catalog
	ctx   context.Context
	clock time.Time
	lib   int64
	user  int64
	book  Ref
}

func newSessionFixture(t *testing.T) *sessionFixture {
	t.Helper()
	c, ctx := newTestCatalog(t)
	f := &sessionFixture{c: c, ctx: ctx, clock: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	c.now = func() time.Time { return f.clock }
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if err != nil {
		t.Fatal(err)
	}
	f.lib = lib.ID
	f.user = seedUser(t, c, ctx)
	f.book = Ref{LibraryID: lib.ID, Path: "Author/Book"}
	if _, err := c.UpsertBook(ctx, &Book{
		LibraryID: lib.ID, RelPath: "Author/Book", IsFolder: true, Title: "The Book", Author: "Ann Author",
		Narrator: "Ned Narrator", Duration: 7200, Codec: "opus", Size: 1000, AddedAt: "2026-09-20T00:00:00Z",
		Chapters: []metadata.Chapter{
			{Index: 0, Title: "Opening", BookOffset: 0, End: 3600},
			{Index: 1, Title: "Middle", BookOffset: 3600, End: 7200},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

// beat sends a heartbeat from token at position, at the fixture's clock.
func (f *sessionFixture) beat(t *testing.T, token int64, pos float64) {
	t.Helper()
	f.beatRef(t, token, f.book, pos, false)
}

// listen plays 15 s from pos on ref: two saves, so the session records listening
// (a single save opens a session that stays hidden).
func (f *sessionFixture) listen(t *testing.T, token int64, ref Ref, pos float64) {
	t.Helper()
	f.beatRef(t, token, ref, pos, false)
	f.clock = f.clock.Add(15 * time.Second)
	f.beatRef(t, token, ref, pos+15, false)
}

func (f *sessionFixture) beatRef(t *testing.T, token int64, ref Ref, pos float64, transcoded bool) {
	t.Helper()
	if err := f.c.RecordHeartbeat(f.ctx, Heartbeat{
		UserID: f.user, Ref: ref, TokenID: token, DeviceName: "iPhone",
		Client:   Client{App: "AudioSilo", Version: "1.4.2", Platform: "ios"},
		Position: pos, Duration: 7200, Speed: 1, Transcoded: transcoded, At: f.clock,
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *sessionFixture) sessions(t *testing.T) []Session {
	t.Helper()
	out, _, err := f.c.ListSessions(f.ctx, SessionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestListenedBetween(t *testing.T) {
	cases := []struct {
		name     string
		wall     time.Duration
		advanced float64
		speed    float64
		want     float64
	}{
		{"normal", 15 * time.Second, 15, 1, 15},
		{"faster speed is less wall time", 15 * time.Second, 30, 2, 15},
		{"seek forward is capped by the wall clock", 15 * time.Second, 3600, 1, 15},
		{"pause then resume counts only playback", 5 * time.Minute, 15, 1, 15},
		{"seek back adds nothing", 15 * time.Second, -100, 1, 0},
		{"no time passed", 0, 10, 1, 0},
		{"zero speed reads as 1", 10 * time.Second, 10, 0, 10},
	}
	for _, tc := range cases {
		if got := listenedBetween(tc.wall, tc.advanced, tc.speed); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRecordHeartbeatBuildsSessions(t *testing.T) {
	f := newSessionFixture(t)
	f.beat(t, 1, 100)
	for i := 0; i < 4; i++ {
		f.clock = f.clock.Add(15 * time.Second)
		f.beat(t, 1, 100+float64(i+1)*15)
	}
	s := f.sessions(t)
	if len(s) != 1 {
		t.Fatalf("want one session, got %d", len(s))
	}
	got := s[0]
	if got.Listened != 60 || got.StartPos != 100 || got.EndPos != 160 {
		t.Fatalf("session = listened %v, %v..%v; want 60, 100..160", got.Listened, got.StartPos, got.EndPos)
	}
	if got.Codec != "opus" || got.Title != "The Book" || got.DeviceName != "iPhone" ||
		got.Client == nil || got.Client.Version != "1.4.2" || got.State != SessionPlaying {
		t.Fatalf("session details wrong: %+v", got)
	}

	// Another device on the same book is its own session.
	f.listen(t, 2, f.book, 500)
	if n := len(f.sessions(t)); n != 2 {
		t.Fatalf("a second device should open a second session, got %d", n)
	}

	// A gap longer than SessionGap starts a new session on the same device.
	f.clock = f.clock.Add(SessionGap + time.Second)
	f.listen(t, 1, f.book, 160)
	if n := len(f.sessions(t)); n != 3 {
		t.Fatalf("a save after the gap should start a new session, got %d", n)
	}
}

func TestRecordHeartbeatContinuesAcrossALockedScreen(t *testing.T) {
	f := newSessionFixture(t)
	f.beat(t, 1, 100)
	f.clock = f.clock.Add(15 * time.Second)
	f.beat(t, 1, 115)
	// Android stops the player's save timer while the phone is locked: the next
	// save comes an hour later with the position an hour further on.
	f.clock = f.clock.Add(time.Hour)
	f.beat(t, 1, 115+3600)
	s := f.sessions(t)
	if len(s) != 1 || s[0].Listened != 3615 {
		t.Fatalf("one session with 3615 s listened expected, got %+v", s)
	}
	// After a long gap with the position barely moved, a save starts a new session.
	f.clock = f.clock.Add(time.Hour)
	f.beat(t, 1, 115+3600+30)
	f.clock = f.clock.Add(15 * time.Second)
	f.beat(t, 1, 115+3600+45)
	if s := f.sessions(t); len(s) != 2 || s[0].Listened != 15 {
		t.Fatalf("a paused hour then 15 s should be a second session of 15 s, got %+v", s)
	}
}

// TestRecordHeartbeatJumpAfterLongGapIsNotListening: a "mark finished" (or a seek
// far ahead) hours after the last save moves the position much further than the
// time that passed; it must not resume the old session as hours of listening.
func TestRecordHeartbeatJumpAfterLongGapIsNotListening(t *testing.T) {
	f := newSessionFixture(t)
	f.listen(t, 1, f.book, 100)
	f.clock = f.clock.Add(5 * time.Hour)
	f.beat(t, 1, 100+15+20*3600) // the end of a 20 h book
	s := f.sessions(t)
	if len(s) != 1 || s[0].Listened != 15 {
		t.Fatalf("only the 15 s really played should count, got %+v", s)
	}
}

func TestSessionsWithoutListeningAreHidden(t *testing.T) {
	f := newSessionFixture(t)
	f.beat(t, 1, 7200) // a single save: "mark finished", or another app syncing
	if s := f.sessions(t); len(s) != 0 {
		t.Fatalf("a session with nothing listened is not listed: %+v", s)
	}
	if live, _ := f.c.LiveSessions(f.ctx); len(live) != 0 {
		t.Fatalf("nor live: %+v", live)
	}
	a, err := f.c.ActivityFor(f.ctx, "7d", f.clock.AddDate(0, 0, -7), f.clock.Add(time.Second), time.UTC)
	if err != nil || a.Totals.Sessions != 0 || a.Totals.Listeners != 0 {
		t.Fatalf("nor counted: %+v %v", a.Totals, err)
	}
	// Once it can no longer be continued, retention deletes it.
	f.clock = f.clock.Add(resumeWindow + time.Minute)
	if _, err := f.c.PruneSessions(f.ctx, f.clock.Add(-SessionRetention), time.UTC); err != nil {
		t.Fatal(err)
	}
	var n int
	f.c.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM listening_sessions`).Scan(&n)
	if n != 0 {
		t.Fatalf("an empty session should be deleted, %d left", n)
	}
}

func TestRecordHeartbeatTranscodedIsSticky(t *testing.T) {
	f := newSessionFixture(t)
	f.beatRef(t, 1, f.book, 10, true)
	f.clock = f.clock.Add(15 * time.Second)
	f.beatRef(t, 1, f.book, 25, false)
	if s := f.sessions(t); !s[0].Transcoded {
		t.Fatal("a session that streamed through the transcoder stays marked")
	}
}

func TestLiveSessionsStatesAndOnePerDevice(t *testing.T) {
	f := newSessionFixture(t)
	other := Ref{LibraryID: f.lib, Path: "Other.m4b"}
	f.listen(t, 1, f.book, 3700) // device 1 on the book, in chapter "Middle"
	f.clock = f.clock.Add(30 * time.Second)
	f.listen(t, 1, other, 5)   // device 1 moved on to another book
	f.listen(t, 2, f.book, 10) // device 2 on the book
	live, err := f.c.LiveSessions(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 {
		t.Fatalf("want one live session per device (2), got %d", len(live))
	}
	for _, s := range live {
		if s.DeviceID == 1 && s.Path != "Other.m4b" {
			t.Fatalf("device 1 should show the book it is on now, got %q", s.Path)
		}
		if s.DeviceID == 2 && s.Chapter != "Opening" {
			t.Fatalf("device 2 chapter = %q, want Opening", s.Chapter)
		}
	}

	// A device that opens another book (one save, nothing listened yet) no longer
	// shows its previous book as live.
	third := Ref{LibraryID: f.lib, Path: "Third.m4b"}
	f.beatRef(t, 1, third, 0, false)
	live, _ = f.c.LiveSessions(f.ctx)
	for _, s := range live {
		if s.DeviceID == 1 {
			t.Fatalf("device 1 moved on to a third book and should not show %q as live", s.Path)
		}
	}
	f.listen(t, 1, other, 20) // back to the other book for the rest of the test

	f.clock = f.clock.Add(2 * time.Minute)
	live, _ = f.c.LiveSessions(f.ctx)
	if len(live) != 2 || live[0].State != SessionPaused {
		t.Fatalf("after two quiet minutes sessions read as paused: %+v", live)
	}
	f.clock = f.clock.Add(SessionGap)
	live, _ = f.c.LiveSessions(f.ctx)
	if len(live) != 0 {
		t.Fatalf("closed sessions are not live: %+v", live)
	}
	if s := f.sessions(t); s[0].State != SessionEnded {
		t.Fatalf("history state = %q, want ended", s[0].State)
	}
}

func TestListSessionsFiltersAndPages(t *testing.T) {
	f := newSessionFixture(t)
	other := Ref{LibraryID: f.lib, Path: "Other.m4b"}
	for i := 0; i < 5; i++ {
		f.clock = f.clock.Add(SessionGap + time.Minute)
		ref := f.book
		if i%2 == 1 {
			ref = other
		}
		f.listen(t, 1, ref, 0)
	}
	page, next, err := f.c.ListSessions(f.ctx, SessionFilter{Limit: 2})
	if err != nil || len(page) != 2 || next == 0 {
		t.Fatalf("page 1: %d sessions, next %d, err %v", len(page), next, err)
	}
	page2, next2, _ := f.c.ListSessions(f.ctx, SessionFilter{Limit: 2, Before: next})
	if len(page2) != 2 || page2[0].ID >= page[1].ID || next2 == 0 {
		t.Fatalf("page 2 should continue below page 1: %+v", page2)
	}
	book, _, _ := f.c.ListSessions(f.ctx, SessionFilter{LibraryID: f.lib, Path: "Author/Book"})
	if len(book) != 3 {
		t.Fatalf("book filter: want 3, got %d", len(book))
	}
	none, _, _ := f.c.ListSessions(f.ctx, SessionFilter{UserID: f.user + 99})
	if len(none) != 0 {
		t.Fatalf("user filter: want none, got %d", len(none))
	}
}

func TestSessionsMoveWithTheBook(t *testing.T) {
	f := newSessionFixture(t)
	f.listen(t, 1, f.book, 10)
	if _, err := f.c.db.ExecContext(f.ctx,
		`INSERT INTO listening_daily(day, user_id, library_id, rel_path, listened, sessions) VALUES('2025-01-01', ?, ?, ?, 60, 1)`,
		f.user, f.lib, f.book.Path); err != nil {
		t.Fatal(err)
	}
	if err := f.c.MoveDurableState(f.ctx, f.lib, "Author/Book", "Author/Book (2)"); err != nil {
		t.Fatal(err)
	}
	if s := f.sessions(t); s[0].Path != "Author/Book (2)" {
		t.Fatalf("session path = %q after the move", s[0].Path)
	}
	var n int
	f.c.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM listening_daily WHERE rel_path = 'Author/Book (2)'`).Scan(&n)
	if n != 1 {
		t.Fatal("rolled-up days should move with the book")
	}
}

func TestPruneSessionsRollsUpByLocalDay(t *testing.T) {
	f := newSessionFixture(t)
	// A session from 23:30 to 00:30 UTC: 30 listened minutes each side of midnight.
	f.clock = time.Date(2025, 3, 1, 23, 30, 0, 0, time.UTC)
	f.beat(t, 1, 0)
	if _, err := f.c.db.ExecContext(f.ctx,
		`UPDATE listening_sessions SET last_at = ?, listened = 3600`,
		formatSessionTime(f.clock.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	f.clock = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	f.listen(t, 2, f.book, 0) // recent: stays raw

	n, err := f.c.PruneSessions(f.ctx, f.clock.Add(-SessionRetention), time.UTC)
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, err %v; want 1", n, err)
	}
	rows, err := f.c.db.QueryContext(f.ctx, `SELECT day, listened, sessions FROM listening_daily ORDER BY day`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type day struct {
		day      string
		listened float64
		sessions int
	}
	var got []day
	for rows.Next() {
		var d day
		rows.Scan(&d.day, &d.listened, &d.sessions)
		got = append(got, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []day{{"2025-03-01", 1800, 1}, {"2025-03-02", 1800, 0}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("daily = %+v, want %+v", got, want)
	}
	if s := f.sessions(t); len(s) != 1 || s[0].DeviceID != 2 {
		t.Fatalf("the recent session should stay raw: %+v", s)
	}
}

// TestSpreadListeningAcrossFallBack: the repeated hour of a daylight-saving
// fall-back once looped forever (time.Date maps the second 01:xx to the first).
func TestSpreadListeningAcrossFallBack(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz data:", err)
	}
	start := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC) // 01:30 EDT
	end := start.Add(time.Hour)                            // 01:30 EST
	done := make(chan float64)
	go func() {
		var total float64
		calls := 0
		spreadListening(start, end, 3600, ny, func(_ time.Time, s float64) { total += s; calls++ })
		done <- total
	}()
	select {
	case total := <-done:
		if total != 3600 {
			t.Fatalf("spread %v, want 3600", total)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("spreadListening did not finish across the fall-back hour")
	}
	// Spring forward (02:00 EST jumps to 03:00 EDT) still shares everything out.
	var total float64
	spring := time.Date(2026, 3, 8, 6, 30, 0, 0, time.UTC) // 01:30 EST
	spreadListening(spring, spring.Add(time.Hour), 3600, ny, func(_ time.Time, s float64) { total += s })
	if math.Abs(total-3600) > 1e-6 {
		t.Fatalf("spring forward spread %v, want 3600", total)
	}
}

func TestSpreadListeningAcrossHours(t *testing.T) {
	start := time.Date(2026, 1, 1, 10, 45, 0, 0, time.UTC)
	got := map[int]float64{}
	spreadListening(start, start.Add(time.Hour), 600, time.UTC, func(h time.Time, s float64) { got[h.Hour()] += s })
	if got[10] != 150 || got[11] != 450 {
		t.Fatalf("spread = %v, want 150 at 10h and 450 at 11h", got)
	}
	got = map[int]float64{}
	spreadListening(start, start, 30, time.UTC, func(h time.Time, s float64) { got[h.Hour()] += s })
	if got[10] != 30 {
		t.Fatalf("a session with no span lands in its starting hour: %v", got)
	}
}

func TestStreamMarks(t *testing.T) {
	m := NewStreamMarks()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.Note(1, 1, "Author/Book/part2.flac", at)
	if !m.Transcoding(1, 1, "Author/Book", at.Add(time.Minute)) {
		t.Fatal("a file inside the folder marks its book")
	}
	if m.Transcoding(1, 1, "Author/Bo", at) || m.Transcoding(2, 1, "Author/Book", at) || m.Transcoding(1, 2, "Author/Book", at) {
		t.Fatal("marks are per token, per library, per book")
	}
	m.Note(1, 1, "Loose.opus", at)
	if !m.Transcoding(1, 1, "Loose.opus", at) {
		t.Fatal("a single-file book matches its own path")
	}
	if m.Transcoding(1, 1, "Author/Book", at.Add(SessionGap+time.Second)) {
		t.Fatal("marks expire after SessionGap")
	}
	for i := 0; i < maxMarksPerKey+5; i++ {
		m.Note(9, 9, "x", at)
	}
	if n := len(m.marks[streamKey{9, 9}]); n != maxMarksPerKey {
		t.Fatalf("marks per key = %d, want capped at %d", n, maxMarksPerKey)
	}
}

func TestSaveProgressStampsStartAndFinish(t *testing.T) {
	f := newSessionFixture(t)
	save := func(pos float64, finished bool) {
		t.Helper()
		f.clock = f.clock.Add(time.Minute)
		if _, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.book, Position: pos, Duration: 7200, Finished: finished,
			UpdatedAt: f.clock.Format(time.RFC3339)}); err != nil {
			t.Fatal(err)
		}
	}
	get := func() UserProgress {
		t.Helper()
		list, err := f.c.ListUserProgress(f.ctx, f.user)
		if err != nil || len(list) != 1 {
			t.Fatalf("progress list: %v %v", list, err)
		}
		return list[0]
	}
	save(10, false)
	first := get()
	if first.StartedAt == nil || first.FinishedAt != nil || first.Title != "The Book" {
		t.Fatalf("first save: %+v", first)
	}
	save(7200, true)
	done := get()
	if done.FinishedAt == nil || *done.StartedAt != *first.StartedAt {
		t.Fatalf("finishing stamps finished_at and keeps started_at: %+v", done)
	}
	save(7200, true)
	if again := get(); *again.FinishedAt != *done.FinishedAt {
		t.Fatal("a second finished save keeps the first finish")
	}
	save(0, false)
	if restarted := get(); restarted.FinishedAt != nil {
		t.Fatal("restarting a book clears its finish date")
	}
	// A finish replayed from an offline queue is dated by the save, not the sync.
	finishedOffline := f.clock.Add(-48 * time.Hour)
	if _, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.book, Position: 7200, Duration: 7200, Finished: true,
		UpdatedAt: finishedOffline.Add(time.Hour * 49).Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	other := Ref{LibraryID: f.lib, Path: "Other.m4b"}
	if _, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: other, Position: 60, Duration: 60, Finished: true,
		UpdatedAt: finishedOffline.Format("2006-01-02T15:04:05.000Z07:00")}); err != nil {
		t.Fatal(err)
	}
	list, _ := f.c.ListUserProgress(f.ctx, f.user)
	for _, p := range list {
		if p.Path == "Other.m4b" && (p.FinishedAt == nil || *p.FinishedAt != finishedOffline.UTC().Format(time.RFC3339)) {
			t.Fatalf("offline finish dated %v, want %s", p.FinishedAt, finishedOffline.UTC().Format(time.RFC3339))
		}
	}
}

func TestEditProgress(t *testing.T) {
	f := newSessionFixture(t)
	yes, no := true, false
	all := Scope{LibraryID: f.lib, AllowAll: true}
	got, err := f.c.EditProgress(f.ctx, f.user, f.book, ProgressEdit{Finished: &yes}, all)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Finished || got.Position != 7200 || got.FinishedAt == nil || got.StartedAt == nil || got.Version != 1 {
		t.Fatalf("mark finished on a fresh book: %+v", got)
	}

	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	got, err = f.c.EditProgress(f.ctx, f.user, f.book, ProgressEdit{
		StartedAt: OptionalTime{Set: true, Value: &start}, FinishedAt: OptionalTime{Set: true, Value: &end},
	}, all)
	if err != nil || *got.StartedAt != "2026-09-01T12:00:00Z" || *got.FinishedAt != "2026-09-20T12:00:00Z" || got.Version != 2 {
		t.Fatalf("set dates: %+v %v", got, err)
	}

	zero := 0.0
	got, err = f.c.EditProgress(f.ctx, f.user, f.book, ProgressEdit{Finished: &no, Position: &zero}, all)
	if err != nil || got.Finished || got.FinishedAt != nil || got.Position != 0 {
		t.Fatalf("mark unfinished: %+v %v", got, err)
	}

	future := f.clock.Add(48 * time.Hour)
	bad := []ProgressEdit{
		{FinishedAt: OptionalTime{Set: true, Value: &end}},                                                                    // a finish on an unfinished book
		{StartedAt: OptionalTime{Set: true, Value: &future}},                                                                  // in the future
		{Position: func() *float64 { v := 9999.0; return &v }()},                                                              // past the end
		{Finished: &yes, StartedAt: OptionalTime{Set: true, Value: &end}, FinishedAt: OptionalTime{Set: true, Value: &start}}, // finish before start
	}
	for i, e := range bad {
		if _, err := f.c.EditProgress(f.ctx, f.user, f.book, e, all); !errors.Is(err, ErrInvalidProgressEdit) {
			t.Errorf("bad edit %d: err = %v, want ErrInvalidProgressEdit", i, err)
		}
	}

	if _, err := f.c.EditProgress(f.ctx, f.user, Ref{LibraryID: f.lib, Path: "Nowhere"}, ProgressEdit{Finished: &yes}, all); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no row and no book: err = %v, want ErrNotFound", err)
	}

	// An admin's edit beats a stale device save but not a newer one.
	f.clock = f.clock.Add(time.Minute)
	stale, _ := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.book, Position: 50, Duration: 7200,
		UpdatedAt: f.clock.Add(-time.Hour).Format(time.RFC3339)})
	if stale.Position != 0 {
		t.Fatalf("a stale device save must not override the admin's edit, got position %v", stale.Position)
	}
}

// An admin's edit can't start progress on a book the user can't see, but can
// still change progress the user already has (access taken away since).
func TestEditProgressNeedsTheUsersAccess(t *testing.T) {
	f := newSessionFixture(t)
	yes := true
	elsewhere := Scope{LibraryID: f.lib, Paths: []string{"Other"}}
	if _, err := f.c.EditProgress(f.ctx, f.user, f.book, ProgressEdit{Finished: &yes}, elsewhere); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("new row outside the user's scope: err = %v, want ErrNoAccess", err)
	}
	if rows, _ := f.c.ListUserProgress(f.ctx, f.user); len(rows) != 0 {
		t.Fatalf("a refused edit wrote progress: %+v", rows)
	}

	within := Scope{LibraryID: f.lib, Paths: []string{f.book.Path}}
	if _, err := f.c.EditProgress(f.ctx, f.user, f.book, ProgressEdit{Finished: &yes}, within); err != nil {
		t.Fatalf("new row inside the user's scope: %v", err)
	}
	no := false
	got, err := f.c.EditProgress(f.ctx, f.user, f.book, ProgressEdit{Finished: &no}, elsewhere)
	if err != nil || got.Finished {
		t.Fatalf("existing row after access was taken away: %+v %v", got, err)
	}
}
