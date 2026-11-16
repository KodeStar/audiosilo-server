package catalog

import (
	"testing"
	"time"
)

// Stay wins (row 9c-2, 0080): a listener who answers another device's offer
// with Stay sends a span with start "stay"; it applies over the other device's
// writes since the offer (S1), and holds the row against that device's spans
// made without seeing it (S2).

// stayFixture is the race a Stay answers: the phone listened to 18000 (version
// 1); the watch, which had version 1, carried on (versions 2 to 4), its last
// span ending watchLast before the clock. The phone was offered the watch's
// place at version 2, so its Stay's base is 2.
func stayFixture(t *testing.T, watchLast time.Duration) *syncFixture {
	t.Helper()
	f := newSyncFixture(t)
	wantResult(t, f.syncOne(t, "phone", f.listen("resume", 17000, 18000, 60*time.Minute, base(0))), statusApplied, "")
	wantResult(t, f.syncOne(t, "watch", f.listen("resume", 18000, 18600, 40*time.Minute, base(1))), statusApplied, "")
	wantResult(t, f.syncOne(t, "watch", f.listen("continue", 18600, 19200, 20*time.Minute, base(2))), statusApplied, "")
	wantResult(t, f.syncOne(t, "watch", f.listen("continue", 19200, 19800, watchLast, base(3))), statusApplied, "")
	if p := f.progress(t); p.Position != 19800 || p.DeviceID != "watch" || p.Version != 4 {
		t.Fatalf("setup: %+v", p)
	}
	return f
}

// stay is a Stay at pos (a paused player's: a span of no length) ending ago
// before the clock.
func (f *syncFixture) stay(pos float64, ago time.Duration, b *int64) SyncEvent {
	return f.listen(startStay, pos, pos, ago, b)
}

// phoneStays is the phone's Stay on the watch's offer (base 2) that wins
// (stay_version 5).
func (f *syncFixture) phoneStays(t *testing.T) {
	t.Helper()
	wantResult(t, f.syncOne(t, "phone", f.stay(18000, 4*time.Minute, base(2))), statusApplied, "")
}

// stayVersion is the row's stay_version, 0 for none.
func (f *syncFixture) stayVersion(t *testing.T, ref Ref) int64 {
	t.Helper()
	return int64(f.count(t, `SELECT COALESCE(stay_version, 0) FROM progress
		WHERE user_id = ? AND library_id = ? AND rel_path = ?`, f.user, ref.LibraryID, ref.Path))
}

// wantKept checks a span resolved as a plain jump against the watch's further
// row: diverged, the row kept.
func wantKept(t *testing.T, res SyncResult) {
	t.Helper()
	if res.Status != statusDiverged || res.Kept != "stored" {
		t.Fatalf("result %+v, want diverged with the stored row kept", res)
	}
}

// wantAppliedOrKept checks a span applied, or else resolved as wantKept.
func wantAppliedOrKept(t *testing.T, res SyncResult, applied bool) {
	t.Helper()
	if applied {
		wantResult(t, res, statusApplied, "")
	} else {
		wantKept(t, res)
	}
}

// S1: the Stay applies over the three versions the watch wrote since the
// offer, drops the offered place (a tablet's) and holds the row; a plain jump
// from the same base keeps the watch's further place (today's rule).
func TestSyncStayWinsOverTheOtherDevicesWrites(t *testing.T) {
	t.Parallel()
	for _, start := range []string{startStay, "jump"} {
		f := stayFixture(t, 5*time.Minute)
		wantKept(t, f.syncOne(t, "tablet", f.listen("resume", 10000, 10500, 3*time.Minute, base(1))))
		if p := f.progress(t); p.OtherPlace == nil || p.OtherPlace.DeviceID != "tablet" {
			t.Fatalf("setup offer: %+v", p.OtherPlace)
		}
		res := f.syncOne(t, "phone", f.listen(start, 18000, 18000, 0, base(2)))
		p := f.progress(t)
		if start == "jump" {
			wantKept(t, res)
			if p.Position != 19800 || f.stayVersion(t, f.book) != 0 {
				t.Fatalf("plain jump moved the row: %+v", p)
			}
			continue
		}
		wantResult(t, res, statusApplied, "")
		if p.Position != 18000 || p.DeviceID != "phone" || p.Version != 5 || p.OtherPlace != nil ||
			f.stayVersion(t, f.book) != 5 {
			t.Fatalf("after the Stay: %+v other %+v stay %d", p, p.OtherPlace, f.stayVersion(t, f.book))
		}
	}
}

