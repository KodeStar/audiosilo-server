package catalog

import (
	"testing"
)

// TestDuplicateSetsCached (server#270): duplicate detection runs once while
// nothing is written (the summary and every page share it, and a page's
// ignored filter leaves the cached groups whole), and a content write, or a
// listener's new or removed progress row, finds the groups afresh.
func TestDuplicateSetsCached(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	uid := seedUser(t, c, ctx)
	for _, b := range []*Book{
		{LibraryID: lib.ID, RelPath: "Dune", Title: "Dune", Author: "Frank Herbert", Format: "m4b", ContentHash: "h1", Size: 100, Duration: 3600},
		{LibraryID: lib.ID, RelPath: "Dune (copy)", Title: "Dune", Author: "Frank Herbert", Format: "m4b", ContentHash: "h1", Size: 100, Duration: 3600},
		{LibraryID: lib.ID, RelPath: "Emma", Title: "Emma", Author: "Jane Austen", Format: "m4b", ContentHash: "h2", Size: 50, Duration: 600},
		{LibraryID: lib.ID, RelPath: "Emma (copy)", Title: "Emma", Author: "Jane Austen", Format: "mp3", ContentHash: "h2", Size: 50, Duration: 600},
		{LibraryID: lib.ID, RelPath: "Alone", Title: "Alone", Author: "Nobody", Format: "m4b", Size: 10, Duration: 60},
	} {
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	rows, sets, err := c.duplicateSets(ctx, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 2 || len(rows) != 4 {
		t.Fatalf("sets %d rows %d, want 2 groups over their 4 copies (the lone book's row dropped)", len(sets), len(rows))
	}
	again, againSets, err := c.duplicateSets(ctx, 0, false)
	if err != nil || &again[0] != &rows[0] || &againSets[0] != &sets[0] {
		t.Fatalf("a second detection with nothing written was not the cached one (%v)", err)
	}

	// An ignored-only page filters a copy of the groups: the open page after it
	// still finds both.
	ignore := []Ref{{LibraryID: lib.ID, Path: "Emma"}, {LibraryID: lib.ID, Path: "Emma (copy)"}}
	if err := c.IgnoreIssue(ctx, IssueDuplicate, ignore, uid); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		ignored, err := c.DuplicatePage(ctx, DuplicateQuery{Ignored: true})
		if err != nil || ignored.Total != 1 {
			t.Fatalf("ignored page = %+v, %v; want the Emma group", ignored, err)
		}
		open, err := c.DuplicatePage(ctx, DuplicateQuery{})
		if err != nil || open.Total != 1 || open.Groups[0].Books[0].Path != "Dune" {
			t.Fatalf("open page = %+v, %v; want the Dune group", open, err)
		}
	}

	// A listener starting a copy (a user-state write: the content generation
	// stays) is seen at once: each copy's listeners rank it.
	if _, err := c.SaveProgress(ctx, uid, Progress{Ref: Ref{LibraryID: lib.ID, Path: "Dune (copy)"}, Position: 5, Duration: 3600}); err != nil {
		t.Fatal(err)
	}
	page, err := c.DuplicatePage(ctx, DuplicateQuery{})
	if err != nil {
		t.Fatal(err)
	}
	listeners := map[string]int{}
	for _, b := range page.Groups[0].Books {
		listeners[b.Path] = b.Listeners
	}
	if listeners["Dune (copy)"] != 1 || listeners["Dune"] != 0 {
		t.Fatalf("listeners after a first save = %v, want the copy's 1", listeners)
	}
	if page.Groups[0].Books[0].Path != "Dune (copy)" {
		t.Fatalf("kept copy = %q, want the one listened to", page.Groups[0].Books[0].Path)
	}
	// A content write (a new copy) is seen at once too.
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "Dune (third)", Title: "Dune", Author: "Frank Herbert",
		Format: "mp3", ContentHash: "h1", Size: 100, Duration: 3600}); err != nil {
		t.Fatal(err)
	}
	if page, err := c.DuplicatePage(ctx, DuplicateQuery{}); err != nil || len(page.Groups[0].Books) != 3 {
		t.Fatalf("page after a new copy = %+v, %v; want three copies", page, err)
	}
}
