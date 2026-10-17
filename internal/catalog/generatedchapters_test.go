package catalog

import (
	"slices"
	"sync/atomic"
	"testing"

	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-server/internal/chapteralign"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// generatedList is a chapters.json for one file of an hour named name (as the
// Bridge names a single-file book's file) of size bytes, with three chapters.
func generatedList(name string, size int64, titles ...string) *metadata.ChapterList {
	if len(titles) == 0 {
		titles = []string{"Chapter 1", "Chapter 2", "Chapter 3"}
	}
	return &metadata.ChapterList{Version: 1, Generator: "audiosilo-bridge", From: "narration",
		Files: []metadata.ChapterListFile{{Path: name, Size: size, DurationMS: 3600000}},
		Chapters: []metadata.ChapterListEntry{
			{Title: titles[0], StartMS: 0, LengthMS: 1000000},
			{Title: titles[1], StartMS: 1000000, LengthMS: 1500000},
			{Title: titles[2], StartMS: 2500000, LengthMS: 1100000},
		}}
}

// plainWith is the chapterless single-file book at path (an hour, 5000 bytes)
// as a scan reads it with list beside it (nil: none).
func plainWith(libID int64, path string, list *metadata.ChapterList) *Book {
	b := chapterless(libID, path)
	b.Size = 5000
	if list != nil {
		b.Attachments = AttachmentsReading{Sig: "chapters.json:1:1", Files: metadata.AttachmentFiles{Chapters: list}}
	}
	return b
}

func upsert(t *testing.T, c *Catalog, b *Book) {
	t.Helper()
	if _, err := c.UpsertBook(t.Context(), b); err != nil {
		t.Fatal(err)
	}
}

// A chapterless book takes the chapters found in its narration (fitted onto
// its file, source generated on the player's book, the admin's row and book
// page); a rescan keeps them, a rename of one holds, and once the file is gone
// the scan's own (none) come back.
func TestGeneratedChaptersApply(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "A/Plain.mp3"
	upsert(t, c, plainWith(lib.ID, p, generatedList("Plain.mp3", 5000)))
	b := mustBook(t, c, ctx, lib.ID, p)
	if !slices.Equal(titlesOf(b), []string{"Chapter 1", "Chapter 2", "Chapter 3"}) || b.ChaptersSource != ChaptersFromGenerated {
		t.Fatalf("generated: %q from %q", titlesOf(b), b.ChaptersSource)
	}
	if ch := b.Chapters[1]; ch.FilePath != p || ch.Start != 1000 || ch.End != 2500 || ch.BookOffset != 1000 {
		t.Fatalf("chapter 2 = %+v", ch)
	}
	if b.Generated {
		t.Errorf("chapters alone are no generated content: generated = true")
	}
	d, err := c.AdminBookDetail(ctx, lib.ID, p)
	if err != nil || d.ChapterSource != ChaptersFromGenerated || d.ChapterChoice != "" {
		t.Fatalf("detail = %q/%q, %v", d.ChapterSource, d.ChapterChoice, err)
	}
	page, err := c.ListAdminBooks(ctx, AdminListOptions{Filter: BookFilter{LibraryID: lib.ID}})
	if err != nil || len(page.Books) != 1 || page.Books[0].ChaptersSource != ChaptersFromGenerated {
		t.Fatalf("row = %+v, %v", page, err)
	}

	if err := c.EditBook(ctx, lib.ID, p, BookEdit{ChapterSet: map[int]string{1: "The Storm"}}); err != nil {
		t.Fatal(err)
	}
	upsert(t, c, plainWith(lib.ID, p, generatedList("Plain.mp3", 5000)))
	if b = mustBook(t, c, ctx, lib.ID, p); !slices.Equal(titlesOf(b), []string{"Chapter 1", "The Storm", "Chapter 3"}) ||
		b.ChaptersSource != ChaptersFromGenerated {
		t.Fatalf("after a rescan: %q from %q", titlesOf(b), b.ChaptersSource)
	}

	// The file is gone (the Bridge's attachments rescan, or a scan).
	if err := c.SetAttachments(ctx, lib.ID, map[string]AttachmentsReading{p: {}}); err != nil {
		t.Fatal(err)
	}
	if b = mustBook(t, c, ctx, lib.ID, p); len(b.Chapters) != 0 || b.ChaptersSource != "" {
		t.Fatalf("after the file went: %q from %q", titlesOf(b), b.ChaptersSource)
	}
}

// A list stands in only for the files it was found on, beside a book with no
// chapters of its own, with nothing chosen, and when it fits the audio; else the
// scan's chapters stay.
func TestGeneratedChaptersRefused(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	long := generatedList("Plain.mp3", 5000)
	long.Chapters[2].LengthMS = 4700000 // the list runs two hours: other audio
	for name, b := range map[string]*Book{
		"another size": plainWith(lib.ID, "A/Plain.mp3", generatedList("Plain.mp3", 4999)),
		"another name": plainWith(lib.ID, "B/Plain.mp3", generatedList("Other.mp3", 5000)),
		"other audio":  plainWith(lib.ID, "C/Plain.mp3", long),
	} {
		upsert(t, c, b)
		if got := mustBook(t, c, ctx, lib.ID, b.RelPath); len(got.Chapters) != 0 || got.ChaptersSource != "" {
			t.Errorf("%s: %q from %q, want none", name, titlesOf(got), got.ChaptersSource)
		}
	}

	// A book with chapters of its own (a file holding two) keeps them.
	own := plainWith(lib.ID, "D/Own.m4b", generatedList("Own.m4b", 5000))
	own.Chapters = []metadata.Chapter{{Index: 0, Title: "Opening", FilePath: own.RelPath, End: 1800},
		{Index: 1, Title: "Closing", FilePath: own.RelPath, Start: 1800, End: 3600, BookOffset: 1800}}
	upsert(t, c, own)
	if got := mustBook(t, c, ctx, lib.ID, own.RelPath); !slices.Equal(titlesOf(got), []string{"Opening", "Closing"}) || got.ChaptersSource != "" {
		t.Errorf("own chapters: %q from %q", titlesOf(got), got.ChaptersSource)
	}

	// The admin's choice of the files keeps the files' (none); back to auto
	// takes the generated ones.
	const p = "E/Plain.mp3"
	upsert(t, c, plainWith(lib.ID, p, generatedList("Plain.mp3", 5000)))
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{ChapterSource: ChaptersFromFiles}); err != nil {
		t.Fatal(err)
	}
	if got := mustBook(t, c, ctx, lib.ID, p); len(got.Chapters) != 0 || got.ChaptersSource != "" {
		t.Errorf("files chosen: %q from %q", titlesOf(got), got.ChaptersSource)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{ChapterSource: ChapterSourceAuto}); err != nil {
		t.Fatal(err)
	}
	if got := mustBook(t, c, ctx, lib.ID, p); got.ChaptersSource != ChaptersFromGenerated || len(got.Chapters) != 3 {
		t.Errorf("back to auto: %q from %q", titlesOf(got), got.ChaptersSource)
	}
	// "generated" is never a choice an admin makes.
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{ChapterSource: ChaptersFromGenerated}); err == nil {
		t.Errorf("choosing generated: no error")
	}
}

