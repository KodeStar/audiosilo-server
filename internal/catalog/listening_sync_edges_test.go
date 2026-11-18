package catalog

import (
	"testing"
	"time"
)

// Sync v2 edges after the Stay rule (server#415) and Audible sync (server#359):
// an offline finish against a held Stay, an in-flight finish against an Undo,
// a short book's finish ahead of its span, and a Stay whose base a move or an
// import's undo made meaningless.

// server#415: a player's finish made at or before the Stay that holds the row
// (4 minutes ago), arriving after it, is "behind" like an Audible one: the Stay
// is the listener's later word. The row and its hold stay. A finish made after
// the Stay applies and ends the hold.
func TestSyncFinishBeforeAStay(t *testing.T) {
	t.Parallel()
	f := stayFixture(t, 5*time.Minute)
	f.phoneStays(t)
	for _, ago := range []time.Duration{6 * time.Minute, 4 * time.Minute} {
		wantResult(t, f.syncOne(t, "watch", f.finishEvent(EventFinish, ago)), statusNotApplied, reasonBehind)
		if p := f.progress(t); p.Finished || p.Position != 18000 || p.DeviceID != "phone" || f.stayVersion(t, f.book) != 5 {
			t.Fatalf("a finish %v ago ended the hold: %+v stay %d", ago, p, f.stayVersion(t, f.book))
		}
	}
	wantResult(t, f.syncOne(t, "watch", f.finishEvent(EventFinish, 3*time.Minute)), statusApplied, "")
	if p := f.progress(t); !p.Finished || f.stayVersion(t, f.book) != 0 {
		t.Fatalf("a finish after the Stay: %+v stay %d", p, f.stayVersion(t, f.book))
	}
}

// finishSentAt is a batch of device's finish made at made (with base b), sent
// at made too and landing at the fixture's clock: its sent_at shift is the
// request's time on the way.
func (f *syncFixture) finishSentAt(t *testing.T, device string, made time.Time, b *int64) SyncBatch {
	t.Helper()
	ev := f.finishEvent(EventFinish, f.clock.Sub(made))
	ev.BaseVersion = b
	batch := f.batch(t, device, ev)
	batch.SentAt = &made
	return batch
}

// server#415: a finish made just before an Undo (a progress edit naming
// finished) but landing after it is dated after the Undo by its time on the
// way; made without seeing the edit (its base below the edit's version), it is
// dated back by that time and is "older", so the Undo stands. One made after
// seeing the edit, or made after the edit on any base, applies.
func TestSyncFinishInFlightBeforeAnUndo(t *testing.T) {
	t.Parallel()
	undo := func(t *testing.T) (*syncFixture, time.Time) {
		t.Helper()
		f := newSyncFixture(t)
		wantResult(t, f.syncOne(t, "phone", f.listen("resume", 0, 600, 10*time.Minute, base(0))), statusApplied, "")
		undoneAt := f.clock
		no := false
		if _, err := f.c.EditProgress(f.ctx, f.user, f.book, ProgressEdit{Finished: &no}, Scope{AllowAll: true}); err != nil {
			t.Fatal(err)
		}
		f.clock = f.clock.Add(time.Second) // the finish lands a second after the Undo
		return f, undoneAt
	}
	// Made 2 s before the Undo, on the version the phone saw before it (1).
	f, at := undo(t)
	wantResult(t, f.sync(t, f.finishSentAt(t, "phone", at.Add(-2*time.Second), base(1))).Results[0], statusNotApplied, reasonOlder)
	if p := f.progress(t); p.Finished {
		t.Fatalf("an in-flight finish undid the Undo: %+v", p)
	}
	// With no base sent too.
	f, at = undo(t)
	wantResult(t, f.sync(t, f.finishSentAt(t, "phone", at.Add(-2*time.Second), nil)).Results[0], statusNotApplied, reasonOlder)

	// Allowed: made on the edit's version (2), it saw the Undo.
	f, at = undo(t)
	wantResult(t, f.sync(t, f.finishSentAt(t, "phone", at.Add(-2*time.Second), base(2))).Results[0], statusApplied, "")
	// Allowed: made after the Undo, on an old base: its transit (a second)
	// doesn't take it back before the edit.
	f, at = undo(t)
	f.clock = f.clock.Add(10 * time.Second)
	wantResult(t, f.sync(t, f.finishSentAt(t, "phone", at.Add(10*time.Second), base(1))).Results[0], statusApplied, "")
	if p := f.progress(t); !p.Finished {
		t.Fatalf("a finish after the Undo: %+v", p)
	}
}

