package catalog

import (
	"errors"
	"testing"
)

// TestRecordMatchItemDeletedLibrary: a library deleted while a run over every
// library works leaves its books' items out instead of failing the run, and a run
// whose own library was deleted (and the run with it) answers ErrNotFound.
func TestRecordMatchItemDeletedLibrary(t *testing.T) {
	c, ctx := newTestCatalog(t)
	keep, _ := c.CreateLibrary(ctx, Library{Name: "Keep", Root: "/tmp/keep"})
	gone, _ := c.CreateLibrary(ctx, Library{Name: "Gone", Root: "/tmp/gone"})
	all, err := c.StartMatchRun(ctx, MatchRun{Mode: MatchModeMatch, Total: 2})
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := c.StartMatchRun(ctx, MatchRun{Mode: MatchModeMatch, LibraryID: &gone.ID, Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteLibrary(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}

	for _, lib := range []int64{keep.ID, gone.ID} {
		item := &MatchRunItem{LibraryID: lib, Path: "A/B", Outcome: OutcomeNone}
		if err := c.RecordMatchItem(ctx, all, item); err != nil {
			t.Fatalf("record an item of library %d: %v", lib, err)
		}
	}
	run, err := c.GetMatchRun(ctx, all)
	if err != nil || run.Done != 2 || run.Counts.None != 1 {
		t.Fatalf("run = %+v %v, want both books done and one item", run, err)
	}

	err = c.RecordMatchItem(ctx, scoped, &MatchRunItem{LibraryID: gone.ID, Path: "A/B", Outcome: OutcomeNone})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("record into a deleted run = %v, want ErrNotFound", err)
	}
}