// The community's fill wins over the generated chapters; the admin's choice of
// the files keeps neither; a community check that stops fitting hands the book
// back to the generated ones.
func TestGeneratedChaptersCommunityWins(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "A/Plain.mp3"
	upsert(t, c, plainWith(lib.ID, p, generatedList("Plain.mp3", 5000, "G1", "G2", "G3")))
	save(t, c, fittedFill(lib.ID, p, string(chapteralign.Fill)))
	b := mustBook(t, c, ctx, lib.ID, p)
	if !slices.Equal(titlesOf(b), []string{"One", "Two", "Three"}) || b.ChaptersSource != ChaptersFromCommunity {
		t.Fatalf("community fill: %q from %q", titlesOf(b), b.ChaptersSource)
	}
	// A rescan keeps the community's.
	upsert(t, c, plainWith(lib.ID, p, generatedList("Plain.mp3", 5000, "G1", "G2", "G3")))
	if b = mustBook(t, c, ctx, lib.ID, p); b.ChaptersSource != ChaptersFromCommunity {
		t.Fatalf("after a rescan: %q from %q", titlesOf(b), b.ChaptersSource)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{ChapterSource: ChaptersFromFiles}); err != nil {
		t.Fatal(err)
	}
	if b = mustBook(t, c, ctx, lib.ID, p); len(b.Chapters) != 0 || b.ChaptersSource != "" {
		t.Fatalf("files chosen: %q from %q", titlesOf(b), b.ChaptersSource)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{ChapterSource: ChapterSourceAuto}); err != nil {
		t.Fatal(err)
	}
	// The community recording turns out to be another edition.
	save(t, c, CommunityChapters{LibraryID: lib.ID, Path: p, Status: string(chapteralign.LengthMismatch), Detail: &chapteralign.Detail{}})
	if b = mustBook(t, c, ctx, lib.ID, p); !slices.Equal(titlesOf(b), []string{"G1", "G2", "G3"}) || b.ChaptersSource != ChaptersFromGenerated {
		t.Fatalf("after a failed check: %q from %q", titlesOf(b), b.ChaptersSource)
	}
}

