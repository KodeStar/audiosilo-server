package catalog

import (
	"encoding/json"
	"testing"
	"time"
)

// chainBacklog is the watch's offline backlog for the server#289 tests: six
// five-minute spans ending 60 to 35 minutes ago, every one on base seen (the
// version it last saw: 0 none, else the phone's first), with a move back in
// the middle (s4) that only a chained span applies.
func (f *syncFixture) chainBacklog(seen int64) []SyncEvent {
	return []SyncEvent{
		f.listen("resume", 7000, 7300, 60*time.Minute, base(seen)),
		f.listen("continue", 7300, 7600, 55*time.Minute, base(seen)),
		f.listen("continue", 7600, 7900, 50*time.Minute, base(seen)),
		f.listen("moved_back", 7100, 7400, 45*time.Minute, base(seen)),
		f.listen("continue", 7400, 7700, 40*time.Minute, base(seen)),
		f.listen("continue", 7700, 8000, 35*time.Minute, base(seen)),
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

// sameAcrossPOSTs posts the backlog events makes (on a fixture setup makes) in
// one POST from device, then split every way listed, and fails unless each
// split leaves exactly what the one POST did; it returns the one POST's
// fixture and results.
func sameAcrossPOSTs(t *testing.T, setup func() *syncFixture, device string, events func(*syncFixture) []SyncEvent,
	splits ...[]int) (*syncFixture, []SyncResult) {
	t.Helper()
	one := setup()
	backlog := events(one)
	results := one.postSplit(t, device, backlog, len(backlog))
	want := one.chainOutcome(t, results)
	for _, sizes := range splits {
		f := setup()
		if got := f.chainOutcome(t, f.postSplit(t, device, events(f), sizes...)); got != want {
			t.Fatalf("POSTs of %v:\n%s\nwant (one POST):\n%s", sizes, got, want)
		}
	}
	return one, results
}

// A finish, an unfinish and the restart after them, each in its own POST after
// the diverged span, chain as they do in one (TestSyncFinishKeepsSpanChain).
func TestSyncChainFinishAcrossPOSTs(t *testing.T) {
	t.Parallel()
	one, results := sameAcrossPOSTs(t, func() *syncFixture {
		f := newSyncFixture(t)
		wantResult(t, f.syncOne(t, "phone", f.listen("resume", 5000, 6000, 10*time.Minute, base(0))), statusApplied, "")
		return f
	}, "watch", func(f *syncFixture) []SyncEvent {
		return []SyncEvent{
			f.listen("resume", 60000, 61200, 40*time.Minute, base(0)),
			f.finishEvent(EventFinish, 39*time.Minute+30*time.Second),
			f.finishEvent(EventUnfinish, 39*time.Minute),
			f.listen("restart", 0, 480, 30*time.Minute, base(0)),
		}
	}, []int{1, 1, 1, 1}, []int{1, 3}, []int{2, 2}, []int{3, 1})
	if results[0].Status != statusDiverged {
		t.Fatalf("first span: %+v", results[0])
	}
	if p := one.progress(t); p.Position != 480 || p.Finished {
		t.Fatalf("row = %v finished=%v, want the restart's 480, unfinished", p.Position, p.Finished)
	}
}

// A finish or unfinish with no span of the device's before it chains too: the
// unfinish that takes another device's finished row, and a finish that makes
// the row, are the device's chain in the next POST, so the restart after them
// (from the unfinish's time, as the player sends it) resolves on the row there
// as in one POST, never diverged from the device's own row.
func TestSyncChainFromAFinishAcrossPOSTs(t *testing.T) {
	t.Parallel()
	restart := func(f *syncFixture) []SyncEvent {
		return []SyncEvent{
			f.finishEvent(EventUnfinish, 30*time.Minute),
			f.listen("restart", 0, 480, 22*time.Minute, base(0)),
			f.listen("continue", 480, 780, 17*time.Minute, base(0)),
		}
	}
	one, _ := sameAcrossPOSTs(t, func() *syncFixture {
		f := newSyncFixture(t)
		wantResult(t, f.syncOne(t, "phone", f.listen("resume", 5000, 6000, 3*time.Hour, base(0))), statusApplied, "")
		wantResult(t, f.syncOne(t, "phone", f.finishEvent(EventFinish, 2*time.Hour)), statusApplied, "")
		return f
	}, "watch", restart, []int{1, 2}, []int{2, 1}, []int{1, 1, 1})
	if p := one.progress(t); p.Position != 780 || p.Finished || p.DeviceID != "watch" {
		t.Fatalf("taken row: %+v, want the watch's 780, unfinished", p)
	}

	one, _ = sameAcrossPOSTs(t, func() *syncFixture { return newSyncFixture(t) }, "watch", func(f *syncFixture) []SyncEvent {
		return []SyncEvent{
			f.finishEvent(EventFinish, 40*time.Minute),
			f.finishEvent(EventUnfinish, 39*time.Minute),
			f.listen("restart", 0, 480, 31*time.Minute, base(0)),
		}
	}, []int{1, 2}, []int{2, 1}, []int{1, 1, 1})
	if p := one.progress(t); p.Position != 480 || p.Finished {
		t.Fatalf("made row: %+v, want the restart's 480, unfinished", p)
	}
}

// An unfinish of another device's unfinished row takes nothing, so the
// device's base-0 restart after it diverges from that row, in one POST as in
// two: the further place (the phone's) is kept and the restart's offered,
// never replaced unseen.
func TestSyncUnfinishOfAnotherDevicesRowTakesNothing(t *testing.T) {
	t.Parallel()
	one, results := sameAcrossPOSTs(t, func() *syncFixture {
		f := newSyncFixture(t)
		wantResult(t, f.syncOne(t, "phone", f.finishEvent(EventFinish, 3*time.Hour)), statusApplied, "")
		wantResult(t, f.syncOne(t, "phone", f.finishEvent(EventUnfinish, 150*time.Minute)), statusApplied, "")
		wantResult(t, f.syncOne(t, "phone", f.listen("jump", 49700, 50000, time.Hour, base(2))), statusApplied, "")
		return f
	}, "watch", func(f *syncFixture) []SyncEvent {
		return []SyncEvent{
			f.finishEvent(EventUnfinish, 30*time.Minute),
			f.listen("restart", 0, 480, 22*time.Minute, base(0)),
		}
	}, []int{1, 1})
	wantResult(t, results[0], statusApplied, "")
	if r := results[1]; r.Status != statusDiverged || r.Kept != "stored" {
		t.Fatalf("restart: %+v, want diverged with the phone's place kept", r)
	}
	if p := one.progress(t); p.Position != 50000 || p.DeviceID != "phone" || p.OtherPlace == nil ||
		p.OtherPlace.Position != 480 || p.OtherPlace.DeviceID != "watch" {
		t.Fatalf("row: %+v, want the phone's 50000 with the watch's 480 offered", p)
	}
}

// A finish or unfinish that comes back "older" decides nothing about the
// device's spans after it in the POST: each is judged on its own against the
// chain of the earlier POST (a jump elsewhere still diverges; a move back
// that starts where the chain ended still applies).
func TestSyncChainIgnoresAnOlderFinish(t *testing.T) {
	t.Parallel()
	setup := func() *syncFixture {
		f := newSyncFixture(t)
		// The phone's place 5000, its finish state last changed 30 minutes ago.
		for i, finished := range []bool{true, false} {
			if _, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.book, Position: 5000, Duration: syncBookLen,
				Finished: finished, DeviceID: "phone",
				UpdatedAt: f.clock.Add(-time.Duration(31-i) * time.Minute).Format(time.RFC3339)}); err != nil {
				t.Fatal(err)
			}
		}
		// The watch's first POST: a span further on, diverged, its place kept.
		if r := f.syncOne(t, "watch", f.listen("resume", 7000, 7300, 50*time.Minute, base(0))); r.Kept != "event" {
			t.Fatalf("first span: %+v", r)
		}
		return f
	}
	for _, older := range []bool{false, true} {
		var unfinish []SyncEvent
		f := setup()
		if older {
			unfinish = append(unfinish, f.finishEvent(EventUnfinish, 50*time.Minute-10*time.Second))
		}
		r := f.sync(t, f.batch(t, "watch", append(unfinish, f.listen("jump", 100, 400, 35*time.Minute, base(0)))...)).Results
		if older {
			wantResult(t, r[0], statusNotApplied, reasonOlder)
		}
		if jump := r[len(r)-1]; jump.Status != statusDiverged || jump.Kept != "stored" || f.progress(t).Position != 7300 {
			t.Fatalf("older unfinish %v: jump %+v, row at %v; want diverged, 7300 kept", older, jump, f.progress(t).Position)
		}

		f = setup()
		unfinish = nil
		if older {
			unfinish = append(unfinish, f.finishEvent(EventUnfinish, 48*time.Minute))
		}
		r = f.sync(t, f.batch(t, "watch", append(unfinish, f.listen("moved_back", 6800, 7070, 45*time.Minute, base(0)))...)).Results
		if back := r[len(r)-1]; back.Status != statusApplied || f.progress(t).Position != 7070 {
			t.Fatalf("older unfinish %v: move back %+v, row at %v; want applied at 7070", older, back, f.progress(t).Position)
		}
	}
}