// A Stay beats the other device's listening that ended no more than
// staySlack after it (a clock a little ahead, a span in flight); one that went
// on longer after it, or a Stay made offline long ago, is a plain jump.
func TestSyncStaySlack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		watchLast, stayAgo time.Duration
		wins               bool
	}{
		{2 * time.Minute, 3 * time.Minute, true},  // the watch's span ended 1 min after the Stay
		{2 * time.Minute, 4 * time.Minute, true},  // 2 min after: at the slack
		{2 * time.Minute, 5 * time.Minute, false}, // 3 min after
		{time.Minute, 2 * time.Hour, false},       // an old offline Stay
	} {
		f := stayFixture(t, tc.watchLast)
		wantAppliedOrKept(t, f.syncOne(t, "phone", f.stay(18000, tc.stayAgo, base(2))), tc.wins)
		if p := f.progress(t); (p.DeviceID == "phone") != tc.wins || (f.stayVersion(t, f.book) != 0) != tc.wins {
			t.Fatalf("%+v: %+v stay %d", tc, p, f.stayVersion(t, f.book))
		}
	}
}

// The slack is measured from the row's time (updated_at), not the end of the
// span whose place it holds: a place that ended long ago, applied over the
// watch's listening of a minute ago (the watch's own late backlog further on,
// an Audible place), leaves the row heard since, so an old offline Stay is
// still a plain jump.
func TestSyncStaySlackAfterAnOldPlaceApplied(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		apply func(t *testing.T, f *syncFixture)
	}{
		{"backlog", func(t *testing.T, f *syncFixture) {
			res := f.syncOne(t, "watch", f.listen("resume", 30000, 30600, 3*time.Hour, base(0)))
			if res.Status != statusDiverged || res.Kept != "event" {
				t.Fatalf("the watch's late backlog: %+v", res)
			}
		}},
		{"audible", func(t *testing.T, f *syncFixture) {
			wantResult(t, f.sync(t, f.audibleBatch(t, f.audiblePlace(30600, 3*time.Hour))).Results[0], statusApplied, "")
		}},
	} {
		f := stayFixture(t, time.Minute)
		tc.apply(t, f)
		wantKept(t, f.syncOne(t, "phone", f.stay(18000, 2*time.Hour, base(2))))
		if p := f.progress(t); p.Position != 30600 || f.stayVersion(t, f.book) != 0 {
			t.Fatalf("%s: an old Stay moved a row heard since: %+v stay %d", tc.name, p, f.stayVersion(t, f.book))
		}
	}
}

// S2: after the Stay, the watch's spans on a base below it (its backlog, an
// old offline resume, a span sent with no base) are kept out and offered,
// while the phone's own continues apply and keep the hold. The watch's first
// span after seeing the Stay resolves as before and ends the hold.
func TestSyncStayHoldsAgainstTheOtherDevice(t *testing.T) {
	t.Parallel()
	f := stayFixture(t, 5*time.Minute)
	f.phoneStays(t)
	res := f.syncOne(t, "watch", f.listen("continue", 19800, 20400, 3*time.Minute, base(4)))
	wantKept(t, res)
	if p := f.progress(t); p.Position != 18000 || p.OtherPlace == nil || p.OtherPlace.DeviceID != "watch" ||
		p.OtherPlace.Position != 20400 {
		t.Fatalf("watch's continue: %+v other %+v", p, p.OtherPlace)
	}
	wantResult(t, f.syncOne(t, "phone", f.listen("continue", 18000, 18060, 2*time.Minute, base(5))), statusApplied, "")
	if p := f.progress(t); p.Position != 18060 || p.Version != 6 || f.stayVersion(t, f.book) != 5 {
		t.Fatalf("phone's continue: %+v stay %d", p, f.stayVersion(t, f.book))
	}
	for _, ev := range []SyncEvent{
		f.listen("continue", 20400, 21000, time.Minute, base(4)),
		f.listen("resume", 30000, 30600, 30*time.Second, base(1)),
		f.listen("resume", 31000, 31600, 20*time.Second, nil),
	} {
		wantKept(t, f.syncOne(t, "watch", ev))
	}
	if p := f.progress(t); p.Position != 18060 || p.DeviceID != "phone" {
		t.Fatalf("the watch moved the held row: %+v", p)
	}
	// The watch saw the Stay (base 6 >= stay_version 5): its jump applies.
	wantResult(t, f.syncOne(t, "watch", f.listen("jump", 25000, 25015, 10*time.Second, base(6))), statusApplied, "")
	if p := f.progress(t); p.Position != 25015 || p.DeviceID != "watch" || f.stayVersion(t, f.book) != 0 {
		t.Fatalf("the watch's jump after seeing the Stay: %+v stay %d", p, f.stayVersion(t, f.book))
	}
}

