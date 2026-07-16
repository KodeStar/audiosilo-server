package library

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
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
	// A normal relative path resolves within root. SafeJoin canonicalizes the
	// root's symlinks (e.g. macOS /var -> /private/var), so compare against the
	// resolved root.
	got, err := SafeJoin(root, "Author/Book.m4b")
	if err != nil {
		t.Fatal(err)
	}
	resolvedRoot, _ := filepath.EvalSymlinks(root)
	if filepath.Dir(filepath.Dir(got)) != resolvedRoot {
		t.Fatalf("resolved path %q not under root %q", got, resolvedRoot)
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
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})

	// ffprobe "" keeps the test independent of an installed ffmpeg; path-derived
	// metadata still populates title/author/series. Each book lives in its own
	// folder (the default "folder = one book" model): "Brandon Sanderson/Mistborn"
	// (1 track) and "Will Wight/Cradle" (2 tracks).
	scanner := NewScanner(cat, "", slog.Default())
	res, err := scanner.Scan(ctx, *lib)
	if err != nil {
		t.Fatal(err)
	}
	if res.Indexed != 2 {
		t.Fatalf("expected 2 indexed books (one per folder), got %d", res.Indexed)
	}

	page, _ := cat.ListBooks(ctx, catalog.ListOptions{LibraryID: lib.ID, Sort: "author"})
	if len(page.Books) != 2 {
		t.Fatalf("expected 2 books, got %d", len(page.Books))
	}
	first := page.Books[0]
	if first.Author != "Brandon Sanderson" || first.Title != "The Final Empire" || !first.IsFolder {
		t.Fatalf("unexpected first book: %+v", first)
	}
	// The scanner stamps added_at from the filesystem (birth time / mtime).
	if _, err := time.Parse(time.RFC3339, first.AddedAt); err != nil {
		t.Fatalf("added_at not a valid RFC3339 timestamp: %q (%v)", first.AddedAt, err)
	}
	// The Cradle folder is one book carrying both files as tracks.
	cradle, err := cat.GetBookByPath(ctx, lib.ID, "Will Wight/Cradle")
	if err != nil || !cradle.IsFolder || len(cradle.Files) != 2 {
		t.Fatalf("Cradle should be one folder book with 2 tracks: %+v (err %v)", cradle, err)
	}

	// A second scan with no changes should index nothing (incremental skip).
	res2, _ := scanner.Scan(ctx, *lib)
	if res2.Indexed != 0 {
		t.Fatalf("expected incremental no-op, indexed %d", res2.Indexed)
	}

	// FTS search works over scanned data (the Cradle book's title comes from its
	// primary track's tag).
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
		Name: "Multi", Root: root,
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
	// A single chaptered m4b living in its own book folder - the common
	// "books in their own folders" layout. The tree is built at runtime (the
	// apostrophes in the real-world title are prohibited inside a Go module
	// zip, so a committed fixture with this path would make the whole module
	// unfetchable); building it here keeps apostrophe paths exercised.
	root := t.TempDir()
	copyChapteredM4B(t, filepath.Join(root,
		"A. F. Kay", "Divine Apostasy", "AF01 - Shade's First Rule", "Shade's First Rule.m4b"))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{
		Name: "FolderBooks", Root: root,
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

	// A library we can mutate: a book lives in its own folder (folder = one book),
	// so the book's path is the folder path.
	root := t.TempDir()
	oldRel := "Author/Series/Unsouled"
	copyFixtureM4B(t, filepath.Join(root, oldRel, "01 - Unsouled.m4b"))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "M", Root: root})
	scanner := NewScanner(cat, "", slog.Default())
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}

	// Record progress on the original (folder) path.
	uid := seedUserID(t, db)
	if _, err := cat.SaveProgress(ctx, uid, catalog.Progress{
		Ref: catalog.Ref{LibraryID: lib.ID, Path: oldRel}, Position: 99,
	}); err != nil {
		t.Fatal(err)
	}

	// Rename the book folder on disk, then rescan: move-tracking (keyed on the
	// primary file's fingerprint) should carry progress to the new folder path.
	newRel := "Author/Series/Unsouled (2024)"
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
		Name: "Main", Root: root,
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

