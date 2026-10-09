package catalog

import (
	"errors"
	"slices"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/chapteralign"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// chapterless is a single-file book of an hour with no chapters of its own.
func chapterless(libID int64, path string) *Book {
	return &Book{LibraryID: libID, RelPath: path, Title: "Plain", Duration: 3600, ASIN: "B000000001",
		AddedAt: "2020-01-01T00:00:00Z"}
}

// fittedFill is a community check of the chapterless book: three chapters.
func fittedFill(libID int64, path, status string) CommunityChapters {
	return CommunityChapters{
		LibraryID: libID, Path: path, Basis: "b", Status: status, WorkID: "w", RecordingID: "r",
		Detail: &chapteralign.Detail{LocalDuration: 3600, CommunityChapters: 3},
		Chapters: []metadata.Chapter{
			{Index: 0, Title: "One", FilePath: path, End: 1000},
			{Index: 1, Title: "Two", FilePath: path, Start: 1000, End: 2500, BookOffset: 1000},
			{Index: 2, Title: "Three", FilePath: path, Start: 2500, End: 3600, BookOffset: 2500},
		},
	}
}

// save records cc as a check of the book as it is now (its current basis).
func save(t *testing.T, c *Catalog, cc CommunityChapters) {
	t.Helper()
	ctx := t.Context()
	in, err := c.ChapterInputs(ctx, cc.LibraryID, cc.Path)
	if err != nil {
		t.Fatal(err)
	}
	cc.Basis = in.Basis
	if err := c.SaveCommunityChapters(ctx, cc); err != nil {
		t.Fatal(err)
	}
}

func titlesOf(b *Book) []string {
	var out []string
	for _, ch := range b.Chapters {
		out = append(out, ch.Title)
	}
	return out
}

// A fill is used with no choice made; a rename of a community chapter holds
// through a rescan; the admin can switch back to the files' (none) and to auto.
func TestCommunityChaptersFill(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "A/Plain.m4b"
	if _, err := c.UpsertBook(ctx, chapterless(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	save(t, c, fittedFill(lib.ID, p, string(chapteralign.Fill)))
	b := mustBook(t, c, ctx, lib.ID, p)
	if !slices.Equal(titlesOf(b), []string{"One", "Two", "Three"}) || b.ChaptersSource != ChaptersFromCommunity {
		t.Fatalf("after the check: %q from %q", titlesOf(b), b.ChaptersSource)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{ChapterSet: map[int]string{1: "Second"}}); err != nil {
		t.Fatal(err)
	}
	// A rescan writes the scan's chapters (none); the community ones and the
	// rename come back in the same transaction.
	if _, err := c.UpsertBook(ctx, chapterless(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	b = mustBook(t, c, ctx, lib.ID, p)
	if !slices.Equal(titlesOf(b), []string{"One", "Second", "Three"}) {
		t.Fatalf("after a rescan: %q", titlesOf(b))
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{ChapterSource: ChaptersFromFiles}); err != nil {
		t.Fatal(err)
	}
	if b = mustBook(t, c, ctx, lib.ID, p); len(b.Chapters) != 0 || b.ChaptersSource != "" {
		t.Fatalf("files chosen: %q from %q", titlesOf(b), b.ChaptersSource)
	}
	// The choice is not a metadata edit: the list's edited flag and fields (what a
	// bulk undo reverts) leave it out. (The rename is dormant: no such chapter.)
	page, err := c.ListAdminBooks(ctx, AdminListOptions{Filter: BookFilter{LibraryID: lib.ID}})
	if err != nil || len(page.Books) != 1 {
		t.Fatalf("%v %v", page, err)
	}
	if row := page.Books[0]; row.Edited || len(row.EditedFields) != 0 || row.ChaptersSource != ChaptersFromFiles || row.ChaptersCheck != "fill" {
		t.Fatalf("row %+v", row)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{ChapterSource: ChapterSourceAuto}); err != nil {
		t.Fatal(err)
	}
	if b = mustBook(t, c, ctx, lib.ID, p); !slices.Equal(titlesOf(b), []string{"One", "Second", "Three"}) {
		t.Fatalf("back to auto: %q", titlesOf(b))
	}
	// The scan's chapters are still what a check fits against.
	in, err := c.ChapterInputs(ctx, lib.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Chapters) != 0 || len(in.Files) != 1 || in.Files[0].RelPath != p || in.Files[0].Duration != 3600 || in.ASIN != "B000000001" {
		t.Fatalf("inputs %+v", in)
	}
}

// A book with chapters of its own keeps them unless the admin picks the
// community's; switching back restores its own, renames included.
func TestCommunityChaptersChosen(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "Author/Series/01 - Title"
	own := scannedBook(lib.ID, p)
	if _, err := c.UpsertBook(ctx, own); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{ChapterSet: map[int]string{1: "The Storm"}}); err != nil {
		t.Fatal(err)
	}
	refine := fittedFill(lib.ID, p, string(chapteralign.Refine))
	for i := range refine.Chapters {
		refine.Chapters[i].FilePath = p + "/a.m4b"
	}
	save(t, c, refine)
	if b := mustBook(t, c, ctx, lib.ID, p); !slices.Equal(titlesOf(b), []string{"Opening", "The Storm"}) {
		t.Fatalf("a refine is only offered: %q", titlesOf(b))
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{ChapterSource: ChaptersFromCommunity}); err != nil {
		t.Fatal(err)
	}
	if b := mustBook(t, c, ctx, lib.ID, p); !slices.Equal(titlesOf(b), []string{"One", "Two", "Three"}) {
		t.Fatalf("community chosen: %q", titlesOf(b))
	}
	// While the community's stand in, a check still sees the scan's.
	in, err := c.ChapterInputs(ctx, lib.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Chapters) != 2 || in.Chapters[1].Title != "Track 2" {
		t.Fatalf("inputs chapters %+v", in.Chapters)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{ChapterSource: ChaptersFromFiles}); err != nil {
		t.Fatal(err)
	}
	if b := mustBook(t, c, ctx, lib.ID, p); !slices.Equal(titlesOf(b), []string{"Opening", "The Storm"}) || b.Chapters[1].Start != 1800 {
		t.Fatalf("own chapters back: %+v", b.Chapters)
	}
	// A failed check never stands in, whatever was chosen.
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{ChapterSource: ChaptersFromCommunity}); err != nil {
		t.Fatal(err)
	}
	save(t, c, CommunityChapters{LibraryID: lib.ID, Path: p,
		Status: string(chapteralign.LengthMismatch), Detail: &chapteralign.Detail{}})
	if b := mustBook(t, c, ctx, lib.ID, p); !slices.Equal(titlesOf(b), []string{"Opening", "The Storm"}) {
		t.Fatalf("after a failed check: %q", titlesOf(b))
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{ChapterSource: "bogus"}); !errors.Is(err, ErrInvalidOverride) {
		t.Fatalf("a bad source: %v", err)
	}
}

func TestDueChapterChecks(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if _, err := c.UpsertBook(ctx, chapterless(lib.ID, "A/One.m4b")); err != nil {
		t.Fatal(err)
	}
	none := chapterless(lib.ID, "A/NoID.m4b")
	none.ASIN = ""
	if _, err := c.UpsertBook(ctx, none); err != nil {
		t.Fatal(err)
	}
	due, err := c.DueChapterChecks(ctx, "2000-01-01T00:00:00Z", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Path != "A/One.m4b" {
		t.Fatalf("due %+v, want the book with an identifier", due)
	}
	in, err := c.ChapterInputs(ctx, lib.ID, "A/One.m4b")
	if err != nil {
		t.Fatal(err)
	}
	cc := fittedFill(lib.ID, "A/One.m4b", string(chapteralign.Fill))
	cc.Basis = in.Basis
	if err := c.SaveCommunityChapters(ctx, cc); err != nil {
		t.Fatal(err)
	}
	if due, _ = c.DueChapterChecks(ctx, "2000-01-01T00:00:00Z", 10); len(due) != 0 {
		t.Fatalf("due after the check: %+v", due)
	}
	// Stale by age, and by a changed basis (a new ASIN).
	if due, _ = c.DueChapterChecks(ctx, "2999-01-01T00:00:00Z", 10); len(due) != 1 {
		t.Fatalf("due when old: %+v", due)
	}
	if err := c.EditBook(ctx, lib.ID, "A/One.m4b", BookEdit{Set: map[string]string{FieldASIN: "B000000002"}}); err != nil {
		t.Fatal(err)
	}
	if due, _ = c.DueChapterChecks(ctx, "2000-01-01T00:00:00Z", 10); len(due) != 1 {
		t.Fatalf("due after a new ASIN: %+v", due)
	}
	// The check follows a move.
	if err := c.MoveDurableState(ctx, lib.ID, "A/One.m4b", "B/One.m4b"); err != nil {
		t.Fatal(err)
	}
	if got, err := c.GetCommunityChapters(ctx, lib.ID, "B/One.m4b"); err != nil || got == nil || got.Status != "fill" {
		t.Fatalf("moved check %+v, %v", got, err)
	}
	if got, _ := c.GetCommunityChapters(ctx, lib.ID, "A/One.m4b"); got != nil {
		t.Fatalf("the check stayed behind: %+v", got)
	}
}

// A check of other audio (a rescan changed the files) or other identifiers is
// stale: its chapters stand down until the book is checked again.
func TestCommunityChaptersStale(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "A/Plain.m4b"
	if _, err := c.UpsertBook(ctx, chapterless(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	save(t, c, fittedFill(lib.ID, p, string(chapteralign.Fill)))
	if b := mustBook(t, c, ctx, lib.ID, p); len(b.Chapters) != 3 {
		t.Fatalf("fitted: %q", titlesOf(b))
	}
	longer := chapterless(lib.ID, p)
	longer.Duration = 4000
	longer.Chapters = []metadata.Chapter{{Title: "Whole", FilePath: p, End: 4000}}
	if _, err := c.UpsertBook(ctx, longer); err != nil {
		t.Fatal(err)
	}
	if b := mustBook(t, c, ctx, lib.ID, p); !slices.Equal(titlesOf(b), []string{"Whole"}) || b.ChaptersSource != "" {
		t.Fatalf("after a rescan of other audio: %q from %q", titlesOf(b), b.ChaptersSource)
	}
	if cc, err := c.GetCommunityChapters(ctx, lib.ID, p); err != nil || !cc.Stale {
		t.Fatalf("check %+v, %v; want stale", cc, err)
	}
	if due, _ := c.DueChapterChecks(ctx, "2000-01-01T00:00:00Z", 10); len(due) != 1 {
		t.Fatalf("due %+v, want the changed book", due)
	}
	// The same audio again: the check is current, and its chapters back.
	if _, err := c.UpsertBook(ctx, chapterless(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	if b := mustBook(t, c, ctx, lib.ID, p); len(b.Chapters) != 3 {
		t.Fatalf("back to the checked audio: %q", titlesOf(b))
	}
}

// The admin's choice is the book's: it follows a move, and the choice alone (no
// metadata edit) does not make the book "edited".
func TestChapterChoiceFollowsAMove(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if _, err := c.UpsertBook(ctx, chapterless(lib.ID, "A/Plain.m4b")); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, "A/Plain.m4b", BookEdit{ChapterSource: ChaptersFromFiles}); err != nil {
		t.Fatal(err)
	}
	if err := c.MoveDurableState(ctx, lib.ID, "A/Plain.m4b", "B/Plain.m4b"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, chapterless(lib.ID, "B/Plain.m4b")); err != nil {
		t.Fatal(err)
	}
	d, err := c.AdminBookDetail(ctx, lib.ID, "B/Plain.m4b")
	if err != nil {
		t.Fatal(err)
	}
	if d.ChapterChoice != ChaptersFromFiles || d.Book.Edited {
		t.Fatalf("choice %q, edited %v", d.ChapterChoice, d.Book.Edited)
	}
}

// A check names the files it fitted, relative to the book: after a move its
// chapters stream from the new path, and files renamed in place make it stale,
// so no chapter streams from a path that is no longer there.
func TestCommunityChaptersAfterAMove(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if _, err := c.UpsertBook(ctx, chapterless(lib.ID, "A/Plain.m4b")); err != nil {
		t.Fatal(err)
	}
	save(t, c, fittedFill(lib.ID, "A/Plain.m4b", string(chapteralign.Fill)))
	if err := c.MoveDurableState(ctx, lib.ID, "A/Plain.m4b", "B/Plain.m4b"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, chapterless(lib.ID, "B/Plain.m4b")); err != nil {
		t.Fatal(err)
	}
	b := mustBook(t, c, ctx, lib.ID, "B/Plain.m4b")
	for _, ch := range b.Chapters {
		if ch.FilePath != "B/Plain.m4b" {
			t.Fatalf("a chapter streams from %q after the move", ch.FilePath)
		}
	}
	// The same audio at a new path: the check is still current.
	if cc, err := c.GetCommunityChapters(ctx, lib.ID, "B/Plain.m4b"); err != nil || cc == nil || cc.Stale {
		t.Fatalf("moved check %+v, %v; want current", cc, err)
	}
	// A folder book whose files are renamed in place: same count, same length.
	const p = "Author/Series/01 - Title"
	folder := func(names ...string) *Book {
		bk := scannedBook(lib.ID, p)
		bk.Chapters = nil
		for i, n := range names {
			bk.Files = append(bk.Files, BookFile{RelPath: p + "/" + n, Seq: i, Duration: 1800})
		}
		return bk
	}
	if _, err := c.UpsertBook(ctx, folder("1.mp3", "2.mp3")); err != nil {
		t.Fatal(err)
	}
	fit := fittedFill(lib.ID, p, string(chapteralign.Fill))
	fit.Chapters = []metadata.Chapter{
		{Index: 0, Title: "One", FilePath: p + "/1.mp3", End: 1800},
		{Index: 1, Title: "Two", FileIndex: 1, FilePath: p + "/2.mp3", End: 1800, BookOffset: 1800},
	}
	save(t, c, fit)
	if _, err := c.UpsertBook(ctx, folder("01.mp3", "02.mp3")); err != nil {
		t.Fatal(err)
	}
	for _, ch := range mustBook(t, c, ctx, lib.ID, p).Chapters {
		if ch.FilePath == p+"/1.mp3" || ch.FilePath == p+"/2.mp3" {
			t.Fatalf("a chapter streams from %q after the files were renamed", ch.FilePath)
		}
	}
}

// A rescan that finds chapters of the book's own (the same audio, now tagged)
// makes a fill fitted against none stale.
func TestCommunityChaptersAfterTagging(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "A/Plain.m4b"
	if _, err := c.UpsertBook(ctx, chapterless(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	save(t, c, fittedFill(lib.ID, p, string(chapteralign.Fill)))
	tagged := chapterless(lib.ID, p)
	tagged.Chapters = []metadata.Chapter{
		{Index: 0, Title: "Mine 1", FilePath: p, End: 1200},
		{Index: 1, Title: "Mine 2", FilePath: p, Start: 1200, End: 3600, BookOffset: 1200},
	}
	if _, err := c.UpsertBook(ctx, tagged); err != nil {
		t.Fatal(err)
	}
	if b := mustBook(t, c, ctx, lib.ID, p); !slices.Equal(titlesOf(b), []string{"Mine 1", "Mine 2"}) || b.ChaptersSource != "" {
		t.Fatalf("after tagging: %q from %q", titlesOf(b), b.ChaptersSource)
	}
	if due, _ := c.DueChapterChecks(ctx, "2000-01-01T00:00:00Z", 10); len(due) != 1 {
		t.Fatalf("due %+v, want the retagged book", due)
	}
}

// A row indexed before the snapshot keeps its check current when the community's
// chapters first stand in (the snapshot written is what the check fitted).
func TestCommunityChaptersOnALegacyRow(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "A/Plain.m4b"
	if _, err := c.UpsertBook(ctx, chapterless(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE books SET scanned_chapters = '' WHERE rel_path = ?`, p); err != nil {
		t.Fatal(err)
	}
	save(t, c, fittedFill(lib.ID, p, string(chapteralign.Fill)))
	if b := mustBook(t, c, ctx, lib.ID, p); len(b.Chapters) != 3 || b.ChaptersSource != ChaptersFromCommunity {
		t.Fatalf("fitted: %q from %q", titlesOf(b), b.ChaptersSource)
	}
	if cc, err := c.GetCommunityChapters(ctx, lib.ID, p); err != nil || cc == nil || cc.Stale {
		t.Fatalf("check %+v, %v; want current", cc, err)
	}
	if due, _ := c.DueChapterChecks(ctx, "2000-01-01T00:00:00Z", 10); len(due) != 0 {
		t.Fatalf("due %+v, want none", due)
	}
}

// Health lists a book whose own chapters a current refine check would detail,
// until the admin picks a source; a stale check lists nothing.
func TestDetailedChaptersIssue(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "Author/Series/01 - Title"
	own := scannedBook(lib.ID, p)
	own.ASIN = "B000000001"
	if _, err := c.UpsertBook(ctx, own); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		t.Helper()
		counts, err := c.IssueCounts(ctx, []string{IssueDetailedChapters})
		if err != nil || len(counts) != 1 {
			t.Fatalf("%+v %v", counts, err)
		}
		return counts[0].Count
	}
	refine := fittedFill(lib.ID, p, string(chapteralign.Refine))
	save(t, c, refine)
	if n := count(); n != 1 {
		t.Fatalf("%d listed, want the refine", n)
	}
	longer := scannedBook(lib.ID, p)
	longer.ASIN, longer.Duration = "B000000001", 4000
	if _, err := c.UpsertBook(ctx, longer); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Fatalf("%d listed for a stale check, want none", n)
	}
	save(t, c, refine)
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{ChapterSource: ChaptersFromFiles}); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Fatalf("%d listed after a choice, want none", n)
	}
}

// A move is the same audio: the check stays current and its chapters follow the
// book to its new path.
func TestCommunityChaptersFollowAMove(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if _, err := c.UpsertBook(ctx, chapterless(lib.ID, "A/Plain.m4b")); err != nil {
		t.Fatal(err)
	}
	save(t, c, fittedFill(lib.ID, "A/Plain.m4b", string(chapteralign.Fill)))
	if err := c.MoveDurableState(ctx, lib.ID, "A/Plain.m4b", "B/Plain.m4b"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteBooksNotIn(ctx, lib.ID, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, chapterless(lib.ID, "B/Plain.m4b")); err != nil {
		t.Fatal(err)
	}
	b := mustBook(t, c, ctx, lib.ID, "B/Plain.m4b")
	if len(b.Chapters) != 3 || b.ChaptersSource != ChaptersFromCommunity || b.Chapters[1].FilePath != "B/Plain.m4b" {
		t.Fatalf("after the move: %+v from %q", b.Chapters, b.ChaptersSource)
	}
	if cc, _ := c.GetCommunityChapters(ctx, lib.ID, "B/Plain.m4b"); cc == nil || cc.Stale {
		t.Fatalf("moved check %+v", cc)
	}
}