// server#415: the transit rule dates back a finish made without seeing the
// row's last finish-state change, by its batch's whole shift up to 30 s, so on
// a clock running behind it lands before a change it was made after. Never
// after the device's own change (a restart, then Mark finished before the
// restart's answer moved its base): that is its own sync event, which it saw.
// Another device's restart is still "older" than a finish made just after it
// on a clock that far behind.
func TestSyncFinishAfterItsOwnUnfinish(t *testing.T) {
	t.Parallel()
	const behind = 20 * time.Second // the phone's clock
	restart := func(t *testing.T, device string) (*syncFixture, time.Time) {
		t.Helper()
		f := newSyncFixture(t)
		wantResult(t, f.syncOne(t, "phone", f.finishEvent(EventFinish, 10*time.Minute)), statusApplied, "")
		// The restart, on the device's clock (the phone's behind), lands at once:
		// its shift moves it to now.
		b := f.finishSentAt(t, device, f.clock.Add(-behind), nil)
		b.Events[0].Kind = EventUnfinish
		wantResult(t, f.sync(t, b).Results[0], statusApplied, "")
		restartedAt := f.clock
		f.clock = f.clock.Add(6 * time.Second) // the finish is made 5 s later, a second on the way
		return f, restartedAt
	}
	finish := func(f *syncFixture, restartedAt time.Time) SyncBatch {
		return f.finishSentAt(t, "phone", restartedAt.Add(5*time.Second-behind), base(1))
	}
	f, at := restart(t, "phone")
	wantResult(t, f.sync(t, finish(f, at)).Results[0], statusApplied, "")
	if p := f.progress(t); !p.Finished {
		t.Fatalf("a finish after the device's own restart: %+v", p)
	}

	// Denied: the tablet's restart, which the phone never saw.
	f, at = restart(t, "tablet")
	wantResult(t, f.sync(t, finish(f, at)).Results[0], statusNotApplied, reasonOlder)
}

// server#359: a very short book's finish posted before its only span, in
// separate batches: the span that lands after the token's applied finish (its
// end just before it) closes its session finished. Another token's span, or
// one long before the finish, does not.
func TestSyncSpanAfterItsFinish(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	wantResult(t, f.syncOne(t, "phone", f.finishEvent(EventFinish, time.Minute)), statusApplied, "")
	if n := f.count(t, `SELECT COUNT(*) FROM listening_sessions`); n != 0 {
		t.Fatalf("setup: the finish opened %d sessions", n)
	}
	f.syncOne(t, "phone", f.listen("resume", syncBookLen-12, syncBookLen, time.Minute+time.Second, base(0)))
	if n := f.count(t, `SELECT COUNT(*) FROM listening_sessions WHERE token_id = ? AND finished = 1`, tokenOf("phone")); n != 1 {
		t.Fatalf("the span after its finish left %d finished sessions, want 1", n)
	}

	// Denied: another device's span at the same time, and the phone's span
	// that ended long before the finish.
	g := newSyncFixture(t)
	wantResult(t, g.syncOne(t, "phone", g.finishEvent(EventFinish, time.Minute)), statusApplied, "")
	g.syncOne(t, "tablet", g.listen("resume", syncBookLen-12, syncBookLen, time.Minute+time.Second, base(0)))
	g.syncOne(t, "phone", g.listen("resume", 0, 600, 2*time.Hour, base(0)))
	if n := g.count(t, `SELECT COUNT(*) FROM listening_sessions WHERE finished = 1`); n != 0 {
		t.Fatalf("%d sessions finished by spans that aren't the finish's", n)
	}
}

