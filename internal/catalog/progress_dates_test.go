package catalog

import (
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
		p.Version != 2 || p.UpdatedAt != "2026-10-01T09:01:00Z" {
		t.Fatalf("mark unfinished = %+v", p)
	}
}
