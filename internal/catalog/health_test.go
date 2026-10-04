package catalog

import (
	"testing"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// Phase 3: scan history, Health issues and their ignores.

func TestScanRunsRecordAndRetain(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	other, _ := c.CreateLibrary(ctx, Library{Name: "O", Root: "/tmp/o"})
	uid := seedUser(t, c, ctx)

	id, err := c.StartScanRun(ctx, lib.ID, "manual", &uid)
	if err != nil {
		t.Fatal(err)
	}
	log := []RunEvent{{Kind: "started"}, {Kind: "removed", Path: "Gone/Book"}, {Kind: "finished"}}
	if err := c.FinishScanRun(ctx, id, RunOK, ScanCounts{Books: 3, Added: 1, Removed: 1}, log); err != nil {
		t.Fatal(err)
	}
	run, err := c.GetScanRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunOK || run.Added != 1 || run.Removed != 1 || run.StartedByName != "u" ||
		run.LibraryName != "L" || run.FinishedAt == nil || len(run.Log) != 3 || run.Log[1].Path != "Gone/Book" {
		t.Fatalf("run = %+v", run)
	}
	if _, err := c.GetScanRun(ctx, id+100); err != ErrNotFound {
		t.Fatalf("missing run = %v, want ErrNotFound", err)
	}

	// A run left open by a server that stopped is marked interrupted.
	open, _ := c.StartScanRun(ctx, other.ID, "startup", nil)
	if err := c.InterruptScanRuns(ctx); err != nil {
		t.Fatal(err)
	}
	if r, _ := c.GetScanRun(ctx, open); r.Status != RunInterrupted || r.FinishedAt == nil {
		t.Fatalf("open run after restart = %+v", r)
	}
	if at, _ := c.LastScanFinished(ctx); at != *run.FinishedAt {
		t.Fatalf("last finished = %q, want %q (an interrupted run isn't a check)", at, *run.FinishedAt)
	}

	// Only the newest maxScanRunsPerLibrary are kept, per library.
	for range maxScanRunsPerLibrary + 5 {
		id, _ := c.StartScanRun(ctx, lib.ID, "schedule", nil)
		if err := c.FinishScanRun(ctx, id, RunOK, ScanCounts{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := c.ListScanRuns(ctx, lib.ID, 0, 200)
	if len(all) != maxScanRunsPerLibrary {
		t.Fatalf("kept %d runs, want %d", len(all), maxScanRunsPerLibrary)
	}
	if o, _ := c.ListScanRuns(ctx, other.ID, 0, 200); len(o) != 1 {
		t.Fatalf("the other library's history was trimmed: %d", len(o))
	}
	// Paging: older than a run id.
	page, _ := c.ListScanRuns(ctx, lib.ID, all[9].ID, 5)
	if len(page) != 5 || page[0].ID >= all[9].ID {
		t.Fatalf("page before %d = %+v", all[9].ID, page)
	}
	starts, _ := c.LastScanStarts(ctx)
	if starts[lib.ID].IsZero() || starts[other.ID].IsZero() {
		t.Fatalf("last starts = %v", starts)
	}
}

// issueBooks seeds one book per issue kind (plus a healthy one).
func issueBooks(t *testing.T, c *Catalog, libID int64) {
	t.Helper()
	yes, no := true, false
	two := 2
	chapters := []metadata.Chapter{{Index: 0, Title: "1"}, {Index: 1, Title: "2"}}
	for _, b := range []*Book{
		{RelPath: "healthy", Title: "Healthy", Author: "A", ASIN: "B1", Duration: 9000, Codec: "aac", HasCover: &yes, Chapters: chapters},
		{RelPath: "nocover", Title: "No Cover", Author: "A", ASIN: "B2", Codec: "aac", HasCover: &no, Chapters: chapters},
		{RelPath: "unchecked", Title: "Unchecked", Author: "A", ASIN: "B3", Codec: "aac", Chapters: chapters}, // has_cover NULL
		{RelPath: "long", Title: "Long", Author: "A", ASIN: "B4", Duration: 3 * 3600, Codec: "aac", HasCover: &yes},
		{RelPath: "ac3", Title: "AC3", Author: "A", ASIN: "B5", Codec: "ac3", HasCover: &yes, Chapters: chapters},
		{RelPath: "suspect", Title: "Suspect", Author: "A", ASIN: "B6", IsFolder: true, Codec: "aac", HasCover: &yes, Chapters: chapters, SuspectParts: &two},
		{RelPath: "broken", Title: "Broken", Author: "A", ASIN: "B7", Codec: "aac", HasCover: &yes, Chapters: chapters,
			ScanError: "empty_file", ScanErrorFile: "broken/02.mp3"},
		{RelPath: "unmatched", Title: "Unmatched", Author: "A", Codec: "aac", HasCover: &yes, Chapters: chapters},
	} {
		b.LibraryID = libID
		if _, err := c.UpsertBook(t.Context(), b); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIssueCountsAndIgnores(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	issueBooks(t, c, lib.ID)
	uid := seedUser(t, c, ctx)

	want := map[string]string{
		IssueNoCover: "nocover", IssueNoChapters: "long", IssueTranscode: "ac3",
		IssueSuspect: "suspect", IssueScanError: "broken", IssueUnmatched: "unmatched",
	}
	kinds := []string{IssueNoCover, IssueNoChapters, IssueTranscode, IssueSuspect, IssueScanError, IssueUnmatched}
	counts, err := c.IssueCounts(ctx, kinds)
	if err != nil {
		t.Fatal(err)
	}
	for _, ic := range counts {
		if ic.Count != 1 || len(ic.Samples) != 1 || ic.Samples[0].Path != want[ic.Kind] {
			t.Errorf("%s = %+v, want just %q", ic.Kind, ic, want[ic.Kind])
		}
		// The list for the kind is the same set the count saw.
		page, err := c.ListAdminBooks(ctx, AdminListOptions{Filter: BookFilter{Issue: ic.Kind}})
		if err != nil || len(page.Books) != 1 || page.Books[0].Path != want[ic.Kind] {
			t.Errorf("%s list = %+v (err %v)", ic.Kind, page, err)
		}
	}

	ref := []Ref{{LibraryID: lib.ID, Path: "nocover"}}
	if err := c.IgnoreIssue(ctx, IssueNoCover, ref, uid); err != nil {
		t.Fatal(err)
	}
	if err := c.IgnoreIssue(ctx, IssueNoCover, ref, uid); err != nil { // idempotent
		t.Fatal(err)
	}
	counts, _ = c.IssueCounts(ctx, []string{IssueNoCover})
	if counts[0].Count != 0 || counts[0].Ignored != 1 {
		t.Fatalf("after ignoring: %+v", counts[0])
	}
	ignored, _ := c.ListAdminBooks(ctx, AdminListOptions{Filter: BookFilter{Issue: IssueNoCover, IssueIgnored: true}})
	if len(ignored.Books) != 1 || ignored.Books[0].Path != "nocover" {
		t.Fatalf("ignored list = %+v", ignored.Books)
	}
	// An ignore follows the book when it moves.
	if err := c.MoveDurableState(ctx, lib.ID, "nocover", "moved/nocover"); err != nil {
		t.Fatal(err)
	}
	var path string
	if err := c.db.QueryRowContext(ctx, `SELECT path FROM issue_ignores`).Scan(&path); err != nil || path != "moved/nocover" {
		t.Fatalf("ignore after the move is at %q (err %v)", path, err)
	}
	if err := c.UnignoreIssue(ctx, IssueNoCover, []Ref{{LibraryID: lib.ID, Path: "moved/nocover"}}); err != nil {
		t.Fatal(err)
	}

	if err := c.IgnoreIssue(ctx, "bogus", ref, uid); err == nil {
		t.Fatal("an unknown kind was accepted")
	}
	// An ignore for a library that doesn't exist is dropped, not an error.
	if err := c.IgnoreIssue(ctx, IssueNoCover, []Ref{{LibraryID: 999, Path: "x"}}, uid); err != nil {
		t.Fatal(err)
	}
}

func TestDuplicateGroups(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	other, _ := c.CreateLibrary(ctx, Library{Name: "O", Root: "/tmp/o"})
	uid := seedUser(t, c, ctx)
	for _, b := range []*Book{
		// Same audio (fingerprint and size): one copy is an m4b, kept.
		{LibraryID: lib.ID, RelPath: "Dune", Title: "Dune", Author: "Frank Herbert", Format: "m4b", ContentHash: "h1", Size: 100, Duration: 3600},
		{LibraryID: lib.ID, RelPath: "Dune (copy)", Title: "Dune rip", Author: "X", Format: "m4b", ContentHash: "h1", Size: 100, Duration: 3600},
		// Same fingerprint, different size: an identical first part, not a copy.
		{LibraryID: lib.ID, RelPath: "Intro A", Title: "A", Author: "Y", ContentHash: "h2", Size: 10},
		{LibraryID: lib.ID, RelPath: "Intro B", Title: "B", Author: "Z", ContentHash: "h2", Size: 20},
		// Same author/title/narrator and length: an mp3 rip and an m4b.
		{LibraryID: lib.ID, RelPath: "Hail Mary mp3", IsFolder: true, Title: "Project Hail Mary", Author: "Andy Weir", Format: "mp3", Duration: 58000, Size: 500},
		{LibraryID: lib.ID, RelPath: "Hail Mary", Title: "Project Hail Mary", Author: "Andy Weir", Format: "m4b", Duration: 58020, Size: 400},
		// Same title, very different length: an abridged edition, not a copy.
		{LibraryID: lib.ID, RelPath: "Hail Mary abridged", Title: "Project Hail Mary", Author: "Andy Weir", Format: "m4b", Duration: 20000},
		// The same book in another library is deliberate.
		{LibraryID: other.ID, RelPath: "Dune", Title: "Dune", Author: "Frank Herbert", Format: "m4b", ContentHash: "h1", Size: 100},
	} {
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	groups, err := c.DuplicateGroups(ctx, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %+v, want 2", groups)
	}
	byKeep := map[string]DuplicateGroup{}
	for _, g := range groups {
		if len(g.Books) != 2 {
			t.Fatalf("group %+v, want 2 copies", g)
		}
		byKeep[g.Books[0].Path] = g
	}
	if g := byKeep["Dune"]; g.Reason != "same_files" || g.Books[1].Path != "Dune (copy)" {
		t.Fatalf("Dune group = %+v", g)
	}
	if g := byKeep["Hail Mary"]; g.Reason != "same_book" || g.Books[1].Path != "Hail Mary mp3" {
		t.Fatalf("Hail Mary group = %+v (the m4b is the copy to keep)", g)
	}

	// "They're different books": every member ignored hides the group...
	ignore := []Ref{{LibraryID: lib.ID, Path: "Hail Mary"}, {LibraryID: lib.ID, Path: "Hail Mary mp3"}}
	if err := c.IgnoreIssue(ctx, IssueDuplicate, ignore, uid); err != nil {
		t.Fatal(err)
	}
	if groups, _ := c.DuplicateGroups(ctx, lib.ID, false); len(groups) != 1 {
		t.Fatalf("after ignoring a group: %+v", groups)
	}
	counts, _ := c.IssueCounts(ctx, []string{IssueDuplicate})
	if counts[0].Count != 1 || counts[0].Ignored != 1 {
		t.Fatalf("duplicate counts = %+v", counts[0])
	}
	// ... until a new copy turns up.
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "Hail Mary 2", Title: "Project Hail Mary",
		Author: "Andy Weir", Format: "m4b", Duration: 58010}); err != nil {
		t.Fatal(err)
	}
	if groups, _ := c.DuplicateGroups(ctx, lib.ID, false); len(groups) != 2 {
		t.Fatalf("a new copy didn't bring the group back: %+v", groups)
	}
}

func TestLibraryScanSettings(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l", ScanSchedule: "every:6h", IgnorePatterns: []string{"*.tmp", "Extras/"}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := c.GetLibrary(ctx, lib.ID)
	if got.ScanSchedule != "every:6h" || len(got.IgnorePatterns) != 2 || got.IgnorePatterns[1] != "Extras/" {
		t.Fatalf("library = %+v", got)
	}
	// A patch that leaves the settings out keeps them; an empty list clears.
	updated, _, _ := c.UpdateLibrary(ctx, lib.ID, LibraryPatch{Name: "Renamed"})
	if updated.ScanSchedule != "every:6h" || len(updated.IgnorePatterns) != 2 {
		t.Fatalf("a rename lost the settings: %+v", updated)
	}
	off, none := "", []string{}
	updated, _, _ = c.UpdateLibrary(ctx, lib.ID, LibraryPatch{ScanSchedule: &off, IgnorePatterns: &none})
	if updated.ScanSchedule != "" || len(updated.IgnorePatterns) != 0 {
		t.Fatalf("clearing the settings: %+v", updated)
	}
	if l, _ := c.GetLibrary(ctx, lib.ID); len(l.IgnorePatterns) != 0 || l.IgnorePatterns == nil {
		t.Fatalf("stored = %#v, want an empty list", l.IgnorePatterns)
	}
}

// A folder an admin set to "one book" (the suspect category's own fix) leaves the
// suspect count and list.
func TestSuspectSettledByBookOverride(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	issueBooks(t, c, lib.ID)
	if err := c.SetFolderOverride(ctx, lib.ID, "suspect", OverrideBook); err != nil {
		t.Fatal(err)
	}
	counts, err := c.IssueCounts(ctx, []string{IssueSuspect})
	if err != nil {
		t.Fatal(err)
	}
	page, err := c.ListAdminBooks(ctx, AdminListOptions{Filter: BookFilter{Issue: IssueSuspect}})
	if err != nil {
		t.Fatal(err)
	}
	if counts[0].Count != 0 || len(page.Books) != 0 {
		t.Fatalf("suspect after a book override: count %d, list %+v", counts[0].Count, page.Books)
	}
}

// A copy of unknown length doesn't join an abridged and an unabridged edition,
// and the ignored view lists only ignored groups.
func TestDuplicateLengthsAndIgnoredView(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	uid := seedUser(t, c, ctx)
	for _, b := range []*Book{
		{LibraryID: lib.ID, RelPath: "full", Title: "Dune", Author: "Frank Herbert", Duration: 75000},
		{LibraryID: lib.ID, RelPath: "unknown", Title: "Dune", Author: "Frank Herbert"},
		{LibraryID: lib.ID, RelPath: "abridged", Title: "Dune", Author: "Frank Herbert", Duration: 20000},
		{LibraryID: lib.ID, RelPath: "a1", Title: "Emma", Author: "Jane Austen", Duration: 1000},
		{LibraryID: lib.ID, RelPath: "a2", Title: "Emma", Author: "Jane Austen", Duration: 1010},
	} {
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	groups, _ := c.DuplicateGroups(ctx, 0, false)
	if len(groups) != 1 || groups[0].Books[0].Title != "Emma" {
		t.Fatalf("groups = %+v, want only Emma", groups)
	}
	if err := c.IgnoreIssue(ctx, IssueDuplicate, []Ref{{LibraryID: lib.ID, Path: "a1"}, {LibraryID: lib.ID, Path: "a2"}}, uid); err != nil {
		t.Fatal(err)
	}
	if open, _ := c.DuplicateGroups(ctx, 0, false); len(open) != 0 {
		t.Fatalf("open groups = %+v", open)
	}
	if ign, _ := c.DuplicateGroups(ctx, 0, true); len(ign) != 1 || !ign[0].Ignored {
		t.Fatalf("ignored groups = %+v", ign)
	}
}
