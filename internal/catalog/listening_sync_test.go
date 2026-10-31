package catalog

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// syncFixture is a catalog with a controllable clock, one library, one user and
// a two-file book at "Author/Book" (17 h: 01.mp3 and 02.mp3, 8.5 h each).
type syncFixture struct {
	c     *Catalog
	ctx   context.Context
	clock time.Time
	lib   int64
	user  int64
	book  Ref
}

const syncBookLen = 61200.0

func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	c, ctx := newTestCatalog(t)
	f := &syncFixture{c: c, ctx: ctx, clock: time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)}
	c.now = func() time.Time { return f.clock }
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if err != nil {
		t.Fatal(err)
	}
	f.lib, f.user = lib.ID, seedUser(t, c, ctx)
	f.book = Ref{LibraryID: lib.ID, Path: "Author/Book"}
	f.upsertBook(t, "Author/Book/01.mp3", "Author/Book/02.mp3")
	return f
}

// upsertBook (re)indexes the fixture's book with its files in this order.
func (f *syncFixture) upsertBook(t *testing.T, files ...string) {
	t.Helper()
	b := &Book{LibraryID: f.lib, RelPath: f.book.Path, IsFolder: true, Title: "The Book", Duration: syncBookLen,
		Codec: "mp3", Size: 1000, AddedAt: "2026-09-20T00:00:00Z"}
	for i, p := range files {
		b.Files = append(b.Files, BookFile{RelPath: p, Seq: i, Duration: syncBookLen / float64(len(files)), Format: "mp3"})
	}
	if _, err := f.c.UpsertBook(f.ctx, b); err != nil {
		t.Fatal(err)
	}
}

