package catalog

import (
	"database/sql"
	"testing"
	"time"
)

// The player's progress carries the server's start and finish dates; a client
// can't set them through a save.
func TestProgressDatesAreTheServers(t *testing.T) {
	f := newSessionFixture(t)
	at := func() string { return f.clock.UTC().Format(time.RFC3339) }
	forged := "2001-01-01T00:00:00Z"

	saved, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.book, Position: 10, Duration: 7200,
		UpdatedAt: at(), StartedAt: forged, FinishedAt: forged})
	if err != nil {
		t.Fatal(err)
	}
	started := at()
	if saved.StartedAt != started || saved.FinishedAt != "" {
		t.Fatalf("first save echo: started %q finished %q, want %q and none", saved.StartedAt, saved.FinishedAt, started)
	}

	f.clock = f.clock.Add(time.Hour)
	saved, err = f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.book, Position: 7200, Duration: 7200, Finished: true,
		UpdatedAt: at(), StartedAt: forged, FinishedAt: forged})
	if err != nil {
		t.Fatal(err)
	}
	finished := at()
	if saved.StartedAt != started || saved.FinishedAt != finished {
		t.Fatalf("finishing save echo: %+v, want started %q finished %q", saved, started, finished)
	}
	got, err := f.c.GetProgress(f.ctx, f.user, f.book)
	if err != nil || got.StartedAt != started || got.FinishedAt != finished {
		t.Fatalf("get: %+v %v", got, err)
	}
	list, err := f.c.ListProgress(f.ctx, f.user, []Scope{{LibraryID: f.lib, AllowAll: true}})
	if err != nil || len(list) != 1 || list[0].StartedAt != started || list[0].FinishedAt != finished {
		t.Fatalf("list: %+v %v", list, err)
	}

	// A stale save loses and echoes the stored row, dates included.
	stale, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.book, Position: 5, Duration: 7200,
		UpdatedAt: f.clock.Add(-2 * time.Hour).Format(time.RFC3339), FinishedAt: forged})
	if err != nil || stale.Position != 7200 || stale.FinishedAt != finished {
		t.Fatalf("stale save echo: %+v %v", stale, err)
	}
}

// Marking a book unfinished keeps its position and start, and clears the finish;
// AsProgress is the player's shape of the edited row.
func TestEditProgressMarkUnfinishedKeepsPosition(t *testing.T) {
	f := newSessionFixture(t)
	if _, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.book, Position: 7000, Duration: 7200, Finished: true,
		UpdatedAt: f.clock.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(time.Minute)
	no := false
	got, err := f.c.EditProgress(f.ctx, f.user, f.book, ProgressEdit{Finished: &no}, Scope{LibraryID: f.lib, AllowAll: true})
	if err != nil {
		t.Fatal(err)
	}
	p := got.AsProgress()
	if p.Finished || p.Position != 7000 || p.FinishedAt != "" || p.StartedAt != "2026-10-01T09:00:00Z" ||
		p.Version != 2 || p.UpdatedAt != "2026-10-01T09:01:00.000Z" {
		t.Fatalf("mark unfinished = %+v", p)
	}
}

// An edit keeps its sub-second time: a device save made earlier in the same
// second that reaches the server after the edit is older, and loses
// last-write-wins as an older save must (a whole-second stamp let it win).
func TestEditProgressBeatsAnOlderSaveInTheSameSecond(t *testing.T) {
	f := newSessionFixture(t)
	if _, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.book, Position: 3000, Duration: 7200,
		UpdatedAt: f.clock.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(5*time.Second + 900*time.Millisecond) // the edit, at 09:00:05.900
	yes := true
	if _, err := f.c.EditProgress(f.ctx, f.user, f.book, ProgressEdit{Finished: &yes}, Scope{LibraryID: f.lib, AllowAll: true}); err != nil {
		t.Fatal(err)
	}
	older := f.clock.Add(-400 * time.Millisecond).Format(time.RFC3339Nano) // a save from 09:00:05.500
	got, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.book, Position: 3010, Duration: 7200, UpdatedAt: older})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Finished || got.Position != 7200 {
		t.Fatalf("an older save undid the edit: %+v", got)
	}
}

// SaveProgress compares and writes in one transaction: a newer write that
// commits while an older save waits for the writer is not overwritten by it.
func TestSaveProgressKeepsANewerWriteItRaced(t *testing.T) {
	f := newSessionFixture(t)
	if _, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.book, Position: 100, Duration: 7200,
		UpdatedAt: f.clock.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	older := f.clock.Add(time.Second).Format(time.RFC3339)     // a device's save
	newer := f.clock.Add(2 * time.Second).Format(time.RFC3339) // the write that beats it (an edit, another device)
	done := make(chan error, 1)
	if err := f.c.db.WithTx(f.ctx, "newer write", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(f.ctx, `UPDATE progress SET position = 200, updated_at = ?, version = version + 1
		  WHERE user_id = ? AND library_id = ? AND rel_path = ?`, newer, f.user, f.book.LibraryID, f.book.Path); err != nil {
			return err
		}
		go func() {
			_, err := f.c.SaveProgress(f.ctx, f.user, Progress{Ref: f.book, Position: 150, Duration: 7200, UpdatedAt: older})
			done <- err
		}()
		time.Sleep(100 * time.Millisecond) // the older save starts while the newer write is uncommitted
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := f.c.GetProgress(f.ctx, f.user, f.book)
	if err != nil || got.Position != 200 || got.UpdatedAt != newer {
		t.Fatalf("the older save overwrote the newer write: %+v %v", got, err)
	}
}
