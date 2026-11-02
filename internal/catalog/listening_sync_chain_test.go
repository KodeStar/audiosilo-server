package catalog

import (
	"encoding/json"
	"testing"
	"time"
)

// chainBacklog is the watch's offline backlog for the server#289 tests: six
// five-minute spans ending 60 to 35 minutes ago, every one on base (the
// version it last saw: 0 none, else the phone's first), with a move back in
// the middle (s4) that only a chained span applies.
func (f *syncFixture) chainBacklog(base int64) []SyncEvent {
	b := func() *int64 { v := base; return &v }
	return []SyncEvent{
		f.listen("resume", 7000, 7300, 60*time.Minute, b()),
		f.listen("continue", 7300, 7600, 55*time.Minute, b()),
		f.listen("continue", 7600, 7900, 50*time.Minute, b()),
		f.listen("moved_back", 7100, 7400, 45*time.Minute, b()),
		f.listen("continue", 7400, 7700, 40*time.Minute, b()),
		f.listen("continue", 7700, 8000, 35*time.Minute, b()),
	}
}

// chainFixture is a fixture whose phone saved 4300 two hours ago (version 1,
// which the watch saw) and moved on to 5000 ten minutes ago (version 2), a
// place behind the watch's backlog but later than it.
func chainFixture(t *testing.T) *syncFixture {
	t.Helper()
	f := newSyncFixture(t)
	wantResult(t, f.syncOne(t, "phone", f.listen("resume", 4000, 4300, 2*time.Hour, base(0))), statusApplied, "")
	wantResult(t, f.syncOne(t, "phone", f.listen("jump", 4700, 5000, 10*time.Minute, base(1))), statusApplied, "")
	return f
}

