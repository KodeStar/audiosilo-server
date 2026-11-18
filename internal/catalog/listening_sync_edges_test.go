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

// server#359: a very short book's finish posted before its only span, in
// separate batches: the span that lands after the token's applied finish (its
// end just before it) closes its session finished. Another token's span, or
// one long before the finish, does not.
func TestSyncSpanAfterItsFinish(t *testing.T) {
	t.Parallel()
	finished := func(f *syncFixture, device string) int {
		t.Helper()
		return f.count(t, `SELECT COUNT(*) FROM listening_sessions WHERE token_id = ? AND finished = 1`, tokenOf(device))
	}
	f := newSyncFixture(t)
	wantResult(t, f.syncOne(t, "phone", f.finishEvent(EventFinish, time.Minute)), statusApplied, "")
	if n := f.count(t, `SELECT COUNT(*) FROM listening_sessions`); n != 0 {
		t.Fatalf("setup: the finish opened %d sessions", n)
	}
	f.syncOne(t, "phone", f.listen("resume", syncBookLen-12, syncBookLen, time.Minute+time.Second, base(0)))
	if n := finished(f, "phone"); n != 1 {
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
	f := stayFixture(t, 5*time.Minute)
	moved := Ref{LibraryID: f.lib, Path: "Author/Moved"}
	if err := f.c.MoveDurableState(f.ctx, f.lib, f.book.Path, moved.Path); err != nil {
		t.Fatal(err)
	}
	ev := f.stay(18000, 0, base(4))
	ev.Path = moved.Path
	f.syncOne(t, "phone", ev)
	if got := f.stayVersion(t, moved); got != 0 {
		t.Fatalf("a Stay on the moved row's version holds it (stay_version %d)", got)
	}

	g := stayFixture(t, 5*time.Minute)
	if err := g.c.MoveDurableState(g.ctx, g.lib, g.book.Path, moved.Path); err != nil {
		t.Fatal(err)
	}
	cont := g.listen("continue", 19800, 19860, time.Minute, base(4))
	cont.Path = moved.Path
	wantResult(t, g.syncOne(t, "watch", cont), statusApplied, "")
	ev = g.stay(18000, 0, base(5))
	ev.Path = moved.Path
	wantResult(t, g.syncOne(t, "phone", ev), statusApplied, "")
	if got := g.stayVersion(t, moved); got != 6 {
		t.Fatalf("a Stay on a version after the move: stay_version %d, want 6", got)
	}
}

// server#415: an import's undo that deletes the row it made loses its
// versions (the next write starts again at 1), so a Stay made before the undo
// whose base names one of them is a plain jump. A Stay made after the undo, on
// the new row's version, wins.
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
		return f
	}
	// Made 3 minutes ago (before the undo), on the deleted row's version 1.
	f := setup(t)
	f.syncOne(t, "phone", f.stay(21000, 3*time.Minute, base(1)))
	if p := f.progress(t); p.DeviceID != "tablet" || f.stayVersion(t, f.book) != 0 {
		t.Fatalf("a Stay from before the undo: %+v stay %d", p, f.stayVersion(t, f.book))
	}
	// Allowed: made now, on the new row's version 1.
	g := setup(t)
	wantResult(t, g.syncOne(t, "phone", g.stay(21000, 0, base(1))), statusApplied, "")
	if p := g.progress(t); p.DeviceID != "phone" || g.stayVersion(t, g.book) != 2 {
		t.Fatalf("a Stay after the undo: %+v stay %d", p, g.stayVersion(t, g.book))
	}
}