// server#415: a move with no row at the new path keeps the row's version, so
// a Stay on that version (4) passed the edit check after it; the carry records
// its write above it, so the Stay is a plain jump. The device that listens
// after the move writes the next version, and a Stay that saw it wins.
func TestSyncStayOnTheVersionAMoveCarried(t *testing.T) {
	t.Parallel()
	// movedFixture moves the Stay fixture's book; its events then name the
	// moved book.
	movedFixture := func(t *testing.T) (*syncFixture, Ref) {
		t.Helper()
		f := stayFixture(t, 5*time.Minute)
		moved := Ref{LibraryID: f.lib, Path: "Author/Moved"}
		if err := f.c.MoveDurableState(f.ctx, f.lib, f.book.Path, moved.Path); err != nil {
			t.Fatal(err)
		}
		return f, moved
	}
	at := func(ev SyncEvent, ref Ref) SyncEvent { ev.Path = ref.Path; return ev }
	f, moved := movedFixture(t)
	f.syncOne(t, "phone", at(f.stay(18000, 0, base(4)), moved))
	if got := f.stayVersion(t, moved); got != 0 {
		t.Fatalf("a Stay on the moved row's version holds it (stay_version %d)", got)
	}

	g, moved := movedFixture(t)
	wantResult(t, g.syncOne(t, "watch", at(g.listen("continue", 19800, 19860, time.Minute, base(4)), moved)), statusApplied, "")
	wantResult(t, g.syncOne(t, "phone", at(g.stay(18000, 0, base(5)), moved)), statusApplied, "")
	if got := g.stayVersion(t, moved); got != 6 {
		t.Fatalf("a Stay on a version after the move: stay_version %d, want 6", got)
	}
}

// server#415: an import's undo that deletes the row it made loses its
// versions, so a Stay whose base names one of them is a plain jump: one made
// before the undo, and one made after it on an offer shown before it (a stale
// base). A sync writing the row again starts above them, so a Stay made after
// the undo on the new row's version wins.
func TestSyncStayBeforeAnImportUndo(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) *syncFixture {
		t.Helper()
		f := newSyncFixture(t)
		id := applyImport(t, f.ctx, f.c, f.user, func(ImportState) (*ImportWrite, error) {
			return &ImportWrite{Progress: []ImportProgress{{Wrote: Progress{Ref: f.book, Position: 21000,
				Duration: syncBookLen, PlaybackSpeed: 1, Version: 1, UpdatedAt: FormatStamp(f.clock.Add(-time.Hour))}}}}, nil
		})
		if _, err := f.c.UndoImport(f.ctx, id); err != nil {
			t.Fatal(err)
		}
		if f.progress(t) != nil {
			t.Fatal("setup: the undo left the row")
		}
		f.clock = f.clock.Add(2 * time.Minute)
		wantResult(t, f.syncOne(t, "tablet", f.listen("resume", 5000, 5060, time.Minute, base(0))), statusApplied, "")
		if p := f.progress(t); p.Version != 2 {
			t.Fatalf("setup: the row written after the undo is at version %d, want 2 (above the deleted row's 1)", p.Version)
		}
		return f
	}
	// Made 3 minutes ago (before the undo), and made now on an offer shown
	// before it, each on the deleted row's version 1: a jump that never saw the
	// tablet's row (diverged: the further place applies, the tablet's offered),
	// holding nothing.
	for _, ago := range []time.Duration{3 * time.Minute, 0} {
		f := setup(t)
		res := f.syncOne(t, "phone", f.stay(21000, ago, base(1)))
		if res.Status != statusDiverged || res.Kept != "event" || res.OtherPlace == nil || res.OtherPlace.DeviceID != "tablet" {
			t.Fatalf("a Stay %v ago on the deleted row's version: %+v, want a diverged jump", ago, res)
		}
		if got := f.stayVersion(t, f.book); got != 0 {
			t.Fatalf("a Stay %v ago on the deleted row's version holds the row (stay_version %d)", ago, got)
		}
	}
	// Allowed: made now, on the new row's version 2.
	g := setup(t)
	wantResult(t, g.syncOne(t, "phone", g.stay(21000, 0, base(2))), statusApplied, "")
	if p := g.progress(t); p.DeviceID != "phone" || g.stayVersion(t, g.book) != 3 {
		t.Fatalf("a Stay after the undo: %+v stay %d", p, g.stayVersion(t, g.book))
	}
}