// TestIndexPathSymlinkedRoot guards against a regression where SafeJoin's
// symlink-resolved path leaked into the scanner's rel_path derivation: when the
// library root is reached through a symlink (e.g. macOS /tmp -> /private/tmp or a
// NAS /data -> /mnt/...), on-demand IndexPath must still record the requested
// rel_path, not a "../"-laden one computed against the unresolved root.
func TestIndexPathSymlinkedRoot(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := catalog.New(db, time.Now)

	real, _ := filepath.Abs(testdataRoot(t))
	link := filepath.Join(t.TempDir(), "library-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: link})
	scanner := NewScanner(cat, "", slog.Default())

	// Resolving a file inside a book folder yields that folder book; its rel_path
	// must be the clean folder path, never a symlink-resolved "../"-laden one.
	book, err := scanner.IndexPath(ctx, *lib, "Will Wight/Cradle/01 - Unsouled.m4b")
	if err != nil {
		t.Fatalf("IndexPath through symlinked root: %v", err)
	}
	const wantRel = "Will Wight/Cradle"
	if book.RelPath != wantRel {
		t.Fatalf("rel_path = %q, want %q (symlinked root must not corrupt rel_path)", book.RelPath, wantRel)
	}
	// And the book must be retrievable by its folder path.
	if _, err := cat.GetBookByPath(ctx, lib.ID, wantRel); err != nil {
		t.Fatalf("GetBookByPath(%q) after on-demand index: %v", wantRel, err)
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
		Name: "Main", Root: root,
	})
	scanner := NewScanner(cat, "", slog.Default())
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}

	indexed := func() int {
		page, _ := cat.ListBooks(ctx, catalog.ListOptions{LibraryID: lib.ID})
		return len(page.Books)
	}
	if indexed() != 2 {
		t.Fatalf("setup: expected 2 indexed, got %d", indexed())
	}

	// Case 1: root no longer exists (share unmounted). Scan must abort without
	// pruning, and the index must be left intact.
	missing := *lib
	missing.Root = filepath.Join(t.TempDir(), "gone")
	if _, err := scanner.Scan(ctx, missing); !errors.Is(err, ErrLibraryUnavailable) {
		t.Fatalf("expected ErrLibraryUnavailable for missing root, got %v", err)
	}
	if indexed() != 2 {
		t.Fatalf("index was pruned despite missing root: %d remain", indexed())
	}

	// Case 2: root exists but is empty (mount dropped to an empty dir). Same
	// protection.
	empty := *lib
	empty.Root = t.TempDir()
	if _, err := scanner.Scan(ctx, empty); !errors.Is(err, ErrLibraryUnavailable) {
		t.Fatalf("expected ErrLibraryUnavailable for empty root, got %v", err)
	}
	if indexed() != 2 {
		t.Fatalf("index was pruned despite empty root: %d remain", indexed())
	}
}

// newScanEnv builds an in-memory catalog + scanner (ffprobe disabled).
func newScanEnv(t *testing.T) (*catalog.Catalog, *Scanner, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := catalog.New(db, time.Now)
	return cat, NewScanner(cat, "", slog.Default()), ctx
}