// chainOutcome is everything a backlog's sync leaves: each event's result in
// backlog order, the progress row, the sessions and the history.
func (f *syncFixture) chainOutcome(t *testing.T, results []SyncResult) string {
	t.Helper()
	p := f.progress(t)
	sessions, _, err := f.c.ListSessions(f.ctx, SessionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	history, err := f.c.ListHistory(f.ctx, f.user, f.book, 50)
	if err != nil {
		t.Fatal(err)
	}
	type session struct {
		Device, Started, Last string
		From, To, Listened    float64
	}
	type span struct {
		From, To      float64
		Started, Last string
	}
	out := struct {
		Results  []SyncResult
		Progress *Progress
		Sessions []session
		History  []span
	}{Progress: p}
	for _, r := range results {
		r.ID = "" // each fixture's backlog has ids of its own
		out.Results = append(out.Results, r)
	}
	for _, s := range sessions {
		out.Sessions = append(out.Sessions, session{s.DeviceName, s.StartedAt, s.LastAt, s.StartPos, s.EndPos, s.Listened})
	}
	for _, h := range history {
		out.History = append(out.History, span{h.From, h.To, h.StartedAt, h.EndedAt})
	}
	b, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// postSplit posts events from device in POSTs of the given sizes and returns
// every event's result in order.
func (f *syncFixture) postSplit(t *testing.T, device string, events []SyncEvent, sizes ...int) []SyncResult {
	t.Helper()
	var results []SyncResult
	for _, n := range sizes {
		results = append(results, f.sync(t, f.batch(t, device, events[:n]...)).Results...)
		events = events[n:]
	}
	if len(events) != 0 {
		t.Fatalf("%d events left unsent", len(events))
	}
	return results
}

// server#289: a backlog split across POSTs (the player halving a batch on 413,
// an outbox longer than one batch), after its first span's diverged apply kept
// the phone's later row time, resolves exactly as the one POST does: the same
// results, place, offer, sessions and history, never "older"; whether its
// spans carry the version the watch saw or base 0 (in-batch chaining's case,
// where the move back resolves on the row its own earlier span wrote).
func TestSyncChainAcrossPOSTs(t *testing.T) {
	t.Parallel()
	for _, seen := range []int64{1, 0} {
		one := chainFixture(t)
		want := one.chainOutcome(t, one.postSplit(t, "watch", one.chainBacklog(seen), 6))
		p := one.progress(t)
		if p.Position != 8000 || p.DeviceID != "watch" || p.OtherPlace == nil || p.OtherPlace.Position != 5000 ||
			p.UpdatedAt != formatSessionTime(one.clock.Add(-10*time.Minute)) {
			t.Fatalf("base %d, one POST: row %+v", seen, p)
		}
		for _, sizes := range [][]int{{3, 3}, {2, 2, 2}, {4, 2}, {1, 1, 1, 1, 1, 1}} {
			f := chainFixture(t)
			if got := f.chainOutcome(t, f.postSplit(t, "watch", f.chainBacklog(seen), sizes...)); got != want {
				t.Fatalf("base %d, POSTs of %v:\n%s\nwant (one POST):\n%s", seen, sizes, got, want)
			}
		}
	}
}

// The chain carried across POSTs keeps every older answer: a span from
// another device with no base that ended before the row's time, and one of the
// watch's own that ended before its last applied span, are still "older"; a
// base-0 span of the watch's that doesn't continue its last span (signed out
// and back in) still diverges from its own place further on; and a replayed
// POST is all duplicates, changing nothing.
func TestSyncChainKeepsOlderAndReplays(t *testing.T) {
	t.Parallel()
	f := chainFixture(t)
	backlog := f.chainBacklog(0)
	first := f.sync(t, f.batch(t, "watch", backlog[:3]...))
	f.sync(t, f.batch(t, "watch", backlog[3:]...))
	before := f.chainOutcome(t, nil)
	events := f.count(t, `SELECT COUNT(*) FROM listening_events`)

	// Replays: the first POST again, then the whole backlog in one.
	again := f.sync(t, f.batch(t, "watch", backlog[:3]...))
	all := f.sync(t, f.batch(t, "watch", backlog...))
	for _, r := range append(again.Results, all.Results...) {
		wantResult(t, r, statusDuplicate, "")
	}
	if got := f.chainOutcome(t, nil); got != before {
		t.Fatalf("a replay changed the state:\n%s\nwant:\n%s", got, before)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM listening_events`); n != events {
		t.Fatalf("%d events stored after the replays, want %d", n, events)
	}
	if first.Results[0].Status != statusDiverged {
		t.Fatalf("first span: %+v", first.Results[0])
	}

	// Another device's span with no base, ended before the row's time: older.
	wantResult(t, f.syncOne(t, "tablet", f.listen("resume", 9000, 9300, 70*time.Minute, nil)),
		statusNotApplied, reasonOlder)
	// The watch's own span with no base, ended before its last applied span
	// (35 minutes ago) though after its first: older.
	wantResult(t, f.syncOne(t, "watch", f.listen("jump", 9000, 9300, 42*time.Minute, nil)),
		statusNotApplied, reasonOlder)
	// A base-0 jump of the watch's long after its last span, from elsewhere:
	// diverged from its own place, which is kept.
	r := f.syncOne(t, "watch", f.listen("jump", 100, 400, 5*time.Minute, base(0)))
	if r.Status != statusDiverged || r.Kept != "stored" {
		t.Fatalf("an unrelated base-0 span: %+v", r)
	}
	if p := f.progress(t); p.Position != 8000 {
		t.Fatalf("row moved to %v", p.Position)
	}
}

// A finish, an unfinish and the restart after them, each in its own POST after
// the diverged span, chain as they do in one (TestSyncFinishKeepsSpanChain).
func TestSyncChainFinishAcrossPOSTs(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	wantResult(t, f.syncOne(t, "phone", f.listen("resume", 5000, 6000, 10*time.Minute, base(0))), statusApplied, "")
	if r := f.syncOne(t, "watch", f.listen("resume", 60000, 61200, 40*time.Minute, base(0))); r.Status != statusDiverged {
		t.Fatalf("first span: %+v", r)
	}
	wantResult(t, f.syncOne(t, "watch", f.finishEvent(EventFinish, 39*time.Minute+30*time.Second)), statusApplied, "")
	wantResult(t, f.syncOne(t, "watch", f.finishEvent(EventUnfinish, 39*time.Minute)), statusApplied, "")
	wantResult(t, f.syncOne(t, "watch", f.listen("restart", 0, 480, 30*time.Minute, base(0))), statusApplied, "")
	if p := f.progress(t); p.Position != 480 || p.Finished {
		t.Fatalf("row = %v finished=%v, want the restart's 480, unfinished", p.Position, p.Finished)
	}
}

// The chain lives only while the row is as the device's span left it: a row
// with none recorded (written before 0062) or written since by anyone else (a
// PUT, here the watch's own old app) answers as before the chain was kept.
func TestSyncChainOnlyWhileRecorded(t *testing.T) {
	t.Parallel()
	for _, breakChain := range []string{"unrecorded", "put"} {
		f := chainFixture(t)
		backlog := f.chainBacklog(1)
		f.sync(t, f.batch(t, "watch", backlog[:3]...))
		switch breakChain {
		case "unrecorded":
			if _, err := f.c.db.ExecContext(f.ctx, `UPDATE progress SET span_at = NULL, span_version = NULL`); err != nil {
				t.Fatal(err)
			}
		case "put":
			p := f.progress(t)
			if _, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.book, Position: p.Position, Duration: syncBookLen,
				DeviceID: "watch", Version: p.Version, UpdatedAt: f.clock.Add(-5 * time.Minute).Format(time.RFC3339)}); err != nil {
				t.Fatal(err)
			}
		}
		r := f.sync(t, f.batch(t, "watch", backlog[3])).Results[0]
		wantResult(t, r, statusNotApplied, reasonOlder)
		if p := f.progress(t); p.Position != 7900 {
			t.Fatalf("%s: row at %v", breakChain, p.Position)
		}
	}
}