// target is the book's sync target, as the handler resolves an exact book path.
func (f *syncFixture) target(t *testing.T, ref Ref) SyncTarget {
	t.Helper()
	book, err := f.c.SyncBookAt(f.ctx, ref.LibraryID, ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	return SyncTarget{Key: ref, Exact: book != nil, Book: book}
}

// batch is device's batch of events, sent at the fixture's clock (no shift).
func (f *syncFixture) batch(t *testing.T, device string, events ...SyncEvent) SyncBatch {
	t.Helper()
	sent := f.clock
	b := SyncBatch{UserID: f.user, DeviceID: device, TokenID: tokenOf(device), DeviceName: device + "'s phone",
		Client: Client{App: "AudioSilo", Version: "2.6.0", Platform: "ios"}, SentAt: &sent, Events: events,
		Targets: map[Ref]SyncTarget{}}
	for _, ev := range events {
		ref := Ref{LibraryID: ev.LibraryID, Path: ev.Path}
		if _, ok := b.Targets[ref]; !ok && ev.LibraryID > 0 {
			b.Targets[ref] = f.target(t, ref)
		}
	}
	return b
}

func tokenOf(device string) int64 {
	var n int64
	for _, r := range device {
		n = n*31 + int64(r)
	}
	return n%1000 + 1
}

func (f *syncFixture) sync(t *testing.T, b SyncBatch) *SyncAnswer {
	t.Helper()
	a, err := f.c.SyncListening(f.ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Results) != len(b.Events) {
		t.Fatalf("%d results for %d events", len(a.Results), len(b.Events))
	}
	return a
}

// syncOne posts one event from device and returns its result.
func (f *syncFixture) syncOne(t *testing.T, device string, ev SyncEvent) SyncResult {
	t.Helper()
	return f.sync(t, f.batch(t, device, ev)).Results[0]
}

func (f *syncFixture) progress(t *testing.T) *Progress {
	t.Helper()
	p, err := f.c.GetProgress(f.ctx, f.user, f.book)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var syncSeq atomic.Int64

// nextSyncID is a fresh event id (the tests run in parallel).
func nextSyncID(prefix string) string { return prefix + strconv.FormatInt(syncSeq.Add(1), 10) }

// listen is a span on the fixture's book from from to to, ending at the
// fixture's clock minus ago and lasting its played length at 1x.
func (f *syncFixture) listen(start string, from, to float64, ago time.Duration, base *int64) SyncEvent {
	ended := f.clock.Add(-ago)
	started := ended.Add(-time.Duration((to - from) * float64(time.Second)))
	if to < from {
		started = ended.Add(-15 * time.Second)
	}
	return SyncEvent{ID: nextSyncID("ev-"), Kind: EventListen, LibraryID: f.book.LibraryID, Path: f.book.Path,
		FromPos: from, ToPos: to, Duration: syncBookLen, PlaybackSpeed: 1, Start: start,
		StartedAt: started.Format(time.RFC3339Nano), EndedAt: ended.Format(time.RFC3339Nano), BaseVersion: base}
}

func (f *syncFixture) finishEvent(kind string, ago time.Duration) SyncEvent {
	return SyncEvent{ID: nextSyncID("ev-"), Kind: kind, LibraryID: f.book.LibraryID, Path: f.book.Path,
		At: f.clock.Add(-ago).Format(time.RFC3339Nano)}
}

func wantResult(t *testing.T, got SyncResult, status, reason string) {
	t.Helper()
	if got.Status != status || got.Reason != reason {
		t.Fatalf("result %+v, want %s %s", got, status, reason)
	}
}

func (f *syncFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.c.db.QueryRowContext(f.ctx, query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Failure 1 and 6: an offline span from a stale base never moves the place back
// silently. Against a further row it is diverged, the row kept and the span's
// place offered and persisted; further than the row it is kept and the row's
// place offered. The row's own device carrying on from an older base keeps the
// offer; a device that saw it and carries on clears it. Control: moving back
// on purpose from the version seen applies.
func TestSyncDiverged(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	wantResult(t, f.syncOne(t, "phone", f.listen("resume", 17000, 17500, 70*time.Minute, base(0))), statusApplied, "")
	wantResult(t, f.syncOne(t, "phone", f.listen("continue", 17500, 18000, time.Hour, base(1))), statusApplied, "")
	// The watch had version 1, went offline and listened 4:00:00 to 4:40:00.
	res := f.syncOne(t, "watch", f.listen("resume", 14400, 16800, 30*time.Minute, base(1)))
	if res.Status != statusDiverged || res.Kept != "stored" || res.OtherPlace == nil ||
		res.OtherPlace.Position != 16800 || res.OtherPlace.DeviceID != "watch" || res.OtherPlace.DeviceName != "watch's phone" {
		t.Fatalf("stale span: %+v", res)
	}
	p := f.progress(t)
	if p.Position != 18000 || p.DeviceID != "phone" || p.Version != 2 || p.OtherPlace == nil ||
		p.OtherPlace.Position != 16800 || p.OtherPlace.DeviceID != "watch" || p.DeviceName != "phone's phone" {
		t.Fatalf("row after the stale span: %+v other %+v", p, p.OtherPlace)
	}
	// The row's own device carrying on from an older base (1, before the offer's
	// version 3) applies and keeps the offer.
	wantResult(t, f.syncOne(t, "phone", f.listen("continue", 18000, 18100, 20*time.Minute, base(1))), statusApplied, "")
	if p = f.progress(t); p.Position != 18100 || p.OtherPlace == nil {
		t.Fatalf("offer dropped: %+v", p)
	}
	// A device further on, from a stale base: kept event, the row's place offered.
	res = f.syncOne(t, "tablet", f.listen("resume", 19000, 20000, 10*time.Minute, base(1)))
	if res.Status != statusDiverged || res.Kept != "event" || res.OtherPlace == nil ||
		res.OtherPlace.DeviceID != "phone" || res.OtherPlace.Position != 18100 {
		t.Fatalf("further span: %+v", res)
	}
	p = f.progress(t)
	if p.Position != 20000 || p.DeviceID != "tablet" || p.OtherPlace == nil || p.OtherPlace.DeviceID != "phone" {
		t.Fatalf("row after the further span: %+v other %+v", p, p.OtherPlace)
	}
	// Control: the tablet, which saw this version, moves back on purpose: applies
	// and, its base at the offer's version, clears the offer.
	wantResult(t, f.syncOne(t, "tablet", f.listen("moved_back", 10000, 10100, 5*time.Minute, base(p.Version))), statusApplied, "")
	p = f.progress(t)
	if p.Position != 10100 || p.OtherPlace != nil {
		t.Fatalf("moved back: %+v other %+v", p, p.OtherPlace)
	}
}

// The row's own device carrying on from the version it acknowledged before an
// offer existed (its own write) keeps the offer, so its answer carries it; its
// next span, on the version that answer had, clears it.
func TestSyncOfferKeptByOlderBase(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	f.syncOne(t, "phone", f.listen("resume", 17000, 18000, time.Hour, base(0)))
	f.syncOne(t, "phone", f.listen("continue", 18000, 18100, 50*time.Minute, base(1)))
	f.syncOne(t, "watch", f.listen("resume", 14400, 16800, 40*time.Minute, base(1)))
	p := f.progress(t)
	if p.OtherPlace == nil || p.Version != 2 {
		t.Fatalf("setup: %+v", p)
	}
	// The phone acknowledged version 2 (its own write) before the watch's offer:
	// its next span on base 2 applies and keeps the offer, which its answer carries.
	a := f.sync(t, f.batch(t, "phone", f.listen("continue", 18100, 18200, 30*time.Minute, base(2))))
	wantResult(t, a.Results[0], statusApplied, "")
	if p = f.progress(t); p.Position != 18200 || p.OtherPlace == nil || a.Progress[0].OtherPlace == nil {
		t.Fatalf("offer cleared by the version acknowledged before it: %+v", p)
	}
	// ... and one on version 3 (the answer that carried it) clears it.
	wantResult(t, f.syncOne(t, "phone", f.listen("continue", 18200, 18300, 20*time.Minute, base(3))), statusApplied, "")
	if p = f.progress(t); p.OtherPlace != nil {
		t.Fatalf("offer kept by a base that saw it: %+v", p.OtherPlace)
	}
}

// Failure 2: a cold-start resume near zero, and its continue chain, never move a
// row further on back; a restart from the version seen applies; a finished row
// is not protected.
func TestSyncResumeBehind(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	f.syncOne(t, "phone", f.listen("resume", 200, 300, time.Hour, base(0)))
	res := f.syncOne(t, "phone", f.listen("resume", 0, 15, 30*time.Minute, base(1)))
	wantResult(t, res, statusNotApplied, reasonResumeBehind)
	if res.OtherPlace == nil || res.OtherPlace.Position != 15 {
		t.Fatalf("resume_behind carries the event's place: %+v", res)
	}
	wantResult(t, f.syncOne(t, "phone", f.listen("continue", 15, 30, 29*time.Minute, base(1))), statusNotApplied, reasonResumeBehind)
	// A cold start on another device (base 0, it had no place yet): the same, and
	// no offer is kept for it.
	wantResult(t, f.syncOne(t, "watch", f.listen("resume", 0, 15, 28*time.Minute, base(0))), statusNotApplied, reasonResumeBehind)
	wantResult(t, f.syncOne(t, "watch", f.listen("continue", 15, 30, 27*time.Minute, nil)), statusNotApplied, reasonResumeBehind)
	if p := f.progress(t); p.Position != 300 || p.OtherPlace != nil {
		t.Fatalf("moved back: %+v", p)
	}
	wantResult(t, f.syncOne(t, "phone", f.listen("restart", 0, 15, 20*time.Minute, base(1))), statusApplied, "")
	if p := f.progress(t); p.Position != 15 {
		t.Fatalf("restart: %+v", p)
	}
	// Finished: a resume near zero may move it (the restart of a finished book).
	wantResult(t, f.syncOne(t, "phone", f.finishEvent(EventFinish, 10*time.Minute)), statusApplied, "")
	p := f.progress(t)
	wantResult(t, f.syncOne(t, "phone", f.listen("resume", 0, 15, 5*time.Minute, base(p.Version))), statusApplied, "")
	if p = f.progress(t); p.Position != 15 || !p.Finished {
		t.Fatalf("resume on a finished row: %+v", p)
	}
}

// Failure 3: a late stale event of the same lineage is "older"; a replayed id
// is "duplicate", and so is an id repeated in one batch.
func TestSyncStaleAndDuplicates(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	first := f.listen("resume", 100, 200, 10*time.Minute, nil)
	wantResult(t, f.syncOne(t, "phone", first), statusApplied, "")
	wantResult(t, f.syncOne(t, "phone", f.listen("continue", 150, 180, 20*time.Minute, nil)), statusNotApplied, reasonOlder)
	wantResult(t, f.syncOne(t, "phone", first), statusDuplicate, "")
	again := f.listen("continue", 200, 215, 9*time.Minute, nil)
	a := f.sync(t, f.batch(t, "phone", again, again))
	wantResult(t, a.Results[0], statusApplied, "")
	wantResult(t, a.Results[1], statusDuplicate, "")
	if n := f.count(t, `SELECT COUNT(*) FROM listening_events WHERE event_id = ?`, again.ID); n != 1 {
		t.Fatalf("%d rows for one id", n)
	}
}

// Failure 4: a device clock 3 h behind lands at the true time by sent_at, and
// by clock_offset; a span corrected into the future is pulled back to now,
// keeping its length.
func TestSyncClockCorrection(t *testing.T) {
	t.Parallel()
	for _, viaOffset := range []bool{false, true} {
		f := newSyncFixture(t)
		skew := 3 * time.Hour
		ev := f.listen("resume", 0, 600, 0, base(0))
		// The device's clock reads 3 h behind: the span ended "now - 3 h" by it.
		ended := f.clock.Add(-skew - 10*time.Minute)
		ev.StartedAt, ev.EndedAt = ended.Add(-10*time.Minute).Format(time.RFC3339), ended.Format(time.RFC3339)
		b := f.batch(t, "watch", ev)
		if viaOffset {
			off := skew.Seconds()
			b.SentAt, b.ClockOffset = nil, &off
		} else {
			sent := f.clock.Add(-skew)
			b.SentAt = &sent
		}
		wantResult(t, f.sync(t, b).Results[0], statusApplied, "")
		wantEnd := formatSessionTime(f.clock.Add(-10 * time.Minute))
		h, err := f.c.ListHistory(f.ctx, f.user, f.book, 10)
		if err != nil || len(h) != 1 || h[0].EndedAt != wantEnd ||
			h[0].StartedAt != formatSessionTime(f.clock.Add(-20*time.Minute)) {
			t.Fatalf("offset %v: history %+v, %v, want end %s", viaOffset, h, err, wantEnd)
		}
		s, _, err := f.c.ListSessions(f.ctx, SessionFilter{})
		if err != nil || len(s) != 1 || s[0].LastAt != wantEnd || math.Abs(s[0].Listened-600) > 0.01 {
			t.Fatalf("offset %v: sessions %+v, %v", viaOffset, s, err)
		}
		if p := f.progress(t); p.UpdatedAt != wantEnd {
			t.Fatalf("offset %v: updated_at %s", viaOffset, p.UpdatedAt)
		}
	}
	// A device 1 h ahead whose sent_at is honest about its own clock, but a span
	// stamped later still: pulled back to now, 10 minutes long.
	f := newSyncFixture(t)
	ev := f.listen("resume", 0, 600, 0, base(0))
	ev.StartedAt = f.clock.Add(50 * time.Minute).Format(time.RFC3339)
	ev.EndedAt = f.clock.Add(time.Hour).Format(time.RFC3339)
	f.sync(t, f.batch(t, "watch", ev))
	h, _ := f.c.ListHistory(f.ctx, f.user, f.book, 10)
	if len(h) != 1 || h[0].EndedAt != formatSessionTime(f.clock) ||
		h[0].StartedAt != formatSessionTime(f.clock.Add(-10*time.Minute)) {
		t.Fatalf("future span: %+v", h)
	}
	// Past what a Duration holds: a sentinel sent_at (year 1, its shift saturating
	// alike on every platform) and a span dated in the year 9999 both land at now,
	// 10 minutes long, never inverted.
	for _, tc := range []struct{ sent, started, ended string }{
		{"0001-01-01T00:00:00Z", f.clock.Add(-20 * time.Minute).Format(time.RFC3339),
			f.clock.Add(-10 * time.Minute).Format(time.RFC3339)},
		{f.clock.Format(time.RFC3339), "9999-12-31T23:49:59Z", "9999-12-31T23:59:59Z"},
	} {
		g := newSyncFixture(t)
		ev := g.listen("resume", 0, 600, 0, base(0))
		ev.StartedAt, ev.EndedAt = tc.started, tc.ended
		b := g.batch(t, "watch", ev)
		sent, err := time.Parse(time.RFC3339, tc.sent)
		if err != nil {
			t.Fatal(err)
		}
		b.SentAt = &sent
		wantResult(t, g.sync(t, b).Results[0], statusApplied, "")
		h, _ := g.c.ListHistory(g.ctx, g.user, g.book, 10)
		if len(h) != 1 || h[0].EndedAt != formatSessionTime(g.clock) ||
			h[0].StartedAt != formatSessionTime(g.clock.Add(-10*time.Minute)) {
			t.Fatalf("sent %s, span %s-%s: history %+v", tc.sent, tc.started, tc.ended, h)
		}
	}
}

// Failure 5: a batch posted twice is all duplicate and counted once; a batch
// whose transaction fails stores nothing, and its retry applies once.
func TestSyncReplayAndFailedBatch(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	b := f.batch(t, "phone", f.listen("resume", 0, 600, 20*time.Minute, base(0)),
		f.listen("continue", 600, 1200, 10*time.Minute, base(0)))
	for _, r := range f.sync(t, b).Results {
		wantResult(t, r, statusApplied, "")
	}
	listened := func() float64 {
		var v float64
		if err := f.c.db.QueryRowContext(f.ctx, `SELECT COALESCE(SUM(listened), 0) FROM listening_sessions`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	before, hist := listened(), f.count(t, `SELECT COUNT(*) FROM listening_history`)
	if math.Abs(before-1200) > 0.01 || hist != 1 {
		t.Fatalf("first post: listened %v, %d history rows", before, hist)
	}
	pBefore := f.progress(t)
	for _, r := range f.sync(t, b).Results {
		wantResult(t, r, statusDuplicate, "")
	}
	if listened() != before || f.count(t, `SELECT COUNT(*) FROM listening_history`) != hist ||
		f.progress(t).Version != pBefore.Version {
		t.Fatal("a replay counted again")
	}

	// A failing transaction (history refuses its insert) stores nothing.
	if _, err := f.c.db.ExecContext(f.ctx, `CREATE TRIGGER fail_history BEFORE INSERT ON listening_history
		BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	failing := f.batch(t, "phone", f.listen("jump", 5000, 5600, 5*time.Minute, base(pBefore.Version)))
	if _, err := f.c.SyncListening(f.ctx, failing); err == nil {
		t.Fatal("the failing batch answered")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM listening_events WHERE event_id = ?`, failing.Events[0].ID); n != 0 ||
		f.progress(t).Position != pBefore.Position {
		t.Fatalf("a failed batch stored its event (%d) or place", n)
	}
	if _, err := f.c.db.ExecContext(f.ctx, `DROP TRIGGER fail_history`); err != nil {
		t.Fatal(err)
	}
	wantResult(t, f.sync(t, failing).Results[0], statusApplied, "")
	wantResult(t, f.sync(t, failing).Results[0], statusDuplicate, "")
}

// Two first spans in one POST (both on base 0) chain rather than diverge from
// each other, and a run of continue spans makes one session and one history row.
func TestSyncInBatchChaining(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	a := f.sync(t, f.batch(t, "phone",
		f.listen("resume", 0, 15, 45*time.Second, base(0)),
		f.listen("continue", 15, 30, 30*time.Second, base(0)),
		f.listen("continue", 30, 45, 15*time.Second, base(0))))
	for _, r := range a.Results {
		wantResult(t, r, statusApplied, "")
	}
	if len(a.Progress) != 1 || a.Progress[0].Position != 45 || a.Progress[0].Version != 3 {
		t.Fatalf("answer's progress: %+v", a.Progress)
	}
	if a.ServerTime != formatSessionTime(f.clock) {
		t.Fatalf("server_time %s", a.ServerTime)
	}
	h, _ := f.c.ListHistory(f.ctx, f.user, f.book, 10)
	if len(h) != 1 || h[0].From != 0 || h[0].To != 45 {
		t.Fatalf("history: %+v", h)
	}
	s, _, _ := f.c.ListSessions(f.ctx, SessionFilter{})
	if len(s) != 1 || math.Abs(s[0].Listened-45) > 0.01 || s[0].State != SessionPlaying {
		t.Fatalf("sessions: %+v", s)
	}
	// A seek ahead inside a span is not listening (the clip).
	f.syncOne(t, "phone", f.listen("jump", 1000, 1000, 0, base(3)))
	ev := f.listen("continue", 1000, 4000, 0, base(4))
	ev.StartedAt = f.clock.Add(-15 * time.Second).Format(time.RFC3339)
	f.syncOne(t, "phone", ev)
	if s, _, _ = f.c.ListSessions(f.ctx, SessionFilter{}); math.Abs(s[0].Listened-60) > 0.01 {
		t.Fatalf("a seek ahead counted: %+v", s)
	}
}

// Failure 7: a finish holds against a later listen from a device that never saw
// it; an older unfinish is "older", a newer one clears it; a legacy PUT with
// finished false, newer, still clears it.
func TestSyncFinishOrdering(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	f.syncOne(t, "phone", f.listen("resume", 0, 1000, 3*time.Hour, base(0)))
	// The phone plays on (version 2), then the watch's offline finish from an hour
	// before arrives.
	f.syncOne(t, "phone", f.listen("continue", 1000, 2000, 2*time.Hour, base(1)))
	wantResult(t, f.syncOne(t, "watch", f.finishEvent(EventFinish, 150*time.Minute)), statusApplied, "")
	p := f.progress(t)
	if !p.Finished || p.Position != syncBookLen || p.FinishedAt == "" {
		t.Fatalf("finish: %+v", p)
	}
	// The phone, which never saw it, plays on from version 2.
	wantResult(t, f.syncOne(t, "phone", f.listen("continue", 2000, 3000, time.Hour, base(2))), statusDiverged, "")
	if p = f.progress(t); !p.Finished {
		t.Fatalf("a listen unfinished the book: %+v", p)
	}
	// A listen that applies (the row's device, the version it saw) still never
	// touches the finish.
	wantResult(t, f.syncOne(t, "watch", f.listen("moved_back", 3000, 3100, 50*time.Minute, base(p.Version))), statusApplied, "")
	if p = f.progress(t); !p.Finished || p.Position != 3100 {
		t.Fatalf("applied listen: %+v", p)
	}
	wantResult(t, f.syncOne(t, "phone", f.finishEvent(EventUnfinish, 3*time.Hour)), statusNotApplied, reasonOlder)
	wantResult(t, f.syncOne(t, "phone", f.finishEvent(EventUnfinish, 40*time.Minute)), statusApplied, "")
	if p = f.progress(t); p.Finished || p.FinishedAt != "" || p.Position != 3100 {
		t.Fatalf("unfinish: %+v", p)
	}
	wantResult(t, f.syncOne(t, "phone", f.finishEvent(EventFinish, 30*time.Minute)), statusApplied, "")
	if p = f.progress(t); !p.Finished {
		t.Fatalf("finish again: %+v", p)
	}
	// Legacy: a newer PUT with finished false still clears it (last-write-wins).
	if _, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.book, Position: 10, Duration: syncBookLen,
		DeviceID: "old-app", UpdatedAt: f.clock.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if p = f.progress(t); p.Finished {
		t.Fatalf("a newer PUT did not unfinish: %+v", p)
	}
	// ... and an older finish event after it is "older".
	wantResult(t, f.syncOne(t, "watch", f.finishEvent(EventFinish, time.Minute)), statusNotApplied, reasonOlder)
}

// Failure 9: another edition's length is rejected and nothing is stored; a file
// place on a book whose file order changed lands in the same file (fe#188); a
// file not in the book falls back to the whole-book seconds.
func TestSyncFingerprintAndFilePlaces(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	ev := f.listen("resume", 0, 100, time.Minute, base(0))
	ev.Duration = syncBookLen + 120
	a := f.sync(t, f.batch(t, "phone", ev))
	wantResult(t, a.Results[0], statusRejected, reasonDurationMismatch)
	if n := f.count(t, `SELECT COUNT(*) FROM listening_events`); n != 0 || f.progress(t) != nil || len(a.Progress) != 0 {
		t.Fatalf("a rejected event stored (%d) or touched progress", n)
	}
	// Within 10 s is the same edition.
	ev = f.listen("resume", 0, 100, time.Minute, base(0))
	ev.Duration = syncBookLen + 9
	wantResult(t, f.syncOne(t, "phone", ev), statusApplied, "")

	// The device's download is in the old order (01, 02); the server's is now 02, 01.
	f.upsertBook(t, "Author/Book/02.mp3", "Author/Book/01.mp3")
	ev = f.listen("continue", 100, 200, 30*time.Second, base(1))
	from, to, file := 100.0, 200.0, "Author/Book/01.mp3"
	ev.FromFile, ev.FromFilePos, ev.ToFile, ev.ToFilePos = &file, &from, &file, &to
	wantResult(t, f.syncOne(t, "phone", ev), statusApplied, "")
	if p := f.progress(t); p.Position != syncBookLen/2+200 {
		t.Fatalf("file place: %v, want %v", p.Position, syncBookLen/2+200)
	}
	var stored float64
	if err := f.c.db.QueryRowContext(f.ctx, `SELECT to_pos FROM listening_events WHERE event_id = ?`, ev.ID).Scan(&stored); err != nil ||
		stored != syncBookLen/2+200 {
		t.Fatalf("stored to_pos %v, %v", stored, err)
	}
	// A place past the file's end is clamped to it; a file not in the book is the sent seconds.
	p := f.progress(t)
	ev = f.listen("jump", 300, 400, 0, base(p.Version))
	gone, far := "Author/Book/99.mp3", 1e9
	ev.FromFile, ev.FromFilePos, ev.ToFile, ev.ToFilePos = &gone, &far, &file, &far
	wantResult(t, f.syncOne(t, "phone", ev), statusApplied, "")
	if p := f.progress(t); p.Position != syncBookLen {
		t.Fatalf("clamped: %v", p.Position)
	}
	if err := f.c.db.QueryRowContext(f.ctx, `SELECT from_pos FROM listening_events WHERE event_id = ?`, ev.ID).Scan(&stored); err != nil ||
		stored != 300 {
		t.Fatalf("fallback from_pos %v, %v", stored, err)
	}
}

// Failure 10: an event 89 days old applies, 91 days is too old; the prune keeps
// 120 days.
func TestSyncAgeAndPrune(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	wantResult(t, f.syncOne(t, "phone", f.listen("resume", 0, 100, 89*24*time.Hour, base(0))), statusApplied, "")
	wantResult(t, f.syncOne(t, "phone", f.listen("continue", 100, 200, 91*24*time.Hour, base(0))), statusRejected, reasonTooOld)
	if _, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.book, Position: 300, DeviceID: "old",
		UpdatedAt: f.clock.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM listening_events`); n != 2 {
		t.Fatalf("%d events", n)
	}
	f.clock = f.clock.Add(31*24*time.Hour + time.Hour) // the 89-day event is now 120 d + 1 h old
	n, err := f.c.PruneListeningEvents(f.ctx, f.clock.Add(-SyncRetention))
	if err != nil || n != 1 || f.count(t, `SELECT COUNT(*) FROM listening_events WHERE kind = 'put'`) != 1 {
		t.Fatalf("prune: %d, %v", n, err)
	}
}

// Every field check is the event's "invalid" (or unknown_kind), never the
// batch's failure, and the other events still apply.
func TestSyncEventChecks(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	good := f.listen("resume", 0, 100, time.Minute, base(0))
	file, neg := "Author/Book/01.mp3", -1.0
	cases := map[string]func(*SyncEvent){
		"bad id":         func(e *SyncEvent) { e.ID = "has space" },
		"long id":        func(e *SyncEvent) { e.ID = string(make([]byte, 65)) },
		"no library":     func(e *SyncEvent) { e.LibraryID = 0 },
		"no path":        func(e *SyncEvent) { e.Path = "" },
		"negative pos":   func(e *SyncEvent) { e.FromPos = -1 },
		"bad time":       func(e *SyncEvent) { e.EndedAt = "yesterday" },
		"ends first":     func(e *SyncEvent) { e.StartedAt, e.EndedAt = e.EndedAt, e.StartedAt },
		"too long":       func(e *SyncEvent) { e.StartedAt = f.clock.Add(-25 * time.Hour).Format(time.RFC3339) },
		"bad start":      func(e *SyncEvent) { e.Start = "teleport" },
		"half file pair": func(e *SyncEvent) { e.ToFile = &file },
		"negative file":  func(e *SyncEvent) { e.ToFile, e.ToFilePos = &file, &neg },
		"fast":           func(e *SyncEvent) { e.PlaybackSpeed = 4.5 },
		"negative base":  func(e *SyncEvent) { e.BaseVersion = base(-1) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			ev := good
			ev.ID = nextSyncID("case-")
			mutate(&ev)
			a := f.sync(t, f.batch(t, "phone", ev))
			wantResult(t, a.Results[0], statusRejected, SyncInvalid)
		})
	}
	unknown := good
	unknown.Kind = "skip"
	wantResult(t, f.syncOne(t, "phone", unknown), statusRejected, reasonUnknownKind)
	finish := f.finishEvent(EventFinish, 0)
	finish.At = "soon"
	wantResult(t, f.syncOne(t, "phone", finish), statusRejected, SyncInvalid)
	// Undecodable events (DecodeSyncEvents) and an empty batch.
	evs := DecodeSyncEvents([]json.RawMessage{
		json.RawMessage(`{"id":"x1","kind":"listen","surprise":1}`),
		json.RawMessage(`{"id":"x2","from_pos":"far"}`),
		json.RawMessage(`[]`),
	})
	a := f.sync(t, f.batch(t, "phone", append(evs, good)...))
	wantResult(t, a.Results[0], statusRejected, SyncInvalid)
	wantResult(t, a.Results[1], statusRejected, SyncInvalid)
	wantResult(t, a.Results[2], statusRejected, SyncInvalid)
	wantResult(t, a.Results[3], statusApplied, "")
	if a.Results[0].ID != "x1" || a.Results[1].ID != "x2" || a.Results[2].ID != "" {
		t.Fatalf("ids: %+v", a.Results)
	}
	empty := f.sync(t, f.batch(t, "phone"))
	if empty.ServerTime == "" || len(empty.Results) != 0 || empty.Progress == nil {
		t.Fatalf("empty batch: %+v", empty)
	}
	// A listen whose to_pos is null or missing (a player's NaN goes out as null)
	// would read as 0:00: invalid. The same event with both positions decodes.
	wire := func(id string, edit func(map[string]any)) json.RawMessage {
		ev := good
		ev.ID = id
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		edit(m)
		if raw, err = json.Marshal(m); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	evs = DecodeSyncEvents([]json.RawMessage{
		wire(nextSyncID("pos-"), func(m map[string]any) { m["to_pos"] = nil }),
		wire(nextSyncID("pos-"), func(m map[string]any) { delete(m, "from_pos") }),
		wire(nextSyncID("pos-"), func(map[string]any) {}),
	})
	a = f.sync(t, f.batch(t, "phone", evs...))
	wantResult(t, a.Results[0], statusRejected, SyncInvalid)
	wantResult(t, a.Results[1], statusRejected, SyncInvalid)
	if a.Results[2].Status == statusRejected {
		t.Fatalf("a listen with both positions: %+v", a.Results[2])
	}
}

// The legacy PUT is logged as a "put" event (applied, or not applied when
// stale), stores the token's device name and finish_changed_at, and clears no
// offer; an edit clears the offer and records the finish-state change.
func TestSaveProgressRecordsPutEvent(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	now := f.clock.Format(time.RFC3339)
	out, err := f.c.SaveProgressFrom(f.ctx, f.user, 7, Progress{Ref: f.book, Position: 50, Duration: syncBookLen,
		DeviceID: "old-app", DeviceName: "Old iPhone", UpdatedAt: now, Finished: true})
	if err != nil || out.DeviceName != "Old iPhone" {
		t.Fatalf("save: %+v, %v", out, err)
	}
	var kind, status, device string
	var token int64
	if err := f.c.db.QueryRowContext(f.ctx, `SELECT kind, status, device_id, token_id FROM listening_events`).
		Scan(&kind, &status, &device, &token); err != nil || kind != "put" || status != "applied" || device != "old-app" || token != 7 {
		t.Fatalf("put event: %s %s %s %d %v", kind, status, device, token, err)
	}
	// A stale PUT echoes the row and is logged as not applied.
	if _, err := f.c.SaveProgressFrom(f.ctx, f.user, 7, Progress{Ref: f.book, Position: 10, DeviceID: "old-app",
		UpdatedAt: f.clock.Add(-time.Hour).Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM listening_events WHERE status = 'not_applied' AND reason = 'older'`); n != 1 {
		t.Fatalf("%d stale put events", n)
	}
	// The finish is a finish-state change: an older sync unfinish is "older".
	wantResult(t, f.syncOne(t, "watch", f.finishEvent(EventUnfinish, time.Minute)), statusNotApplied, reasonOlder)

	// An offer survives a PUT, and an edit clears it.
	f.clock = f.clock.Add(time.Minute)
	f.syncOne(t, "phone", f.listen("resume", 20000, 30000, 0, base(0)))
	f.syncOne(t, "watch", f.listen("resume", 0, 600, 0, base(0)))
	if p := f.progress(t); p.OtherPlace == nil {
		t.Fatalf("no offer: %+v", p)
	}
	f.clock = f.clock.Add(time.Minute)
	saved, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.book, Position: 30100, DeviceID: "old-app",
		UpdatedAt: f.clock.Format(time.RFC3339)})
	if err != nil || saved.OtherPlace == nil || f.progress(t).OtherPlace == nil {
		t.Fatalf("a PUT cleared the offer: %+v, %v", saved, err)
	}
	finished := true
	if _, err := f.c.EditProgress(f.ctx, f.user, f.book, ProgressEdit{Finished: &finished}, Scope{AllowAll: true}); err != nil {
		t.Fatal(err)
	}
	p := f.progress(t)
	if p.OtherPlace != nil || !p.Finished {
		t.Fatalf("edit: %+v", p)
	}
	var changed string
	if err := f.c.db.QueryRowContext(f.ctx, `SELECT finish_changed_at FROM progress`).Scan(&changed); err != nil ||
		changed != formatSessionTime(f.clock) {
		t.Fatalf("finish_changed_at %q, %v", changed, err)
	}
}

// A move carries the events onto the new path and drops the offer, keeping the
// device name and the finish-state change; a join puts the events on the
// joined timeline.
func TestSyncStateMovesWithTheBook(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	f.syncOne(t, "phone", f.listen("resume", 20000, 30000, 10*time.Minute, base(0)))
	f.syncOne(t, "phone", f.listen("continue", 30000, 30060, 9*time.Minute, base(1)))
	f.syncOne(t, "phone", f.finishEvent(EventFinish, 7*time.Minute))
	f.syncOne(t, "watch", f.listen("resume", 0, 600, 5*time.Minute, base(1)))
	if p := f.progress(t); p.OtherPlace == nil {
		t.Fatal("no offer to drop")
	}
	if err := f.c.MoveDurableState(f.ctx, f.lib, "Author/Book", "Author/Moved"); err != nil {
		t.Fatal(err)
	}
	p, err := f.c.GetProgress(f.ctx, f.user, Ref{LibraryID: f.lib, Path: "Author/Moved"})
	if err != nil || p == nil || p.OtherPlace != nil || p.DeviceName != "phone's phone" || !p.Finished {
		t.Fatalf("moved row: %+v, %v", p, err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM listening_events WHERE rel_path = 'Author/Moved'`); n != 4 {
		t.Fatalf("%d events moved", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM progress WHERE finish_changed_at IS NOT NULL`); n != 1 {
		t.Fatal("finish_changed_at not carried")
	}
	// A join: the disc's events land after the earlier discs.
	disc := Ref{LibraryID: f.lib, Path: "Joined/Disc 2"}
	b := f.batch(t, "phone", SyncEvent{ID: "disc-1", Kind: EventListen, LibraryID: f.lib, Path: disc.Path,
		FromPos: 10, ToPos: 20, Start: "resume", StartedAt: f.clock.Add(-10 * time.Second).Format(time.RFC3339),
		EndedAt: f.clock.Format(time.RFC3339)})
	f.sync(t, b)
	if err := f.c.JoinDurableState(f.ctx, f.lib, "Joined", []JoinPart{{Path: disc.Path, Offset: 1000, Duration: 500, Last: true}}, 1500); err != nil {
		t.Fatal(err)
	}
	var from, to float64
	if err := f.c.db.QueryRowContext(f.ctx, `SELECT from_pos, to_pos FROM listening_events WHERE event_id = 'disc-1'
		AND rel_path = 'Joined'`).Scan(&from, &to); err != nil || from != 1010 || to != 1020 {
		t.Fatalf("joined event %v %v %v", from, to, err)
	}
}

// Only a live span (ending within livePlaying of now) is marked transcoded from
// the token's stream marks; an old span is never live.
func TestSyncTranscodedOnlyLive(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		ago  time.Duration
		want bool
	}{{0, true}, {2 * time.Hour, false}} {
		f := newSyncFixture(t)
		b := f.batch(t, "phone", f.listen("resume", 0, 30, tc.ago, nil))
		target := b.Targets[f.book]
		target.Transcoding = true
		b.Targets[f.book] = target
		f.sync(t, b)
		s, _, err := f.c.ListSessions(f.ctx, SessionFilter{})
		if err != nil || len(s) != 1 || s[0].Transcoded != tc.want || s[0].DeviceName != "phone's phone" ||
			s[0].Client == nil || s[0].Client.Version != "2.6.0" {
			t.Fatalf("%v ago: %+v, %v", tc.ago, s, err)
		}
	}
}

// The offer is cleared only by a base the device sent (never the in-batch
// chaining's), follows its device's continue chain behind the row, and is
// dropped by a finish.
func TestSyncOfferFollowsAndClears(t *testing.T) {
	t.Parallel()
	// In one batch the watch's second span chains on its first (diverged, kept),
	// but the watch never saw the phone's place the first one offered: it stays.
	f := newSyncFixture(t)
	f.syncOne(t, "phone", f.listen("resume", 3000, 3600, 2*time.Hour, base(0)))
	a := f.sync(t, f.batch(t, "watch",
		f.listen("resume", 7200, 7215, 30*time.Second, base(0)),
		f.listen("continue", 7215, 7230, 15*time.Second, base(0))))
	if a.Results[0].Status != statusDiverged || a.Results[0].Kept != "event" || a.Results[1].Status != statusApplied {
		t.Fatalf("results: %+v", a.Results)
	}
	if p := f.progress(t); p.Position != 7230 || p.OtherPlace == nil || p.OtherPlace.DeviceID != "phone" {
		t.Fatalf("a chained span cleared the offer: %+v", p)
	}

	// A jump behind the row on a device that hadn't seen it (base 0) is offered;
	// its continue chain (resume_behind) takes the offer to where it got to, and a
	// cold start elsewhere in the book leaves it.
	g := newSyncFixture(t)
	g.syncOne(t, "phone", g.listen("resume", 17000, 18000, 3*time.Hour, base(0)))
	if res := g.syncOne(t, "watch", g.listen("jump", 14400, 14415, 70*time.Minute, base(0))); res.Status != statusDiverged ||
		res.Kept != "stored" {
		t.Fatalf("jump: %+v", res)
	}
	wantResult(t, g.syncOne(t, "watch", g.listen("continue", 14415, 16800, 30*time.Minute, base(0))),
		statusNotApplied, reasonResumeBehind)
	if p := g.progress(t); p.Position != 18000 || p.OtherPlace == nil || p.OtherPlace.Position != 16800 {
		t.Fatalf("offer after the continue chain: %+v %+v", p, p.OtherPlace)
	}
	wantResult(t, g.syncOne(t, "watch", g.listen("resume", 0, 15, 20*time.Minute, base(0))),
		statusNotApplied, reasonResumeBehind)
	if p := g.progress(t); p.OtherPlace == nil || p.OtherPlace.Position != 16800 {
		t.Fatalf("a cold start moved the offer: %+v", p.OtherPlace)
	}

	// A finish drops it (the listener chose the end), as Mark finished does.
	wantResult(t, g.syncOne(t, "phone", g.finishEvent(EventFinish, 10*time.Minute)), statusApplied, "")
	if p := g.progress(t); !p.Finished || p.OtherPlace != nil {
		t.Fatalf("the finished row kept the offer: %+v %+v", p, p.OtherPlace)
	}
}

// Restarting a book another device finished: the unfinish takes the row for
// this device, so the restart span after it applies (in one batch or two)
// rather than diverging from the finishing device and leaving the place at the
// end. An unfinish of a book that isn't finished hands nothing over.
func TestSyncRestartAfterAnotherDeviceFinished(t *testing.T) {
	t.Parallel()
	for _, split := range []bool{false, true} {
		f := newSyncFixture(t)
		f.syncOne(t, "phone", f.listen("resume", 0, 1000, 3*time.Hour, base(0)))
		wantResult(t, f.syncOne(t, "watch", f.finishEvent(EventFinish, time.Hour)), statusApplied, "")
		seen := f.progress(t).Version // the phone resumes from the finished row
		unfinish := f.finishEvent(EventUnfinish, 10*time.Minute)
		restart := f.listen("restart", 0, 15, 10*time.Minute-15*time.Second, base(seen))
		var results []SyncResult
		if split {
			results = []SyncResult{f.syncOne(t, "phone", unfinish), f.syncOne(t, "phone", restart)}
		} else {
			results = f.sync(t, f.batch(t, "phone", unfinish, restart)).Results
		}
		for _, r := range results {
			wantResult(t, r, statusApplied, "")
		}
		if p := f.progress(t); p.Finished || p.Position != 15 || p.DeviceID != "phone" {
			t.Fatalf("split %v: restarted row %+v", split, p)
		}
	}
	g := newSyncFixture(t)
	g.syncOne(t, "phone", g.listen("resume", 0, 1000, time.Hour, base(0)))
	wantResult(t, g.syncOne(t, "watch", g.finishEvent(EventUnfinish, 30*time.Minute)), statusApplied, "")
	if p := g.progress(t); p.DeviceID != "phone" {
		t.Fatalf("an unfinish of an unfinished book took the row: %+v", p)
	}
	wantResult(t, g.syncOne(t, "phone", g.listen("continue", 1000, 1100, 20*time.Minute, base(1))), statusApplied, "")
}

// A batch sent again after its answer was lost: its stored first span is a
// duplicate, and the device's later base-0 spans chain on the row it wrote, as
// they did the first time, so its move back applies.
func TestSyncReplayChainsOnOwnRow(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	first := f.listen("resume", 0, 3600, 2*time.Hour, base(0))
	wantResult(t, f.syncOne(t, "tablet", first), statusApplied, "") // stored; its answer lost
	a := f.sync(t, f.batch(t, "tablet", first,
		f.listen("moved_back", 600, 615, 50*time.Minute, base(0)),
		f.listen("continue", 615, 1200, 30*time.Minute, base(0))))
	wantResult(t, a.Results[0], statusDuplicate, "")
	wantResult(t, a.Results[1], statusApplied, "")
	wantResult(t, a.Results[2], statusApplied, "")
	if p := f.progress(t); p.Position != 1200 || p.Version != 3 {
		t.Fatalf("row after the replay: %+v", p)
	}
}

// A late backlog of contiguous spans joins into one history row even when
// another device's row on the book is newer.
func TestSyncHistoryJoinsBehindNewerRow(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	f.syncOne(t, "web", f.listen("resume", 9000, 10800, 30*time.Minute, base(0))) // 07:00-07:30
	f.sync(t, f.batch(t, "phone",
		f.listen("resume", 100, 1000, 165*time.Minute, base(0)),     // 05:00-05:15
		f.listen("continue", 1000, 1900, 150*time.Minute, base(0)),  // 05:15-05:30
		f.listen("continue", 1900, 2800, 135*time.Minute, base(0)))) // 05:30-05:45
	h, err := f.c.ListHistory(f.ctx, f.user, f.book, 10)
	if err != nil || len(h) != 2 || h[1].From != 100 || h[1].To != 2800 ||
		h[1].EndedAt != formatSessionTime(f.clock.Add(-135*time.Minute)) {
		t.Fatalf("history: %+v, %v", h, err)
	}
}

// A span synced late opens a session dated when it happened without taking the
// token's live session's place; spans from before a household switch, synced
// after it, continue each other as one session.
func TestSyncSessionsOfLateSpans(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	at := func(h, m, s int) time.Time { return time.Date(2026, 10, 25, h, m, s, 0, time.UTC) }
	save := func(when time.Time, pos float64) {
		t.Helper()
		if err := f.c.RecordHeartbeat(f.ctx, Heartbeat{UserID: f.user, Ref: f.book, TokenID: tokenOf("phone"),
			DeviceName: "phone's phone", Position: pos, Duration: syncBookLen, Speed: 1, At: when}); err != nil {
			t.Fatal(err)
		}
	}
	// The phone's live saves (PUT), 07:45 to 08:00, then a span of its own from
	// 07:00 to 07:30 synced late, then its next save.
	save(at(7, 45, 0), 5000)
	save(at(7, 52, 30), 5450)
	save(at(8, 0, 0), 5900)
	f.syncOne(t, "phone", f.listen("resume", 1000, 2800, 30*time.Minute, nil))
	save(at(8, 0, 15), 5915)
	s, _, err := f.c.ListSessions(f.ctx, SessionFilter{})
	if err != nil || len(s) != 2 || s[0].LastAt != formatSessionTime(at(8, 0, 15)) ||
		math.Abs(s[0].Listened-915) > 0.01 || math.Abs(s[1].Listened-1800) > 0.01 {
		t.Fatalf("sessions: %+v, %v", s, err)
	}

	g := newSyncFixture(t)
	g.clock = at(7, 30, 0)
	if err := g.c.SetHouseholdSharing(g.ctx, g.user, true); err != nil {
		t.Fatal(err)
	}
	g.clock = at(8, 0, 0)
	g.sync(t, g.batch(t, "tablet",
		g.listen("resume", 100, 1000, 105*time.Minute, nil),    // 06:00-06:15
		g.listen("continue", 1000, 1900, 90*time.Minute, nil),  // 06:15-06:30
		g.listen("continue", 1900, 2800, 75*time.Minute, nil))) // 06:30-06:45
	if s, _, err = g.c.ListSessions(g.ctx, SessionFilter{}); err != nil || len(s) != 1 ||
		math.Abs(s[0].Listened-2700) > 0.01 {
		t.Fatalf("pre-switch spans: %+v, %v", s, err)
	}
}

// A PUT from a clock a few minutes fast (inside plausibleUpdatedAt's skew)
// dates its finish change no later than the server's now, so a Sync v2
// unfinish sent just after it applies.
func TestSavePutFinishChangeNotAhead(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	if _, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.book, Position: syncBookLen, Duration: syncBookLen,
		Finished: true, DeviceID: "old-app", UpdatedAt: f.clock.Add(4 * time.Minute).Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(time.Minute)
	wantResult(t, f.syncOne(t, "phone", f.finishEvent(EventUnfinish, 0)), statusApplied, "")
	if p := f.progress(t); p.Finished {
		t.Fatalf("still finished: %+v", p)
	}
}
