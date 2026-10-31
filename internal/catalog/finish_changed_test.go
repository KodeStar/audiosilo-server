package catalog

import (
	"context"
	"testing"
	"time"
)

// Every non-sync writer of a row's finished state records finish_changed_at, so
// a Sync v2 finish or unfinish event older than the write is "older" and can't
// undo it, while a newer one still applies.

// syncFinishOn posts one finish or unfinish event on ref, dated at, from a
// device whose clock is the catalog's (no shift), and returns its result.
func syncFinishOn(t *testing.T, c *Catalog, ctx context.Context, user int64, ref Ref, kind string, at time.Time) SyncResult {
	t.Helper()
	sent := c.now()
	a, err := c.SyncListening(ctx, SyncBatch{UserID: user, DeviceID: "watch", TokenID: 9, DeviceName: "Watch",
		SentAt: &sent, Targets: map[Ref]SyncTarget{ref: {Key: ref}},
		Events: []SyncEvent{{ID: nextSyncID("fin-"), Kind: kind, LibraryID: ref.LibraryID,
			Path: ref.Path, At: at.Format(time.RFC3339Nano)}}})
	if err != nil {
		t.Fatal(err)
	}
	return a.Results[0]
}

// An imported finish (dated at the source's save, day 3 21:00) is not undone by
// an unfinish from before it; one after it still unfinishes the book. An undo
// that brings back an unfinished row is a finish-state change too.
func TestImportedFinishHoldsAgainstAnOlderUnfinish(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t)
	f.apply(t, f.newImport(t, day(10, 0)))
	if p, _ := f.c.GetProgress(f.ctx, f.user, f.book); p == nil || !p.Finished {
		t.Fatalf("import did not finish: %+v", p)
	}
	wantResult(t, syncFinishOn(t, f.c, f.ctx, f.user, f.book, EventUnfinish, day(3, 20)), statusNotApplied, reasonOlder)
	if p, _ := f.c.GetProgress(f.ctx, f.user, f.book); !p.Finished {
		t.Fatalf("an older unfinish undid the imported finish: %+v", p)
	}
	wantResult(t, syncFinishOn(t, f.c, f.ctx, f.user, f.book, EventUnfinish, day(3, 22)), statusApplied, "")
	if p, _ := f.c.GetProgress(f.ctx, f.user, f.book); p.Finished {
		t.Fatalf("a newer unfinish did not apply: %+v", p)
	}

	// The stamp never runs ahead of the server: a source save dated in the future
	// is a change at now, so an unfinish sent just after the import applies.
	g := newImportFixture(t)
	id := g.newImport(t, day(10, 0))
	future := g.clock.Add(time.Hour)
	if _, err := g.c.ApplyImport(g.ctx, id, func(st ImportState) (*ImportWrite, error) {
		w := g.write(st)
		w.Progress[0].Wrote.UpdatedAt = FormatStamp(future)
		return w, nil
	}); err != nil {
		t.Fatal(err)
	}
	g.clock = g.clock.Add(time.Second)
	wantResult(t, syncFinishOn(t, g.c, g.ctx, g.user, g.book, EventUnfinish, g.clock), statusApplied, "")
}

// An undo restoring a row the import had finished (its prior unfinished) is a
// finish-state change too: a finish event from before the restored save is
// "older", a newer one applies.
func TestImportUndoStampsTheFinishChange(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t)
	if _, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.other, Position: 100, Duration: 3600,
		DeviceID: "phone", UpdatedAt: FormatStamp(day(2, 10))}); err != nil {
		t.Fatal(err)
	}
	id := f.newImport(t, day(10, 0))
	if _, err := f.c.ApplyImport(f.ctx, id, func(st ImportState) (*ImportWrite, error) {
		have := st.Progress[f.other]
		up := have
		up.Position, up.Finished, up.Version, up.UpdatedAt = 3600, true, have.Version+1, FormatStamp(day(3, 22))
		return &ImportWrite{Progress: []ImportProgress{{Prior: &have, Wrote: up}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.UndoImport(f.ctx, id); err != nil {
		t.Fatal(err)
	}
	p, _ := f.c.GetProgress(f.ctx, f.user, f.other)
	if p == nil || p.Finished {
		t.Fatalf("undo: %+v", p)
	}
	// The undo dates the change at the restored row's time (here now: a rewound
	// row is restored as a save at now).
	wantResult(t, syncFinishOn(t, f.c, f.ctx, f.user, f.other, EventFinish, day(2, 9)), statusNotApplied, reasonOlder)
	f.clock = f.clock.Add(time.Second)
	wantResult(t, syncFinishOn(t, f.c, f.ctx, f.user, f.other, EventFinish, f.clock), statusApplied, "")
}

// An edit's finish (the listener's own PATCH and the admin's both run
// EditProgress) holds against an older unfinish; a newer one applies; an edit
// unfinishing holds against an older finish.
func TestEditedFinishHoldsAgainstAnOlderUnfinish(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	f.syncOne(t, "phone", f.listen("resume", 0, 600, time.Hour, base(0)))
	edit := func(finished bool) {
		t.Helper()
		if _, err := f.c.EditProgress(f.ctx, f.user, f.book, ProgressEdit{Finished: &finished}, Scope{AllowAll: true}); err != nil {
			t.Fatal(err)
		}
	}
	edit(true)
	wantResult(t, f.syncOne(t, "watch", f.finishEvent(EventUnfinish, time.Minute)), statusNotApplied, reasonOlder)
	if p := f.progress(t); !p.Finished {
		t.Fatalf("an older unfinish undid the edit: %+v", p)
	}
	f.clock = f.clock.Add(time.Minute)
	wantResult(t, f.syncOne(t, "watch", f.finishEvent(EventUnfinish, 0)), statusApplied, "")
	if p := f.progress(t); p.Finished {
		t.Fatalf("a newer unfinish did not apply: %+v", p)
	}
	// The other way: an edit marking it finished again, then unfinished, holds
	// against a finish from before the edit.
	f.clock = f.clock.Add(time.Minute)
	edit(true)
	f.clock = f.clock.Add(time.Minute)
	edit(false)
	wantResult(t, f.syncOne(t, "watch", f.finishEvent(EventFinish, 30*time.Second)), statusNotApplied, reasonOlder)
	if p := f.progress(t); p.Finished {
		t.Fatalf("an older finish undid the edit: %+v", p)
	}
}