// copyFixtureM4B writes a copy of a fixture audiobook to dst (creating parents).
// copyChapteredM4B copies the 3-chapter m4b fixture (Prologue / Chapter One /
// Chapter Two embedded chapter marks) to dst, creating parent directories.
func copyChapteredM4B(t *testing.T, dst string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "chaptered.m4b"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func copyFixtureM4B(t *testing.T, dst string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(testdataRoot(t), "Will Wight", "Cradle", "01 - Unsouled.m4b"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestScannerFindsFolderCover guards the cover-art lookup: a folder book's cover
// lives INSIDE the folder (the demo bug was looking in the parent), an arbitrary
// image in a book folder is used as a fallback, and a loose single-file book picks
// up a conventionally-named cover beside it.
func TestScannerFindsFolderCover(t *testing.T) {
	cat, scanner, ctx := newScanEnv(t)
	root := t.TempDir()
	writeImg := func(p string) {
		t.Helper()
		if err := os.WriteFile(p, []byte("img"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Folder book with a conventionally-named cover inside the folder.
	named := filepath.Join(root, "Charles Dickens - A Christmas Carol")
	copyFixtureM4B(t, filepath.Join(named, "01.mp3"))
	copyFixtureM4B(t, filepath.Join(named, "02.mp3"))
	writeImg(filepath.Join(named, "cover.jpg"))
	// Folder book whose only image is non-conventionally named → first-image fallback.
	anyimg := filepath.Join(root, "Some Author - Some Book")
	copyFixtureM4B(t, filepath.Join(anyimg, "01.mp3"))
	writeImg(filepath.Join(anyimg, "art.png"))
	// Folder book with a thumbnail (sorts first) and a "cover"-named image → the
	// cover-named one wins over the arbitrary thumbnail.
	coverish := filepath.Join(root, "Thumbnail Author - Book")
	copyFixtureM4B(t, filepath.Join(coverish, "01.mp3"))
	writeImg(filepath.Join(coverish, "61AbCd._SL500_.jpg"))
	writeImg(filepath.Join(coverish, "Book - Cover.jpg"))
	// Multi-CD book: audio lives in CD subfolders, the cover sits in the parent.
	cd := filepath.Join(root, "Big Series", "The Big Book")
	copyFixtureM4B(t, filepath.Join(cd, "CD1", "01.mp3"))
	copyFixtureM4B(t, filepath.Join(cd, "CD2", "01.mp3"))
	writeImg(filepath.Join(cd, "cover.jpg"))
	// Loose single-file book at the root with a named cover beside it.
	copyFixtureM4B(t, filepath.Join(root, "Standalone.m4b"))
	writeImg(filepath.Join(root, "cover.jpg"))

	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "Covers", Root: root})
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	check := func(path, want string) {
		t.Helper()
		b, err := cat.GetBookByPath(ctx, lib.ID, path)
		if err != nil {
			t.Fatalf("get %q: %v", path, err)
		}
		if b.CoverPath != want {
			t.Fatalf("book %q cover = %q, want %q", path, b.CoverPath, want)
		}
	}
	check("Charles Dickens - A Christmas Carol", "Charles Dickens - A Christmas Carol/cover.jpg")
	check("Some Author - Some Book", "Some Author - Some Book/art.png")
	check("Thumbnail Author - Book", "Thumbnail Author - Book/Book - Cover.jpg")
	check("Big Series/The Big Book/CD1", "Big Series/The Big Book/cover.jpg")
	check("Big Series/The Big Book/CD2", "Big Series/The Big Book/cover.jpg")
	check("Standalone.m4b", "cover.jpg")
}

func TestChooseTitle(t *testing.T) {
	// A meaningful embedded title wins; a missing or generic one falls back to the
	// path-derived title.
	cases := []struct{ embedded, path, want string }{
		{"Unsouled", "01 - Unsouled", "Unsouled"},
		{"", "A Christmas Carol", "A Christmas Carol"},
		{"Track 01", "A Christmas Carol", "A Christmas Carol"},
		{"Chapter 5", "CD1", "CD1"},
	}
	for _, c := range cases {
		if got := chooseTitle(c.embedded, c.path); got != c.want {
			t.Errorf("chooseTitle(%q, %q) = %q, want %q", c.embedded, c.path, got, c.want)
		}
	}
}

func TestIsDiscFolder(t *testing.T) {
	for _, s := range []string{"CD1", "CD 1", "cd01", "Disc 2", "disk3", "CD"} {
		if !isDiscFolder(s) {
			t.Errorf("isDiscFolder(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"The Big Book", "CD Projekt", "Disco Inferno", "Author Name"} {
		if isDiscFolder(s) {
			t.Errorf("isDiscFolder(%q) = true, want false", s)
		}
	}
}

// TestScannerFolderIsOneBook is the regression guard for the "every chapter
// counted as a book" bug: a folder of audio is ONE book regardless of how its
// files are named (distinct chapter titles included), single-file book folders
// key on the folder, and only audio directly at the library root is per-file.
func TestScannerFolderIsOneBook(t *testing.T) {
	cat, scanner, ctx := newScanEnv(t)
	root := t.TempDir()
	// A multi-track book whose chapter files have DISTINCT titles - the case that
	// used to explode into one book per chapter.
	base := filepath.Join(root, "Lee Child", "Jack Reacher", "JR04 - Running Blind")
	copyFixtureM4B(t, filepath.Join(base, "01 - The Setup.mp3"))
	copyFixtureM4B(t, filepath.Join(base, "02 - The Chase.mp3"))
	copyFixtureM4B(t, filepath.Join(base, "03 - The Reveal.mp3"))
	// A single-file book in its own folder.
	copyFixtureM4B(t, filepath.Join(root, "Casualfarmer", "Beware of Chicken", "BOC01", "Beware of Chicken.m4b"))
	// A loose file directly at the library root (flat) is its own book.
	copyFixtureM4B(t, filepath.Join(root, "A Standalone Title.m4b"))

	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "FolderPerBook", Root: root})
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}

	page, _ := cat.ListBooks(ctx, catalog.ListOptions{LibraryID: lib.ID})
	if len(page.Books) != 3 {
		t.Fatalf("expected 3 books (2 folders + 1 root file), got %d", len(page.Books))
	}
	rb, err := cat.GetBookByPath(ctx, lib.ID, "Lee Child/Jack Reacher/JR04 - Running Blind")
	if err != nil || !rb.IsFolder || len(rb.Files) != 3 {
		t.Fatalf("multi-chapter folder must be ONE book with 3 tracks: %+v (err %v)", rb, err)
	}
	if b, err := cat.GetBookByPath(ctx, lib.ID, "Casualfarmer/Beware of Chicken/BOC01"); err != nil || !b.IsFolder {
		t.Fatalf("single-file book must key on its folder: %+v (err %v)", b, err)
	}
	if b, err := cat.GetBookByPath(ctx, lib.ID, "A Standalone Title.m4b"); err != nil || b.IsFolder {
		t.Fatalf("a root-level file must be its own single-file book: %+v (err %v)", b, err)
	}
}

// TestScannerIgnoresAAX covers the Audible DRM case: an .aax sitting next to its
// converted .m4b must NOT join the book (it can't be streamed, and indexing it
// doubled up the chapters), and a folder holding ONLY an .aax yields no book.
func TestScannerIgnoresAAX(t *testing.T) {
	cat, scanner, ctx := newScanEnv(t)
	root := t.TempDir()
	// A converted book with the original .aax left alongside it.
	base := filepath.Join(root, "Andy Weir", "Project Hail Mary")
	copyFixtureM4B(t, filepath.Join(base, "Project Hail Mary.m4b"))
	if err := os.WriteFile(filepath.Join(base, "Project Hail Mary.aax"), []byte("drm"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A folder with only an unconverted .aax - nothing playable, so no book.
	onlyAax := filepath.Join(root, "Andy Weir", "The Martian")
	if err := os.MkdirAll(onlyAax, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(onlyAax, "The Martian.aax"), []byte("drm"), 0o644); err != nil {
		t.Fatal(err)
	}

	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "AAX", Root: root})
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}

	page, _ := cat.ListBooks(ctx, catalog.ListOptions{LibraryID: lib.ID})
	if len(page.Books) != 1 {
		t.Fatalf("expected 1 book (the .m4b only; the .aax-only folder is skipped), got %d", len(page.Books))
	}
	b, err := cat.GetBookByPath(ctx, lib.ID, "Andy Weir/Project Hail Mary")
	if err != nil || !b.IsFolder || len(b.Files) != 1 {
		t.Fatalf("the .aax must not join the book: want 1 file, got %+v (err %v)", b, err)
	}
	if strings.HasSuffix(strings.ToLower(b.Files[0].RelPath), ".aax") {
		t.Fatalf("the indexed file must be the .m4b, not the .aax: %q", b.Files[0].RelPath)
	}
	if _, err := cat.GetBookByPath(ctx, lib.ID, "Andy Weir/The Martian"); err == nil {
		t.Fatal("a folder with only an .aax must not produce a book")
	}
}

// TestScannerFolderOverrides verifies the per-folder override: collection splits
// a folder into one book per file (the books_in_folder case), and IndexPath
// honors it; clearing reverts to one folder book.
func TestScannerFolderOverrides(t *testing.T) {
	cat, scanner, ctx := newScanEnv(t)
	root := t.TempDir()
	dir := filepath.Join(root, "Will Wight", "Cradle")
	copyFixtureM4B(t, filepath.Join(dir, "01 - Unsouled.m4b"))
	copyFixtureM4B(t, filepath.Join(dir, "02 - Soulsmith.m4b"))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "O", Root: root})

	// Default: the folder is ONE book with both files as tracks.
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	page, _ := cat.ListBooks(ctx, catalog.ListOptions{LibraryID: lib.ID})
	if len(page.Books) != 1 || !page.Books[0].IsFolder {
		t.Fatalf("default: folder should be one book, got %d", len(page.Books))
	}

	// collection override: each file becomes its own book.
	if err := cat.SetFolderOverride(ctx, lib.ID, "Will Wight/Cradle", catalog.OverrideCollection); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	page, _ = cat.ListBooks(ctx, catalog.ListOptions{LibraryID: lib.ID})
	if len(page.Books) != 2 {
		t.Fatalf("collection override: expected 2 single-file books, got %d", len(page.Books))
	}
	for _, p := range []string{"Will Wight/Cradle/01 - Unsouled.m4b", "Will Wight/Cradle/02 - Soulsmith.m4b"} {
		if _, err := cat.GetBookByPath(ctx, lib.ID, p); err != nil {
			t.Fatalf("expected a single-file book at %q: %v", p, err)
		}
	}

	// IndexPath honors the override: a file is its own book, not the folder.
	b, err := scanner.IndexPath(ctx, *lib, "Will Wight/Cradle/01 - Unsouled.m4b")
	if err != nil || b.IsFolder || b.RelPath != "Will Wight/Cradle/01 - Unsouled.m4b" {
		t.Fatalf("IndexPath under collection override should be the single-file book: %+v (err %v)", b, err)
	}

	// Clearing reverts to one folder book.
	if err := cat.DeleteFolderOverride(ctx, lib.ID, "Will Wight/Cradle"); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	page, _ = cat.ListBooks(ctx, catalog.ListOptions{LibraryID: lib.ID})
	if len(page.Books) != 1 || !page.Books[0].IsFolder {
		t.Fatalf("after clearing override: expected one folder book, got %d", len(page.Books))
	}
}