// In-batch chaining with S2: the phone's base-0 continues after its Stay in the
// same POST chain on the Stay's version and keep the hold; the watch's backlog
// in one POST (a span on its old base, then base-0 spans) is held whole and
// never chains past the Stay.
func TestSyncStayInBatchChaining(t *testing.T) {
	t.Parallel()
	f := stayFixture(t, 5*time.Minute)
	a := f.sync(t, f.batch(t, "phone", f.stay(18000, 4*time.Minute, base(2)),
		f.listen("continue", 18000, 18060, 3*time.Minute, base(0)),
		f.listen("continue", 18060, 18120, 2*time.Minute, base(0))))
	for _, r := range a.Results {
		wantResult(t, r, statusApplied, "")
	}
	if p := f.progress(t); p.Position != 18120 || p.Version != 7 || f.stayVersion(t, f.book) != 5 {
		t.Fatalf("the phone's batch: %+v stay %d", p, f.stayVersion(t, f.book))
	}
	a = f.sync(t, f.batch(t, "watch", f.listen("continue", 19800, 20400, 90*time.Second, base(4)),
		f.listen("continue", 20400, 21000, time.Minute, base(0)),
		f.listen("continue", 21000, 21600, 30*time.Second, base(0))))
	for _, r := range a.Results {
		wantKept(t, r)
	}
	if p := f.progress(t); p.Position != 18120 || p.DeviceID != "phone" || f.stayVersion(t, f.book) != 5 {
		t.Fatalf("the watch's backlog moved the held row: %+v stay %d", p, f.stayVersion(t, f.book))
	}
}

// The other device answering with a jump or its own Stay from a base that saw
// the phone's Stay resolves as before (its Stay then holds the row for it); its
// Stay from below the phone's is a plain jump the phone's Stay holds out.
func TestSyncStayAnsweredByTheOtherDevice(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		start    string
		base     int64
		applied  bool
		wantPos  float64
		wantStay int64
	}{
		{"jump", 5, true, 19800, 0},
		{startStay, 5, true, 19800, 6},
		{startStay, 4, false, 18000, 5},
	} {
		f := stayFixture(t, 5*time.Minute)
		f.phoneStays(t)
		wantAppliedOrKept(t, f.syncOne(t, "watch", f.listen(tc.start, 19800, 19800, 0, base(tc.base))), tc.applied)
		if p, got := f.progress(t), f.stayVersion(t, f.book); p.Position != tc.wantPos || got != tc.wantStay {
			t.Fatalf("%+v: %+v stay_version %d", tc, p, got)
		}
	}
}

