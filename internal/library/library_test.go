package library

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store"
)

func TestSafeJoinRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []string{"../etc/passwd", "../../secret", "a/../../b", "/etc/passwd"} {
		if _, err := SafeJoin(root, bad); err == nil {
			// /etc/passwd is absolute; after anchoring it becomes root/etc/passwd,
			// which is allowed, so only verify the traversal cases escape.
			if bad == "/etc/passwd" {
				continue
			}
			t.Errorf("SafeJoin(%q) should have been rejected", bad)
		}
	}
	// A normal relative path resolves within root.
	got, err := SafeJoin(root, "Author/Book.m4b")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(filepath.Dir(got)) != root {
		t.Fatalf("resolved path %q not under root %q", got, root)
	}
}

// testdataRoot points at the repository's fixture library.
func testdataRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "testdata", "library")
}

func TestBrowseFSInstant(t *testing.T) {
	listing, err := BrowseFS(testdataRoot(t), "", 0, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	if listing.Total != 2 {
		t.Fatalf("expected 2 top-level authors, got %d", listing.Total)
	}
	// Directories must sort first and be flagged.
	if !listing.Entries[0].IsDir {
		t.Fatal("expected directory entries")
	}
}

func TestScannerIndexesFixtures(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := catalog.New(db, time.Now)
	root, _ := filepath.Abs(testdataRoot(t))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{
		Name: "Main", Root: root, Layout: config.LayoutBooksInFolder,
	})

	// ffprobe "" keeps the test independent of an installed ffmpeg; path-derived
	// metadata still populates title/author/series.
	scanner := NewScanner(cat, "", slog.Default())
	res, err := scanner.Scan(ctx, *lib)
	if err != nil {
		t.Fatal(err)
	}
	if res.Indexed != 3 {
		t.Fatalf("expected 3 indexed books, got %d", res.Indexed)
	}

	page, _ := cat.ListBooks(ctx, catalog.ListOptions{LibraryID: lib.ID, Sort: "author"})
	if len(page.Books) != 3 {
		t.Fatalf("expected 3 books, got %d", len(page.Books))
	}
	first := page.Books[0]
	if first.Author != "Brandon Sanderson" || first.Series != "Mistborn" || first.Title != "The Final Empire" {
		t.Fatalf("unexpected first book: %+v", first)
	}

	// A second scan with no changes should index nothing (incremental skip).
	res2, _ := scanner.Scan(ctx, *lib)
	if res2.Indexed != 0 {
		t.Fatalf("expected incremental no-op, indexed %d", res2.Indexed)
	}

	// FTS search works over scanned data.
	hits, _ := cat.Search(ctx, "unsouled", []catalog.Scope{{LibraryID: lib.ID, AllowAll: true}}, 10)
	if len(hits) != 1 || hits[0].Title != "Unsouled" {
		t.Fatalf("search result = %+v", hits)
	}
}

func TestScannerMultiFileChapters(t *testing.T) {
	if !metadata.HasFFprobe("ffprobe") {
		t.Skip("ffprobe not available; multi-file durations require it")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := catalog.New(db, time.Now)
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "multifile"))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{
		Name: "Multi", Root: root, Layout: config.LayoutChaptersInFolder,
	})
	scanner := NewScanner(cat, "ffprobe", slog.Default())
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}

	page, _ := cat.ListBooks(ctx, catalog.ListOptions{LibraryID: lib.ID})
	if len(page.Books) != 1 {
		t.Fatalf("expected 1 folder book, got %d", len(page.Books))
	}
	book, _ := cat.GetBook(ctx, page.Books[0].ID)
	if !book.IsFolder || len(book.Files) != 2 || len(book.Chapters) != 2 {
		t.Fatalf("expected a 2-part folder book, got folder=%v files=%d chapters=%d",
			book.IsFolder, len(book.Files), len(book.Chapters))
	}
	// Each part is a chapter pointing at its own file, with cumulative offsets.
	c0, c1 := book.Chapters[0], book.Chapters[1]
	if c0.FileIndex != 0 || c0.BookOffset != 0 {
		t.Fatalf("chapter 0 = %+v", c0)
	}
	if c1.FileIndex != 1 || c1.BookOffset <= 0 {
		t.Fatalf("chapter 1 should start after chapter 0: %+v", c1)
	}
	// book_offset of part 2 equals part 1's duration; total ~= sum of parts.
	if diff := c1.BookOffset - c0.End; diff > 0.01 || diff < -0.01 {
		t.Fatalf("part 2 offset (%v) should equal part 1 end (%v)", c1.BookOffset, c0.End)
	}
	if book.Duration < c1.BookOffset {
		t.Fatalf("total duration %v should cover all parts", book.Duration)
	}
}