// TestScannerBackfillsMissingCodec guards the cross-deploy gap: a book indexed
// before the codec column existed (codec empty, but with a stored duration) must
// be re-probed on the next scan so its codec backfills - otherwise direct_playable
// can't be trusted for already-indexed libraries.
func TestScannerBackfillsMissingCodec(t *testing.T) {
	if !metadata.HasFFprobe("ffprobe") {
		t.Skip("ffprobe required to read codec")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	copyFixtureM4B(t, filepath.Join(root, "Book Folder", "audio.m4b"))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "C", Root: root})
	scanner := NewScanner(cat, "ffprobe", slog.Default())
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	b, err := cat.GetBookByPath(ctx, lib.ID, "Book Folder")
	if err != nil || b.Codec == "" {
		t.Fatalf("first scan should record a codec: %+v (err %v)", b, err)
	}

	// Simulate a row indexed before the codec column: clear codec, keep duration.
	if _, err := db.ExecContext(ctx, `UPDATE books SET codec='' WHERE id = ?`, b.ID); err != nil {
		t.Fatal(err)
	}
	// Rescan: mtime/size are unchanged, but the missing codec forces a re-probe.
	res, err := scanner.Scan(ctx, *lib)
	if err != nil {
		t.Fatal(err)
	}
	if res.Indexed != 1 {
		t.Fatalf("codec-less book should be re-probed, indexed=%d", res.Indexed)
	}
	if b2, _ := cat.GetBookByPath(ctx, lib.ID, "Book Folder"); b2.Codec == "" {
		t.Fatal("codec was not backfilled on rescan")
	}
}

// TestBrowseFSHidesNonAudio verifies the filesystem view lists audio files and
// directories only, so a client never opens a .jpg/.nfo as a book.
func TestBrowseFSHidesNonAudio(t *testing.T) {
	root := t.TempDir()
	copyFixtureM4B(t, filepath.Join(root, "Book", "audio.m4b"))
	for _, name := range []string{"cover.jpg", "notes.nfo", "desc.txt"} {
		if err := os.WriteFile(filepath.Join(root, "Book", name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "Subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	listing, err := BrowseFS(root, "Book", 0, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	gotAudio := false
	for _, e := range listing.Entries {
		if !e.IsDir && !e.IsAudio {
			t.Fatalf("non-audio file leaked into browse: %q", e.Name)
		}
		if e.Name == "audio.m4b" {
			gotAudio = true
		}
	}
	if !gotAudio {
		t.Fatal("audio file missing from browse")
	}

	// Directories remain navigable.
	top, _ := BrowseFS(root, "", 0, 100, nil)
	hasDir := false
	for _, e := range top.Entries {
		if e.IsDir && e.Name == "Subdir" {
			hasDir = true
		}
	}
	if !hasDir {
		t.Fatal("directories should remain navigable in browse")
	}
}