// A Stay is a plain jump on a base that saw nothing (0, none) or a version the
// row never had, on a finished row, and after an explicit write since its base
// (an edit, a finish and unfinish, an import's apply or undo, a move); a PUT
// since the offer is an old app's listening and does not stop it.
func TestSyncStayIsAPlainJump(t *testing.T) {
	t.Parallel()
	importPlace := func(t *testing.T, f *syncFixture, undo bool) {
		t.Helper()
		id := applyImport(t, f.ctx, f.c, f.user, func(st ImportState) (*ImportWrite, error) {
			have := st.Progress[f.book]
			up := have
			up.Position, up.Version, up.UpdatedAt = 21000, have.Version+1, FormatStamp(f.clock.Add(-3*time.Minute))
			return &ImportWrite{Progress: []ImportProgress{{Prior: &have, Wrote: up}}}, nil
		})
		if undo {
			if _, err := f.c.UndoImport(f.ctx, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	moved := func(f *syncFixture) Ref { return Ref{LibraryID: f.lib, Path: "Author/Moved"} }
	for _, tc := range []struct {
		name   string
		before func(t *testing.T, f *syncFixture)
		base   *int64
		on     func(f *syncFixture) Ref
		// applies: the plain jump applies by last-write-wins (else it is kept out).
		applies bool
	}{
		{name: "base 0", base: base(0)},
		{name: "no base", applies: true},
		{name: "base above the row", base: base(99)},
		{name: "finished", base: base(5), applies: true, before: func(t *testing.T, f *syncFixture) {
			wantResult(t, f.syncOne(t, "watch", f.finishEvent(EventFinish, 3*time.Minute)), statusApplied, "")
		}},
		{name: "edit", base: base(2), before: func(t *testing.T, f *syncFixture) {
			pos := 19000.0
			if _, err := f.c.EditProgress(f.ctx, f.user, f.book, ProgressEdit{Position: &pos}, Scope{AllowAll: true}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "finish and unfinish", base: base(2), before: func(t *testing.T, f *syncFixture) {
			wantResult(t, f.syncOne(t, "watch", f.finishEvent(EventFinish, 3*time.Minute)), statusApplied, "")
			wantResult(t, f.syncOne(t, "watch", f.finishEvent(EventUnfinish, 2*time.Minute)), statusApplied, "")
		}},
		{name: "import", base: base(2), before: func(t *testing.T, f *syncFixture) { importPlace(t, f, false) }},
		{name: "import undone", base: base(2), before: func(t *testing.T, f *syncFixture) { importPlace(t, f, true) }},
		{name: "move", base: base(2), on: moved, before: func(t *testing.T, f *syncFixture) {
			if err := f.c.MoveDurableState(f.ctx, f.lib, f.book.Path, moved(f).Path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		f := stayFixture(t, 5*time.Minute)
		if tc.before != nil {
			tc.before(t, f)
		}
		ev := f.stay(18000, 0, tc.base)
		ref := f.book
		if tc.on != nil {
			ref = tc.on(f)
			ev.Path = ref.Path
		}
		wantAppliedOrKept(t, f.syncOne(t, "phone", ev), tc.applies)
		if got := f.stayVersion(t, ref); got != 0 {
			t.Fatalf("%s: the plain jump holds the row (stay_version %d)", tc.name, got)
		}
	}

	// Allowed: a PUT since the offer (an old app listening on the tablet) is no
	// edit: the Stay still wins.
	f := stayFixture(t, 5*time.Minute)
	if _, err := f.c.SaveProgressFrom(f.ctx, f.user, 7, Progress{Ref: f.book, Position: 20000, Duration: syncBookLen,
		PlaybackSpeed: 1, DeviceID: "tablet", UpdatedAt: FormatStamp(f.clock.Add(-4 * time.Minute))}); err != nil {
		t.Fatal(err)
	}
	wantResult(t, f.syncOne(t, "phone", f.stay(18000, 0, base(2))), statusApplied, "")
	if p := f.progress(t); p.Position != 18000 || p.DeviceID != "phone" || f.stayVersion(t, f.book) != p.Version {
		t.Fatalf("Stay after a PUT: %+v", p)
	}
}

// Every writer but the Stay's own device ends the hold: a PUT, an edit, a
// finish, an Audible place ahead (furthest only, unchanged).
func TestSyncStayClearedByOtherWriters(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, f *syncFixture)
	}{
		{"put", func(t *testing.T, f *syncFixture) {
			if _, err := f.c.SaveProgressFrom(f.ctx, f.user, 7, Progress{Ref: f.book, Position: 18100, Duration: syncBookLen,
				PlaybackSpeed: 1, DeviceID: "phone", UpdatedAt: FormatStamp(f.clock.Add(-3 * time.Minute))}); err != nil {
				t.Fatal(err)
			}
		}},
		{"edit", editTo18100},
		{"finish", func(t *testing.T, f *syncFixture) {
			wantResult(t, f.syncOne(t, "phone", f.finishEvent(EventFinish, 3*time.Minute)), statusApplied, "")
		}},
		{"audible", func(t *testing.T, f *syncFixture) {
			wantResult(t, f.sync(t, f.audibleBatch(t, f.audiblePlace(40000, 3*time.Minute))).Results[0], statusApplied, "")
		}},
	} {
		f := stayFixture(t, 5*time.Minute)
		f.phoneStays(t)
		tc.write(t, f)
		if got := f.stayVersion(t, f.book); got != 0 {
			t.Fatalf("%s kept the hold (stay_version %d)", tc.name, got)
		}
	}
}

// server#415: an Audible place heard at or before the held row's time (the
// Stay's, 4 minutes ago) is refused like one before a restart, however far
// ahead: it is from before the listener chose the held place (the watch's
// pre-Stay place echoed back through Audible), and so is the finish an end
// place brings. The row and its hold stay. One heard after it applies
// (furthest wins) and ends the hold.
func TestSyncAudiblePlaceBeforeAStay(t *testing.T) {
	t.Parallel()
	f := stayFixture(t, 5*time.Minute)
	f.phoneStays(t)
	for _, ago := range []time.Duration{5 * time.Minute, 4 * time.Minute} {
		wantResult(t, f.sync(t, f.audibleBatch(t, f.audiblePlace(40000, ago))).Results[0], statusNotApplied, reasonBehind)
		if p := f.progress(t); p.Position != 18000 || p.DeviceID != "phone" || f.stayVersion(t, f.book) != 5 {
			t.Fatalf("an Audible place %v ago moved the held row: %+v stay %d", ago, p, f.stayVersion(t, f.book))
		}
		// A place at the book's end comes with a finish (the Bridge's
		// atBookEnd): from before the Stay it is refused too, never finishing
		// the book over the hold.
		wantResult(t, f.sync(t, f.audibleBatch(t, f.finishEvent(EventFinish, ago))).Results[0], statusNotApplied, reasonBehind)
		if p := f.progress(t); p.Finished || p.Position != 18000 || f.stayVersion(t, f.book) != 5 {
			t.Fatalf("an Audible finish %v ago ended the hold: %+v stay %d", ago, p, f.stayVersion(t, f.book))
		}
	}
	wantResult(t, f.sync(t, f.audibleBatch(t, f.audiblePlace(40000, 3*time.Minute))).Results[0], statusApplied, "")
	if p := f.progress(t); p.Position != 40000 || f.stayVersion(t, f.book) != 0 {
		t.Fatalf("an Audible place after the Stay: %+v stay %d", p, f.stayVersion(t, f.book))
	}

	// Without a hold the same dated place still applies (furthest wins, as
	// before): the rule is the Stay's, not the row's time.
	g := stayFixture(t, 5*time.Minute)
	wantResult(t, g.sync(t, g.audibleBatch(t, g.audiblePlace(40000, 6*time.Minute))).Results[0], statusApplied, "")
}

// editTo18100 is a PATCH of the fixture's row to 18100.
func editTo18100(t *testing.T, f *syncFixture) {
	t.Helper()
	pos := 18100.0
	if _, err := f.c.EditProgress(f.ctx, f.user, f.book, ProgressEdit{Position: &pos}, Scope{AllowAll: true}); err != nil {
		t.Fatal(err)
	}
}

// After an edit ends the hold, the watch's backlog resolves as before (kept
// event).
func TestSyncStayBacklogAfterAnEdit(t *testing.T) {
	t.Parallel()
	f := stayFixture(t, 5*time.Minute)
	f.phoneStays(t)
	editTo18100(t, f)
	res := f.syncOne(t, "watch", f.listen("continue", 19800, 20400, 2*time.Minute, base(4)))
	if res.Status != statusDiverged || res.Kept != "event" {
		t.Fatalf("the watch's backlog after the edit: %+v", res)
	}
}

// An unfinish of a row that isn't finished (the watch saw the book finished,
// late) changes nothing, so it keeps the phone's hold: the watch's backlog
// after it is still kept out.
func TestSyncStayKeptByANoOpUnfinish(t *testing.T) {
	t.Parallel()
	f := stayFixture(t, 5*time.Minute)
	f.phoneStays(t)
	wantResult(t, f.syncOne(t, "watch", f.finishEvent(EventUnfinish, 3*time.Minute)), statusApplied, "")
	if got := f.stayVersion(t, f.book); got != 5 {
		t.Fatalf("the no-op unfinish ended the hold (stay_version %d)", got)
	}
	wantKept(t, f.syncOne(t, "watch", f.listen("continue", 19800, 20400, 2*time.Minute, base(4))))
	if p := f.progress(t); p.Position != 18000 || p.DeviceID != "phone" {
		t.Fatalf("the watch's backlog moved the held row: %+v", p)
	}
}

// A replayed Stay is a duplicate: it never applies again over a place written
// since (the watch's, after it saw the Stay).
func TestSyncStayReplay(t *testing.T) {
	t.Parallel()
	f := stayFixture(t, 5*time.Minute)
	st := f.stay(18000, 4*time.Minute, base(2))
	wantResult(t, f.syncOne(t, "phone", st), statusApplied, "")
	wantResult(t, f.syncOne(t, "watch", f.listen("jump", 25000, 25015, 3*time.Minute, base(5))), statusApplied, "")
	wantResult(t, f.syncOne(t, "phone", st), statusDuplicate, "")
	if p := f.progress(t); p.Position != 25015 || p.DeviceID != "watch" || f.stayVersion(t, f.book) != 0 {
		t.Fatalf("after the replay: %+v", p)
	}
}

// "stay" is a start the decoder takes; a start it doesn't know is invalid.
func TestSyncStayStartChecked(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	ok := f.stay(100, 0, base(0))
	bad := f.listen("hold", 100, 100, 0, base(0))
	a := f.sync(t, f.batch(t, "phone", ok, bad))
	wantResult(t, a.Results[0], statusApplied, "")
	wantResult(t, a.Results[1], statusRejected, SyncInvalid)
}