// A finish or unfinish never dates the chain earlier than what it compares
// against already: a late batch's unfinish keeps the end of the device's
// later span (its restart is older than the continuation delivered first, as
// without a chain; one POST, applied in time order, can't arrive out of
// order), and one older than a save elsewhere since starts no chain (the
// restart is older than that save, as without a chain).
func TestSyncFinishNeverDatesTheChainBack(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	wantResult(t, f.syncOne(t, "phone", f.finishEvent(EventFinish, time.Hour)), statusApplied, "")
	wantResult(t, f.syncOne(t, "watch", f.listen("continue", 480, 780, 30*time.Minute, base(1))), statusApplied, "")
	r := f.sync(t, f.batch(t, "watch",
		f.finishEvent(EventUnfinish, 40*time.Minute),
		f.listen("restart", 0, 480, 35*time.Minute, base(1)))).Results
	wantResult(t, r[0], statusApplied, "")
	wantResult(t, r[1], statusNotApplied, reasonOlder)
	if p := f.progress(t); p.Position != 780 || p.Finished || p.DeviceID != "watch" {
		t.Fatalf("late unfinish: row %+v, want the watch's 780, unfinished", p)
	}

	f = newSyncFixture(t)
	wantResult(t, f.syncOne(t, "phone", f.finishEvent(EventFinish, time.Hour)), statusApplied, "")
	// An old app's save on the finished row, newer than the watch's unfinish.
	if _, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.book, Position: 30000, Duration: syncBookLen,
		Finished: true, DeviceID: "tablet", UpdatedAt: f.clock.Add(-20 * time.Minute).Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	r = f.sync(t, f.batch(t, "watch",
		f.finishEvent(EventUnfinish, 40*time.Minute),
		f.listen("restart", 0, 480, 35*time.Minute, base(0)))).Results
	wantResult(t, r[0], statusApplied, "")
	wantResult(t, r[1], statusNotApplied, reasonOlder)
	if p := f.progress(t); p.Position != 30000 {
		t.Fatalf("unfinish under a newer save: row at %v, want the tablet's 30000", p.Position)
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

// A replay seeds the batch's chain only while the row is still as the device's
// chain left it: an edit since (which keeps the row's device) ends it, so the
// base-0 span after the replay diverges from the edited place rather than
// replacing it unseen.
func TestSyncReplayAfterAnEditDoesNotChain(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	first := f.listen("resume", 7000, 7300, 60*time.Minute, base(0))
	wantResult(t, f.syncOne(t, "watch", first), statusApplied, "")
	pos := 50000.0
	f.clock = f.clock.Add(-30 * time.Minute)
	if _, err := f.c.EditProgress(f.ctx, f.user, f.book, ProgressEdit{Position: &pos}, Scope{AllowAll: true}); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(30 * time.Minute)
	r := f.sync(t, f.batch(t, "watch", first, f.listen("moved_back", 100, 400, 2*time.Minute, base(0)))).Results
	wantResult(t, r[0], statusDuplicate, "")
	if r[1].Status != statusDiverged || r[1].Kept != "stored" {
		t.Fatalf("span after the replay: %+v, want diverged with the edit kept", r[1])
	}
	if p := f.progress(t); p.Position != 50000 {
		t.Fatalf("row at %v, want the edit's 50000", p.Position)
	}
}

// A replay of a span that diverged and kept the stored place wrote nothing, so
// it seeds no chain: the base-0 span after it diverges as it would without the
// replay, never moving the device's own further place back unseen.
func TestSyncReplayOfAKeptStoredSpanDoesNotChain(t *testing.T) {
	t.Parallel()
	for _, replay := range []bool{false, true} {
		f := newSyncFixture(t)
		wantResult(t, f.syncOne(t, "watch", f.listen("resume", 49700, 50000, 3*time.Hour, base(0))), statusApplied, "")
		jump := f.listen("jump", 100, 400, 20*time.Minute, base(0))
		if r := f.syncOne(t, "watch", jump); r.Status != statusDiverged || r.Kept != "stored" {
			t.Fatalf("jump: %+v", r)
		}
		events := []SyncEvent{f.listen("moved_back", 50, 350, 10*time.Minute, base(0))}
		if replay {
			events = append([]SyncEvent{jump}, events...)
		}
		r := f.sync(t, f.batch(t, "watch", events...)).Results
		if back := r[len(r)-1]; back.Status != statusDiverged || back.Kept != "stored" || f.progress(t).Position != 50000 {
			t.Fatalf("replay %v: %+v, row at %v; want diverged, 50000 kept", replay, back, f.progress(t).Position)
		}
	}
}

// A replay of a late finish (dated before the chain's end, which it keeps)
// still seeds the chain: the finish wrote the row's version, so the base-0 span
// after it applies as it did in the same POST.
func TestSyncReplayOfALateFinishChains(t *testing.T) {
	t.Parallel()
	for _, replay := range []bool{false, true} {
		f := newSyncFixture(t)
		wantResult(t, f.syncOne(t, "watch", f.listen("resume", 0, 3600, 10*time.Minute, base(0))), statusApplied, "")
		fin := f.finishEvent(EventFinish, 30*time.Minute)
		if replay {
			wantResult(t, f.syncOne(t, "watch", fin), statusApplied, "") // stored; its answer lost
		}
		r := f.sync(t, f.batch(t, "watch", fin, f.listen("moved_back", 100, 400, time.Minute, base(0)))).Results
		if back := r[1]; back.Status != statusApplied || f.progress(t).Position != 400 {
			t.Fatalf("replay %v: %+v, row at %v; want applied at 400", replay, back, f.progress(t).Position)
		}
	}
}