// A folder book of two files, one chapter per file (no chapters of its own),
// takes a list across both: each chapter in its own file, from the book's
// timeline. Its files are named relative to the book's folder.
func TestGeneratedChaptersAcrossFiles(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "Author/Book"
	b := &Book{LibraryID: lib.ID, RelPath: p, IsFolder: true, Title: "Book", Duration: 3600, AddedAt: "2020-01-01T00:00:00Z",
		Files: []BookFile{{RelPath: p + "/CD1/01.mp3", Seq: 0, Duration: 1800, Size: 10}, {RelPath: p + "/CD2/01.mp3", Seq: 1, Duration: 1800, Size: 20}},
		Chapters: []metadata.Chapter{{Index: 0, Title: "01", FilePath: p + "/CD1/01.mp3", End: 1800},
			{Index: 1, Title: "01", FileIndex: 1, FilePath: p + "/CD2/01.mp3", End: 1800, BookOffset: 1800}},
		Attachments: AttachmentsReading{Sig: "chapters.json:1:1", Files: metadata.AttachmentFiles{Chapters: &metadata.ChapterList{Version: 1,
			Files: []metadata.ChapterListFile{{Path: "CD1/01.mp3", Size: 10, DurationMS: 1800000}, {Path: "CD2/01.mp3", Size: 20, DurationMS: 1800000}},
			Chapters: []metadata.ChapterListEntry{{Title: "Prologue", LengthMS: 900000}, {Title: "Chapter 1", StartMS: 900000, LengthMS: 900000},
				{Title: "Chapter 2", StartMS: 1800000, LengthMS: 900000}, {Title: "Chapter 3", StartMS: 2700000, LengthMS: 900000}}}}},
	}
	upsert(t, c, b)
	got := mustBook(t, c, ctx, lib.ID, p)
	if got.ChaptersSource != ChaptersFromGenerated || len(got.Chapters) != 4 {
		t.Fatalf("generated: %q from %q", titlesOf(got), got.ChaptersSource)
	}
	if ch := got.Chapters[3]; ch.FilePath != p+"/CD2/01.mp3" || ch.FileIndex != 1 || ch.Start != 900 || ch.BookOffset != 2700 {
		t.Fatalf("chapter 3 = %+v", ch)
	}
	// Files in another order are other files.
	b.Files[0].Size, b.Files[1].Size = 20, 10
	upsert(t, c, b)
	if got := mustBook(t, c, ctx, lib.ID, p); got.ChaptersSource != "" || len(got.Chapters) != 2 {
		t.Fatalf("other files: %q from %q", titlesOf(got), got.ChaptersSource)
	}
}

// SetAttachments (the scan's backfill and the Bridge's attachments rescan)
// re-resolves a book and announces it when its chapters.json came, changed or
// went, and only then.
func TestSetAttachmentsResolvesChapters(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "A/Plain.mp3"
	upsert(t, c, plainWith(lib.ID, p, nil))
	var changes atomic.Int32
	c.OnBookChange(func() { changes.Add(1) })
	set := func(files metadata.AttachmentFiles) {
		t.Helper()
		if err := c.SetAttachments(ctx, lib.ID, map[string]AttachmentsReading{p: {Sig: "x", Files: files}}); err != nil {
			t.Fatal(err)
		}
	}
	recaps := &model.Recaps{InShort: "Short."}
	set(metadata.AttachmentFiles{Recaps: recaps})
	if n := changes.Load(); n != 0 {
		t.Fatalf("recaps only: %d changes, want 0", n)
	}
	set(metadata.AttachmentFiles{Recaps: recaps, Chapters: generatedList("Plain.mp3", 5000)})
	if b := mustBook(t, c, ctx, lib.ID, p); changes.Load() != 1 || b.ChaptersSource != ChaptersFromGenerated || len(b.Chapters) != 3 {
		t.Fatalf("chapters came: %d changes, %q from %q", changes.Load(), titlesOf(b), b.ChaptersSource)
	}
	set(metadata.AttachmentFiles{Chapters: generatedList("Plain.mp3", 5000)})
	if n := changes.Load(); n != 1 {
		t.Fatalf("the same chapters: %d changes, want 1", n)
	}
	set(metadata.AttachmentFiles{Chapters: generatedList("Plain.mp3", 5000, "A", "B", "C")})
	if b := mustBook(t, c, ctx, lib.ID, p); changes.Load() != 2 || !slices.Equal(titlesOf(b), []string{"A", "B", "C"}) {
		t.Fatalf("chapters changed: %d changes, %q", changes.Load(), titlesOf(b))
	}
	set(metadata.AttachmentFiles{})
	if b := mustBook(t, c, ctx, lib.ID, p); changes.Load() != 3 || len(b.Chapters) != 0 || b.ChaptersSource != "" {
		t.Fatalf("chapters went: %d changes, %q from %q", changes.Load(), titlesOf(b), b.ChaptersSource)
	}
}
