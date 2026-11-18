package catalog

import (
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/chapteralign"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// twoFiles is a folder book of two half-hour files with one chapter each (the
// scan's own: a file a chapter), sizes 10 and 20 bytes.
func twoFiles(libID int64, p string) *Book {
	return &Book{LibraryID: libID, RelPath: p, IsFolder: true, Title: "Book", Duration: 3600, ASIN: "B000000001",
		AddedAt: "2020-01-01T00:00:00Z",
		Files:   []BookFile{{RelPath: p + "/01.mp3", Seq: 0, Duration: 1800, Size: 10}, {RelPath: p + "/02.mp3", Seq: 1, Duration: 1800, Size: 20}},
		Chapters: []metadata.Chapter{{Index: 0, Title: "01", FilePath: p + "/01.mp3", End: 1800},
			{Index: 1, Title: "02", FileIndex: 1, FilePath: p + "/02.mp3", End: 1800, BookOffset: 1800}}}
}

// lengthsOf is files' lengths in whole milliseconds, as a list made on them
// gives them (placeOnFiles).
func lengthsOf(files []BookFile) []int64 {
	out := make([]int64, len(files))
	for i, f := range files {
		out[i] = msOf(f.Duration)
	}
	return out
}

// basisNow is the book page's basis for the book at p.
func basisNow(t *testing.T, c *Catalog, libID int64, p string) ChapterBasis {
	t.Helper()
	d, err := c.AdminBookDetail(t.Context(), libID, p)
	if err != nil {
		t.Fatal(err)
	}
	return d.ChapterBasis
}

// fourChapters is an edit of twoFiles: each file split in two.
var fourChapters = []EditedChapter{{0, "Prologue"}, {900.25, "One"}, {1800, "Two"}, {2700.5, "Three"}}

// placeOnFiles lays whole-book starts on a single file and on several, each
// chapter in the file its start falls in, to the next start or the file's
// end; a start a few milliseconds off a file's start is that file's first
// chapter; anything that doesn't fit is nil.
func TestPlaceOnFiles(t *testing.T) {
	t.Parallel()
	one := []BookFile{{RelPath: "A/B.m4b", Duration: 3600}}
	got := placeOnFiles([]metadata.ChapterListEntry{{Title: "A"}, {Title: "B", StartMS: 1000500}}, one, lengthsOf(one))
	want := []metadata.Chapter{
		{Index: 0, Title: "A", FilePath: "A/B.m4b", End: 1000.5},
		{Index: 1, Title: "B", FilePath: "A/B.m4b", Start: 1000.5, End: 3600, BookOffset: 1000.5},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("one file = %+v\nwant %+v", got, want)
	}
	files := []BookFile{{RelPath: "A/1.mp3", Duration: 1000.0004}, {RelPath: "A/2.mp3", Duration: 500}, {RelPath: "A/3.mp3", Duration: 700}}
	five := []metadata.ChapterListEntry{{Title: "A"}, {Title: "B", StartMS: 400000},
		{Title: "C", StartMS: 1000030}, {Title: "D", StartMS: 1500000}, {Title: "E", StartMS: 1900000}}
	got = placeOnFiles(five, files, lengthsOf(files))
	want = []metadata.Chapter{
		{Index: 0, Title: "A", FilePath: "A/1.mp3", End: 400},
		{Index: 1, Title: "B", FilePath: "A/1.mp3", Start: 400, End: 1000.0004, BookOffset: 400},
		// 30 ms past the second file's start (a rounding): its first chapter.
		{Index: 2, Title: "C", FileIndex: 1, FilePath: "A/2.mp3", End: 500, BookOffset: 1000},
		{Index: 3, Title: "D", FileIndex: 2, FilePath: "A/3.mp3", End: 400, BookOffset: 1500},
		{Index: 4, Title: "E", FileIndex: 2, FilePath: "A/3.mp3", Start: 400, End: 700, BookOffset: 1900},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("three files = %+v\nwant %+v", got, want)
	}
	// The files read longer now than the list's lengths (another probe of the
	// same files): each chapter stays at its place in its file, its book offset
	// on the files as they are now.
	longer := []BookFile{{RelPath: "A/1.mp3", Duration: 1001.5}, {RelPath: "A/2.mp3", Duration: 500.25}, {RelPath: "A/3.mp3", Duration: 700}}
	got = placeOnFiles(five, longer, lengthsOf(files))
	want = []metadata.Chapter{
		{Index: 0, Title: "A", FilePath: "A/1.mp3", End: 400},
		{Index: 1, Title: "B", FilePath: "A/1.mp3", Start: 400, End: 1001.5, BookOffset: 400},
		{Index: 2, Title: "C", FileIndex: 1, FilePath: "A/2.mp3", End: 500.25, BookOffset: 1001.5},
		{Index: 3, Title: "D", FileIndex: 2, FilePath: "A/3.mp3", End: 400, BookOffset: 1501.75},
		{Index: 4, Title: "E", FileIndex: 2, FilePath: "A/3.mp3", Start: 400, End: 700, BookOffset: 1901.75},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("longer files = %+v\nwant %+v", got, want)
	}
	// A file shorter now than where a chapter starts in it: they no longer fit.
	shorter := slices.Clone(files)
	shorter[2].Duration = 350
	if got := placeOnFiles(five, shorter, lengthsOf(files)); got != nil {
		t.Errorf("a chapter past its file's end now: %+v, want nil", got)
	}
	for name, entries := range map[string][]metadata.ChapterListEntry{
		"no chapters":          nil,
		"a file with no start": {{Title: "A"}, {Title: "D", StartMS: 1500000}},
		"far off a file start": {{Title: "A"}, {Title: "C", StartMS: 1000100}, {Title: "D", StartMS: 1500000}},
		"out of order":         {{Title: "A"}, {Title: "C", StartMS: 1000000}, {Title: "B", StartMS: 400000}, {Title: "D", StartMS: 1500000}},
		"past the end":         {{Title: "A"}, {Title: "C", StartMS: 1000000}, {Title: "D", StartMS: 1500000}, {Title: "Z", StartMS: 2200000}},
	} {
		if got := placeOnFiles(entries, files, lengthsOf(files)); got != nil {
			t.Errorf("%s: %+v, want nil", name, got)
		}
	}
}

// snapToFiles gives each file the nearest chapter start in reach that no
// file before it took (server#410): a file under 100 ms (twice the reach)
// never takes the chapter of the file beside it, and one with no chapter of
// its own is no fit.
func TestSnapToFilesShortFile(t *testing.T) {
	t.Parallel()
	fileStarts := []int64{0, 1000, 1040} // the second file is 40 ms long
	starts := []int64{0, 1002, 1039}
	if fixed, ok := snapToFiles(starts, fileStarts, 2000); !ok || !slices.Equal(starts, []int64{0, 1000, 1040}) ||
		!slices.Equal(fixed, []bool{true, true, true}) {
		t.Fatalf("each file its own chapter = %v, want [0 1000 1040]", starts)
	}
	// The 40 ms file has no chapter: the one before it is no stand-in.
	starts = []int64{0, 1000}
	if _, ok := snapToFiles(starts, fileStarts, 2000); ok {
		t.Fatalf("a file with no chapter fitted: %v", starts)
	}
	// Nearest wins, not first: 990 is the second file's, 1035 the third's.
	starts = []int64{0, 990, 1035}
	if _, ok := snapToFiles(starts, []int64{0, 1000, 1030}, 2000); !ok || !slices.Equal(starts, []int64{0, 1000, 1030}) {
		t.Fatalf("nearest start = %v, want [0 1000 1030]", starts)
	}
	// A file of no length (one the scan read no length for) starts where the
	// next file does: the one chapter there starts both, as in the console's
	// editor (validateDraft), and it is placed in the file with the audio.
	starts = []int64{0, 1003}
	if _, ok := snapToFiles(starts, []int64{0, 1000, 1000}, 1500); !ok || !slices.Equal(starts, []int64{0, 1000}) {
		t.Fatalf("a file of no length = %v, want [0 1000]", starts)
	}
	files := []BookFile{{RelPath: "A/1.mp3", Duration: 1000}, {RelPath: "A/2.mp3"}, {RelPath: "A/3.mp3", Duration: 500}}
	entries, err := checkChapterEdit([]EditedChapter{{0, "A"}, {1000, "B"}}, files)
	if err != nil {
		t.Fatalf("an edit over a file of no length: %v", err)
	}
	want := []metadata.Chapter{{Index: 0, Title: "A", FilePath: "A/1.mp3", End: 1000},
		{Index: 1, Title: "B", FileIndex: 2, FilePath: "A/3.mp3", End: 500, BookOffset: 1000}}
	if got := placeOnFiles(entries, files, lengthsOf(files)); !slices.Equal(got, want) {
		t.Fatalf("placed over a file of no length = %+v\nwant %+v", got, want)
	}
}

// checkChapterEdit refuses each rule's breach with its detail, and stores a
// good edit's starts in milliseconds, file starts exact, titles trimmed.
func TestCheckChapterEdit(t *testing.T) {
	t.Parallel()
	files := twoFiles(1, "A").Files
	long := make([]EditedChapter, metadata.MaxChapterListEntries+1)
	for i := range long {
		long[i] = EditedChapter{Start: float64(i), Title: "x"}
	}
	title201 := string(make([]byte, 201))
	for name, c := range map[string]struct {
		chapters []EditedChapter
		detail   string
	}{
		"none":                         {nil, ChapterEditEmpty},
		"too many":                     {long, ChapterEditTooMany},
		"before 0":                     {[]EditedChapter{{0, "A"}, {-1, "B"}, {1800, "C"}}, ChapterEditOutOfRange},
		"at the end":                   {[]EditedChapter{{0, "A"}, {1800, "C"}, {3600, "D"}}, ChapterEditOutOfRange},
		"rounds to the end":            {[]EditedChapter{{0, "A"}, {1800, "C"}, {3599.9996, "D"}}, ChapterEditOutOfRange},
		"empty title":                  {[]EditedChapter{{0, "  "}, {1800, "C"}}, ChapterEditTitle},
		"invisible title":              {[]EditedChapter{{0, "\u200b\u2800"}, {1800, "C"}}, ChapterEditTitle},
		"long title":                   {[]EditedChapter{{0, "A"}, {1800, title201}}, ChapterEditTitle},
		"control character":            {[]EditedChapter{{0, "A\tB"}, {1800, "C"}}, ChapterEditTitle},
		"no file start":                {[]EditedChapter{{0, "A"}, {1700, "B"}}, ChapterEditFileStart},
		"first not at 0":               {[]EditedChapter{{0.2, "A"}, {1800, "B"}}, ChapterEditFileStart},
		"out of order":                 {[]EditedChapter{{0, "A"}, {1800, "C"}, {900, "B"}}, ChapterEditOrder},
		"under a second":               {[]EditedChapter{{0, "A"}, {0.999, "B"}, {1800, "C"}}, ChapterEditOrder},
		"under a second before a file": {[]EditedChapter{{0, "A"}, {1799.5, "B"}, {1800, "C"}}, ChapterEditOrder},
		"a file's start twice":         {[]EditedChapter{{0, "A"}, {1800, "B"}, {1800, "C"}}, ChapterEditOrder},
	} {
		_, err := checkChapterEdit(c.chapters, files)
		if ce, ok := errors.AsType[*ChapterEditError](err); !ok || ce.Detail != c.detail {
			t.Errorf("%s: %v, want %s", name, err, c.detail)
		}
	}
	got, err := checkChapterEdit([]EditedChapter{{0, " Prologue "}, {1.0, "One"}, {1800.02, "Two"}}, files)
	want := []metadata.ChapterListEntry{{Title: "Prologue", LengthMS: 1000}, {Title: "One", StartMS: 1000, LengthMS: 1799000},
		{Title: "Two", StartMS: 1800000, LengthMS: 1800000}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("good edit = %+v, %v\nwant %+v", got, err, want)
	}
}

// An edit is the first source: over the community's (a fill), a choice and the
// files'; the player sees source "edited" with the edit's titles and book
// offsets; the chapter renames go dormant while it applies and come back on
// Revert, which brings back the source below it. Saving again replaces it.
func TestChapterEditPrecedence(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "A/Book"
	upsert(t, c, twoFiles(lib.ID, p))
	fit := fittedFill(lib.ID, p, string(chapteralign.Fill))
	fit.Chapters = []metadata.Chapter{{Index: 0, Title: "C1", FilePath: p + "/01.mp3", End: 1800},
		{Index: 1, Title: "C2", FileIndex: 1, FilePath: p + "/02.mp3", End: 1800, BookOffset: 1800}}
	save(t, c, fit)
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{ChapterSet: map[int]string{0: "Renamed"}}); err != nil {
		t.Fatal(err)
	}
	if b := mustBook(t, c, ctx, lib.ID, p); b.ChaptersSource != ChaptersFromCommunity || !slices.Equal(titlesOf(b), []string{"Renamed", "C2"}) {
		t.Fatalf("before the edit: %q from %q", titlesOf(b), b.ChaptersSource)
	}
	if err := c.SaveChapterEdit(ctx, lib.ID, p, basisNow(t, c, lib.ID, p), fourChapters, 0); err != nil {
		t.Fatal(err)
	}
	b := mustBook(t, c, ctx, lib.ID, p)
	if b.ChaptersSource != ChaptersFromEdited || !slices.Equal(titlesOf(b), []string{"Prologue", "One", "Two", "Three"}) {
		t.Fatalf("edited: %q from %q", titlesOf(b), b.ChaptersSource)
	}
	if ch := b.Chapters[3]; ch.FilePath != p+"/02.mp3" || ch.FileIndex != 1 || ch.Start != 900.5 || ch.End != 1800 || ch.BookOffset != 2700.5 {
		t.Fatalf("chapter 3 = %+v", ch)
	}
	if ch := b.Chapters[1]; ch.FilePath != p+"/01.mp3" || ch.Start != 900.25 || ch.End != 1800 {
		t.Fatalf("chapter 1 = %+v", ch)
	}
	// A choice made while it applies changes nothing until it is reverted.
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{ChapterSource: ChaptersFromFiles}); err != nil {
		t.Fatal(err)
	}
	if b := mustBook(t, c, ctx, lib.ID, p); b.ChaptersSource != ChaptersFromEdited {
		t.Fatalf("a files choice over the edit: %q", b.ChaptersSource)
	}
	// A rename over an edited list is refused: its titles are the edit's.
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{ChapterSet: map[int]string{1: "x"}}); !errors.Is(err, ErrInvalidOverride) {
		t.Fatalf("a rename over the edit = %v, want invalid", err)
	}
	// A rescan keeps it.
	upsert(t, c, twoFiles(lib.ID, p))
	if b := mustBook(t, c, ctx, lib.ID, p); b.ChaptersSource != ChaptersFromEdited || len(b.Chapters) != 4 {
		t.Fatalf("after a rescan: %q from %q", titlesOf(b), b.ChaptersSource)
	}
	// Saved again: the new list replaces it.
	if err := c.SaveChapterEdit(ctx, lib.ID, p, basisNow(t, c, lib.ID, p), []EditedChapter{{0, "Whole"}, {1800, "Second"}}, 0); err != nil {
		t.Fatal(err)
	}
	if b := mustBook(t, c, ctx, lib.ID, p); !slices.Equal(titlesOf(b), []string{"Whole", "Second"}) {
		t.Fatalf("saved again: %q", titlesOf(b))
	}
	// Reverted: the files choice made meanwhile, with the rename back (it is
	// keyed on the first file's start, which the files' chapter has too).
	if removed, err := c.RevertChapterEdit(ctx, lib.ID, p); err != nil || !removed {
		t.Fatalf("revert = %v, %v", removed, err)
	}
	if b := mustBook(t, c, ctx, lib.ID, p); b.ChaptersSource != "" || !slices.Equal(titlesOf(b), []string{"Renamed", "02"}) {
		t.Fatalf("reverted: %q from %q", titlesOf(b), b.ChaptersSource)
	}
	if removed, err := c.RevertChapterEdit(ctx, lib.ID, p); err != nil || removed {
		t.Fatalf("a second revert = %v, %v; want nothing removed", removed, err)
	}
	if _, err := c.RevertChapterEdit(ctx, lib.ID, "No/Book"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revert with no book = %v, want ErrNotFound", err)
	}
}