func TestScannerFolderBookExpandsEmbeddedChapters(t *testing.T) {
	if !metadata.HasFFprobe("ffprobe") {
		t.Skip("ffprobe not available; embedded chapters require it")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := catalog.New(db, time.Now)
	// A single chaptered m4b living in its own book folder — the common
	// "books in their own folders" layout.
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "folderbook"))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{
		Name: "FolderBooks", Root: root, Layout: config.LayoutChaptersInFolder,
	})
	scanner := NewScanner(cat, "ffprobe", slog.Default())
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}

	page, _ := cat.ListBooks(ctx, catalog.ListOptions{LibraryID: lib.ID})
	if len(page.Books) != 1 {
		t.Fatalf("expected 1 folder book, got %d", len(page.Books))
	}
	book, _ := cat.GetBook(ctx, page.Books[0].ID)
	if book.Author != "A. F. Kay" || book.Series != "Divine Apostasy" {
		t.Fatalf("path-derived author/series wrong: author=%q series=%q", book.Author, book.Series)
	}
	// The single m4b's three embedded chapters must be expanded (not collapsed
	// into one part), all pointing at file 0 with their in-file offsets.
	if len(book.Chapters) != 3 {
		t.Fatalf("expected 3 embedded chapters expanded, got %d", len(book.Chapters))
	}
	for i, ch := range book.Chapters {
		if ch.FileIndex != 0 {
			t.Fatalf("chapter %d should reference file 0, got %d", i, ch.FileIndex)
		}
		if ch.BookOffset != ch.Start {
			t.Fatalf("single-file chapter %d book_offset (%v) should equal start (%v)", i, ch.BookOffset, ch.Start)
		}
	}
	if book.Chapters[0].Title != "Prologue" || book.Chapters[2].Title != "Chapter Two" {
		t.Fatalf("chapter titles not read: %q .. %q", book.Chapters[0].Title, book.Chapters[2].Title)
	}
}

func TestScannerMoveTracking(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := catalog.New(db, time.Now)

	// A library we can mutate: copy one fixture book into a temp dir.
	root := t.TempDir()
	srcDir := filepath.Join(root, "Author", "Series")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(testdataRoot(t), "Will Wight", "Cradle", "01 - Unsouled.m4b"))
	if err != nil {
		t.Fatal(err)
	}
	oldRel := "Author/Series/01 - Unsouled.m4b"
	if err := os.WriteFile(filepath.Join(root, oldRel), data, 0o644); err != nil {
		t.Fatal(err)
	}
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "M", Root: root, Layout: config.LayoutBooksInFolder})
	scanner := NewScanner(cat, "", slog.Default())
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}

	// Record progress on the original path.
	uid := seedUserID(t, db)
	if _, err := cat.SaveProgress(ctx, uid, catalog.Progress{
		Ref: catalog.Ref{LibraryID: lib.ID, Path: oldRel}, Position: 99,
	}); err != nil {
		t.Fatal(err)
	}

	// Rename the file on disk, then rescan: move-tracking should carry progress.
	newRel := "Author/Series/01 - Unsouled (2024).m4b"
	if err := os.Rename(filepath.Join(root, oldRel), filepath.Join(root, newRel)); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}

	if p, _ := cat.GetProgress(ctx, uid, catalog.Ref{LibraryID: lib.ID, Path: oldRel}); p != nil {
		t.Fatal("progress should have left the old path")
	}
	moved, _ := cat.GetProgress(ctx, uid, catalog.Ref{LibraryID: lib.ID, Path: newRel})
	if moved == nil || moved.Position != 99 {
		t.Fatalf("progress should follow the move, got %+v", moved)
	}
}