// An edit made on other files is refused (ErrChaptersBasis); one whose book's
// files change goes dormant (the next source's chapters, the page says
// dormant) and applies again once the files are back; in a podcast library
// none is made.
func TestChapterEditDormant(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "A/Book"
	upsert(t, c, twoFiles(lib.ID, p))
	basis := basisNow(t, c, lib.ID, p)
	want := []metadata.ChapterListFile{{Path: "01.mp3", Size: 10, DurationMS: 1800000}, {Path: "02.mp3", Size: 20, DurationMS: 1800000}}
	if !slices.Equal(basis.Files, want) {
		t.Fatalf("basis = %+v", basis)
	}
	stale := ChapterBasis{Files: slices.Clone(basis.Files)}
	stale.Files[1].Size = 21
	if err := c.SaveChapterEdit(ctx, lib.ID, p, stale, fourChapters, 0); !errors.Is(err, ErrChaptersBasis) {
		t.Fatalf("another basis = %v, want ErrChaptersBasis", err)
	}
	if err := c.SaveChapterEdit(ctx, lib.ID, "No/Book", basis, fourChapters, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no book = %v, want ErrNotFound", err)
	}
	if err := c.SaveChapterEdit(ctx, lib.ID, p, basis, fourChapters, 0); err != nil {
		t.Fatal(err)
	}
	// The second file replaced (another size): dormant.
	other := twoFiles(lib.ID, p)
	other.Files[1].Size = 21
	upsert(t, c, other)
	if b := mustBook(t, c, ctx, lib.ID, p); b.ChaptersSource != "" || !slices.Equal(titlesOf(b), []string{"01", "02"}) {
		t.Fatalf("other files: %q from %q", titlesOf(b), b.ChaptersSource)
	}
	d, err := c.AdminBookDetail(ctx, lib.ID, p)
	if err != nil || d.ChapterEdit == nil || d.ChapterEdit.State != ChapterEditDormant || d.Book.Edited {
		t.Fatalf("dormant page: %+v, %v", d.ChapterEdit, err)
	}
	// The files back: it applies again.
	upsert(t, c, twoFiles(lib.ID, p))
	if b := mustBook(t, c, ctx, lib.ID, p); b.ChaptersSource != ChaptersFromEdited || len(b.Chapters) != 4 {
		t.Fatalf("files back: %q from %q", titlesOf(b), b.ChaptersSource)
	}
	// The same files in another play order: the chapters move with them.
	swapped := twoFiles(lib.ID, p)
	swapped.Files[0].RelPath, swapped.Files[1].RelPath = p+"/02.mp3", p+"/01.mp3"
	swapped.Files[0].Size, swapped.Files[1].Size = 20, 10
	upsert(t, c, swapped)
	if b := mustBook(t, c, ctx, lib.ID, p); b.ChaptersSource != ChaptersFromEdited ||
		!slices.Equal(titlesOf(b), []string{"Two", "Three", "Prologue", "One"}) || b.Chapters[2].FilePath != p+"/01.mp3" {
		t.Fatalf("reordered: %q from %q", titlesOf(b), b.ChaptersSource)
	}

	pods, err := c.CreateLibrary(ctx, Library{Name: "P", Root: "/tmp/p", Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	upsert(t, c, twoFiles(pods.ID, p))
	if err := c.SaveChapterEdit(ctx, pods.ID, p, basis, fourChapters, 0); !errors.Is(err, ErrNotBookLibrary) {
		t.Fatalf("a podcast library = %v, want ErrNotBookLibrary", err)
	}
}

// The same files read again keep the edit: lengths a probe reads a little
// otherwise (each chapter at its place in its file, book offsets on the files
// now), and a single-file book's file renamed (its edit moved with it).
func TestChapterEditSameFilesReadAgain(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "A/Book"
	upsert(t, c, twoFiles(lib.ID, p))
	if err := c.SaveChapterEdit(ctx, lib.ID, p, basisNow(t, c, lib.ID, p), fourChapters, 0); err != nil {
		t.Fatal(err)
	}
	reread := twoFiles(lib.ID, p)
	reread.Files[0].Duration, reread.Duration = 1801.5, 3601.5
	upsert(t, c, reread)
	b := mustBook(t, c, ctx, lib.ID, p)
	if b.ChaptersSource != ChaptersFromEdited || len(b.Chapters) != 4 {
		t.Fatalf("read again: %q from %q", titlesOf(b), b.ChaptersSource)
	}
	if ch := b.Chapters[1]; ch.Start != 900.25 || ch.End != 1801.5 {
		t.Fatalf("chapter 1 = %+v", ch)
	}
	if ch := b.Chapters[3]; ch.FileIndex != 1 || ch.Start != 900.5 || ch.BookOffset != 2702 {
		t.Fatalf("chapter 3 = %+v", ch)
	}

	const one = "A/Old title.m4b"
	upsert(t, c, &Book{LibraryID: lib.ID, RelPath: one, Title: "Single", Duration: 3600, Size: 99, AddedAt: "2020-01-01T00:00:00Z",
		Chapters: []metadata.Chapter{{Index: 0, Title: "Whole", FilePath: one, End: 3600}}})
	if err := c.SaveChapterEdit(ctx, lib.ID, one, basisNow(t, c, lib.ID, one), []EditedChapter{{0, "A"}, {1800, "B"}}, 0); err != nil {
		t.Fatal(err)
	}
	const renamed = "A/New title.m4b"
	if err := c.MoveDurableState(ctx, lib.ID, one, renamed); err != nil {
		t.Fatal(err)
	}
	upsert(t, c, &Book{LibraryID: lib.ID, RelPath: renamed, Title: "Single", Duration: 3600, Size: 99, AddedAt: "2020-01-01T00:00:00Z",
		Chapters: []metadata.Chapter{{Index: 0, Title: "Whole", FilePath: renamed, End: 3600}}})
	if b := mustBook(t, c, ctx, lib.ID, renamed); b.ChaptersSource != ChaptersFromEdited ||
		!slices.Equal(titlesOf(b), []string{"A", "B"}) || b.Chapters[1].FilePath != renamed {
		t.Fatalf("renamed: %q from %q", titlesOf(b), b.ChaptersSource)
	}
}

// The book page names the edit (applied, by whom, when) and the admin row
// counts it as an edit; Health's detailed-chapters issue is settled by it.
func TestChapterEditOnTheBookPage(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	admin := seedNamedUser(t, c, "admin")
	const p = "A/Book"
	upsert(t, c, twoFiles(lib.ID, p))
	refine := fittedFill(lib.ID, p, string(chapteralign.Refine))
	save(t, c, refine)
	detailed := func() int {
		t.Helper()
		counts, err := c.IssueCounts(ctx, []string{IssueDetailedChapters})
		if err != nil || len(counts) != 1 {
			t.Fatalf("%+v %v", counts, err)
		}
		return counts[0].Count
	}
	if n := detailed(); n != 1 {
		t.Fatalf("%d listed before the edit, want 1", n)
	}
	if d, err := c.AdminBookDetail(ctx, lib.ID, p); err != nil || d.ChapterEdit != nil || d.Book.Edited {
		t.Fatalf("no edit: %+v, edited %v, %v", d.ChapterEdit, d.Book.Edited, err)
	}
	if err := c.SaveChapterEdit(ctx, lib.ID, p, basisNow(t, c, lib.ID, p), fourChapters, admin); err != nil {
		t.Fatal(err)
	}
	d, err := c.AdminBookDetail(ctx, lib.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	if e := d.ChapterEdit; e == nil || e.State != ChapterEditApplied || e.EditedBy == nil || *e.EditedBy != "admin" || e.EditedAt == "" ||
		d.ChapterSource != ChaptersFromEdited || !d.Book.Edited || len(d.Chapters) != 4 {
		t.Fatalf("page: %+v source %q edited %v", d.ChapterEdit, d.ChapterSource, d.Book.Edited)
	}
	if n := detailed(); n != 0 {
		t.Fatalf("%d listed after the edit, want none", n)
	}
	if _, err := c.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, admin); err != nil {
		t.Fatal(err)
	}
	if d, err := c.AdminBookDetail(ctx, lib.ID, p); err != nil || d.ChapterEdit == nil || d.ChapterEdit.EditedBy != nil {
		t.Fatalf("editor gone: %+v, %v", d.ChapterEdit, err)
	}
}

// The edit is the book's: it follows a move (the same audio) and replaces
// what the new path held, while a join (other audio) never copies it.
func TestChapterEditFollowsAMoveNotAJoin(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	upsert(t, c, twoFiles(lib.ID, "A/Book"))
	if err := c.SaveChapterEdit(ctx, lib.ID, "A/Book", basisNow(t, c, lib.ID, "A/Book"), fourChapters, 0); err != nil {
		t.Fatal(err)
	}
	// A stale edit an earlier book left at the new path.
	if _, err := c.db.ExecContext(ctx, `INSERT INTO chapter_edits(library_id, path, list, updated_at) VALUES(?, 'B/Book', '{}', 't')`, lib.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.MoveDurableState(ctx, lib.ID, "A/Book", "B/Book"); err != nil {
		t.Fatal(err)
	}
	upsert(t, c, twoFiles(lib.ID, "B/Book"))
	b := mustBook(t, c, ctx, lib.ID, "B/Book")
	if b.ChaptersSource != ChaptersFromEdited || len(b.Chapters) != 4 || b.Chapters[3].FilePath != "B/Book/02.mp3" {
		t.Fatalf("moved: %q from %q", titlesOf(b), b.ChaptersSource)
	}

	// Joined into a folder: the part's edit stays with the part's path.
	upsert(t, c, twoFiles(lib.ID, "J/CD1"))
	if err := c.SaveChapterEdit(ctx, lib.ID, "J/CD1", basisNow(t, c, lib.ID, "J/CD1"), fourChapters, 0); err != nil {
		t.Fatal(err)
	}
	if err := c.JoinDurableState(ctx, lib.ID, "J", []JoinPart{{Path: "J/CD1", Last: true}}, 3600); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chapter_edits WHERE path = 'J'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("joined book's edits = %d, %v; want none", n, err)
	}
}

// A file under a second long is editable (server#410): two files' starts are
// fixed, so only a chapter of the admin's own needs the second apart. A last
// file of no length (one the scan read no length for) starts no chapter: the
// edit holds on the files before it, and goes dormant once that file reads a
// length (its audio in no chapter of the edit).
func TestChapterEditShortAndEmptyFiles(t *testing.T) {
	t.Parallel()
	short := []BookFile{{RelPath: "A/1.mp3", Duration: 600}, {RelPath: "A/2.mp3", Duration: 0.4}, {RelPath: "A/3.mp3", Duration: 600}}
	entries, err := checkChapterEdit([]EditedChapter{{0, "A"}, {600, "Sting"}, {600.4, "B"}, {900, "C"}}, short)
	if err != nil {
		t.Fatalf("a 400 ms file: %v", err)
	}
	if got := placeOnFiles(entries, short, lengthsOf(short)); len(got) != 4 || got[1].FileIndex != 1 || got[1].End != 0.4 ||
		got[2].FileIndex != 2 || got[2].BookOffset != 600.4 {
		t.Fatalf("placed over a 400 ms file = %+v", got)
	}
	// A chapter of the admin's own still keeps the second from a file's start.
	if _, err := checkChapterEdit([]EditedChapter{{0, "A"}, {600, "Sting"}, {600.4, "B"}, {600.9, "C"}}, short); err == nil {
		t.Fatal("a chapter 500 ms after a file's start was taken")
	}

	trailing := []BookFile{{RelPath: "A/1.mp3", Duration: 600}, {RelPath: "A/2.mp3", Duration: 600}, {RelPath: "A/3.mp3"}}
	entries, err = checkChapterEdit([]EditedChapter{{0, "A"}, {300, "B"}, {600, "C"}}, trailing)
	if err != nil {
		t.Fatalf("a last file of no length: %v", err)
	}
	want := []metadata.Chapter{{Index: 0, Title: "A", FilePath: "A/1.mp3", End: 300},
		{Index: 1, Title: "B", FilePath: "A/1.mp3", Start: 300, End: 600, BookOffset: 300},
		{Index: 2, Title: "C", FileIndex: 1, FilePath: "A/2.mp3", End: 600, BookOffset: 600}}
	listed := lengthsOf(trailing)
	if got := placeOnFiles(entries, trailing, listed); !slices.Equal(got, want) {
		t.Fatalf("placed over a last file of no length = %+v\nwant %+v", got, want)
	}
	read := slices.Clone(trailing)
	read[2].Duration = 120
	if got := placeOnFiles(entries, read, listed); got != nil {
		t.Fatalf("a file the edit had no length for reads one now: %+v, want dormant", got)
	}
	// One in the middle the same: no chapter of the edit holds its audio now.
	mid := []BookFile{{RelPath: "A/1.mp3", Duration: 600}, {RelPath: "A/2.mp3"}, {RelPath: "A/3.mp3", Duration: 600}}
	entries, err = checkChapterEdit([]EditedChapter{{0, "A"}, {600, "B"}}, mid)
	if err != nil {
		t.Fatal(err)
	}
	listed = lengthsOf(mid)
	mid[1].Duration = 30
	if got := placeOnFiles(entries, mid, listed); got != nil {
		t.Fatalf("a middle file the edit had no length for reads one now: %+v, want dormant", got)
	}
}

// A one-file folder's file renamed in place keeps the folder's edit, matched
// by its size as a loose file is (server#410); another size is other audio.
func TestChapterEditOneFileFolderRenamed(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "A/Book"
	folder := func(name string, size int64) *Book {
		f := p + "/" + name
		return &Book{LibraryID: lib.ID, RelPath: p, IsFolder: true, Title: "Book", Duration: 3600, AddedAt: "2020-01-01T00:00:00Z",
			Files:    []BookFile{{RelPath: f, Duration: 3600, Size: size}},
			Chapters: []metadata.Chapter{{Index: 0, Title: "Whole", FilePath: f, End: 3600}}}
	}
	upsert(t, c, folder("Old name.m4b", 99))
	if err := c.SaveChapterEdit(ctx, lib.ID, p, basisNow(t, c, lib.ID, p), []EditedChapter{{0, "A"}, {1800, "B"}}, 0); err != nil {
		t.Fatal(err)
	}
	upsert(t, c, folder("New name.m4b", 99))
	if b := mustBook(t, c, ctx, lib.ID, p); b.ChaptersSource != ChaptersFromEdited ||
		!slices.Equal(titlesOf(b), []string{"A", "B"}) || b.Chapters[1].FilePath != p+"/New name.m4b" {
		t.Fatalf("renamed in its folder: %q from %q", titlesOf(b), b.ChaptersSource)
	}
	upsert(t, c, folder("New name.m4b", 100))
	if b := mustBook(t, c, ctx, lib.ID, p); b.ChaptersSource != "" {
		t.Fatalf("another file: %q from %q, want the files'", titlesOf(b), b.ChaptersSource)
	}
}

// seedDetect is an open chapters.detect job for the book at p in state, as
// the api queues it (bridge.DedupeKey); its id.
func seedDetect(t *testing.T, c *Catalog, libID int64, p, state string) int64 {
	t.Helper()
	res, err := c.db.ExecContext(t.Context(),
		`INSERT INTO bridge_jobs(kind, library_id, rel_path, state, params, dedupe_key, created_at, updated_at)
		 VALUES(?, ?, ?, ?, '{}', ?, 't', 't')`,
		bridge.KindDetectChapters, libID, p, state, bridge.DedupeKey(bridge.KindDetectChapters, &libID, &p))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// jobState is a job's state, cancel_requested and error.
func jobState(t *testing.T, c *Catalog, id int64) (state string, cancel bool, reason string) {
	t.Helper()
	var e sql.NullString
	if err := c.db.QueryRowContext(t.Context(), `SELECT state, cancel_requested, error FROM bridge_jobs WHERE id = ?`, id).
		Scan(&state, &cancel, &e); err != nil {
		t.Fatal(err)
	}
	return state, cancel, e.String
}

// Saving an edit cancels the book's open chapters.detect job, and so does
// choosing its chapter source (server#410): the list it finds would never be
// used. A queued one ends at once, a working one is asked to stop, each with
// the server's reason; another book's job, and setting the source back to
// automatic, cancel nothing.
func TestChapterEditCancelsDetect(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	upsert(t, c, twoFiles(lib.ID, "A/Book"))
	upsert(t, c, twoFiles(lib.ID, "B/Book"))
	queued := seedDetect(t, c, lib.ID, "A/Book", "queued")
	other := seedDetect(t, c, lib.ID, "B/Book", "queued")
	if err := c.SaveChapterEdit(ctx, lib.ID, "A/Book", basisNow(t, c, lib.ID, "A/Book"), fourChapters, 0); err != nil {
		t.Fatal(err)
	}
	if st, _, why := jobState(t, c, queued); st != "cancelled" || why != bridge.CancelChaptersEdited {
		t.Fatalf("queued detect after an edit: %s %q", st, why)
	}
	if st, cancel, _ := jobState(t, c, other); st != "queued" || cancel {
		t.Fatalf("another book's detect: %s cancel %v", st, cancel)
	}
	running := seedDetect(t, c, lib.ID, "A/Book", "running")
	if err := c.SaveChapterEdit(ctx, lib.ID, "A/Book", basisNow(t, c, lib.ID, "A/Book"), fourChapters, 0); err != nil {
		t.Fatal(err)
	}
	if st, cancel, why := jobState(t, c, running); st != "running" || !cancel || why != bridge.CancelChaptersEdited {
		t.Fatalf("working detect after an edit: %s cancel %v %q", st, cancel, why)
	}

	if err := c.EditBook(ctx, lib.ID, "B/Book", BookEdit{ChapterSource: ChapterSourceAuto}); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := jobState(t, c, other); st != "queued" {
		t.Fatalf("detect after automatic: %s", st)
	}
	if err := c.EditBook(ctx, lib.ID, "B/Book", BookEdit{ChapterSource: ChaptersFromFiles}); err != nil {
		t.Fatal(err)
	}
	if st, _, why := jobState(t, c, other); st != "cancelled" || why != bridge.CancelChaptersChosen {
		t.Fatalf("queued detect after a choice: %s %q", st, why)
	}
}

// DetectRefused names a chosen source and an applied edit, and lets a
// detection through with neither or with a dormant edit.
func TestDetectRefused(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "A/Book"
	upsert(t, c, twoFiles(lib.ID, p))
	if err := c.DetectRefused(ctx, lib.ID, p); err != nil {
		t.Fatalf("no choice, no edit: %v", err)
	}
	if err := c.DetectRefused(ctx, lib.ID, "No/Book"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no book: %v, want ErrNotFound", err)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{ChapterSource: ChaptersFromCommunity}); err != nil {
		t.Fatal(err)
	}
	if err := c.DetectRefused(ctx, lib.ID, p); !errors.Is(err, bridge.ErrChaptersChosen) {
		t.Fatalf("a choice: %v, want ErrChaptersChosen", err)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{ChapterSource: ChapterSourceAuto}); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveChapterEdit(ctx, lib.ID, p, basisNow(t, c, lib.ID, p), fourChapters, 0); err != nil {
		t.Fatal(err)
	}
	if err := c.DetectRefused(ctx, lib.ID, p); !errors.Is(err, bridge.ErrChaptersEdited) {
		t.Fatalf("an applied edit: %v, want ErrChaptersEdited", err)
	}
	other := twoFiles(lib.ID, p)
	other.Files[1].Size = 21
	upsert(t, c, other)
	if err := c.DetectRefused(ctx, lib.ID, p); err != nil {
		t.Fatalf("a dormant edit: %v", err)
	}
}