func seedUserID(t *testing.T, db *store.DB) int64 {
	t.Helper()
	res, err := db.ExecContext(context.Background(),
		`INSERT INTO users(username, password_hash, role, created_at, updated_at)
		 VALUES('mover','x','user','t','t')`)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestIndexPathOnDemand(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := catalog.New(db, time.Now)
	root, _ := filepath.Abs(testdataRoot(t))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{
		Name: "Main", Root: root, Layout: config.LayoutBooksInFolder,
	})
	scanner := NewScanner(cat, "", slog.Default())

	// No scan has run, so the index is empty. Resolving a browsed file path must
	// index just that file and return it.
	rel := "Will Wight/Cradle/01 - Unsouled.m4b"
	book, err := scanner.IndexPath(ctx, *lib, rel)
	if err != nil {
		t.Fatalf("IndexPath: %v", err)
	}
	if book.ID == 0 || book.Title != "Unsouled" || book.Author != "Will Wight" {
		t.Fatalf("unexpected resolved book: %+v", book)
	}
	// Only the one path should have been indexed (not the whole library).
	page, _ := cat.ListBooks(ctx, catalog.ListOptions{LibraryID: lib.ID})
	if len(page.Books) != 1 {
		t.Fatalf("expected only the resolved book indexed, got %d", len(page.Books))
	}

	// A non-audio / missing path is rejected, not indexed.
	if _, err := scanner.IndexPath(ctx, *lib, "Will Wight"); !errors.Is(err, ErrNotIndexable) {
		t.Fatalf("expected ErrNotIndexable for a directory in file layout, got %v", err)
	}
	if _, err := scanner.IndexPath(ctx, *lib, "nope/missing.m4b"); !errors.Is(err, ErrNotIndexable) {
		t.Fatalf("expected ErrNotIndexable for missing path, got %v", err)
	}
}

func TestScannerProtectsIndexWhenRootUnavailable(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := catalog.New(db, time.Now)
	root, _ := filepath.Abs(testdataRoot(t))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{
		Name: "Main", Root: root, Layout: config.LayoutBooksInFolder,
	})
	scanner := NewScanner(cat, "", slog.Default())
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}

	indexed := func() int {
		page, _ := cat.ListBooks(ctx, catalog.ListOptions{LibraryID: lib.ID})
		return len(page.Books)
	}
	if indexed() != 3 {
		t.Fatalf("setup: expected 3 indexed, got %d", indexed())
	}

	// Case 1: root no longer exists (share unmounted). Scan must abort without
	// pruning, and the index must be left intact.
	missing := *lib
	missing.Root = filepath.Join(t.TempDir(), "gone")
	if _, err := scanner.Scan(ctx, missing); !errors.Is(err, ErrLibraryUnavailable) {
		t.Fatalf("expected ErrLibraryUnavailable for missing root, got %v", err)
	}
	if indexed() != 3 {
		t.Fatalf("index was pruned despite missing root: %d remain", indexed())
	}

	// Case 2: root exists but is empty (mount dropped to an empty dir). Same
	// protection.
	empty := *lib
	empty.Root = t.TempDir()
	if _, err := scanner.Scan(ctx, empty); !errors.Is(err, ErrLibraryUnavailable) {
		t.Fatalf("expected ErrLibraryUnavailable for empty root, got %v", err)
	}
	if indexed() != 3 {
		t.Fatalf("index was pruned despite empty root: %d remain", indexed())
	}
}
