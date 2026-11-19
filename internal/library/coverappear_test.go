package library

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// A cover image put beside a book that had none is the book's cover from the
// next scan, though its audio is unchanged: a folder book takes any image in
// its folder, a loose file in the library's root a conventionally named one beside it. A book whose
// cover is already known is not read again for it.
func TestScanFindsCoverAddedToUnchangedBook(t *testing.T) {
	t.Parallel()
	cat, s, ctx := newScanEnv(t)
	root := t.TempDir()
	copyFixtureM4B(t, filepath.Join(root, "Author", "Book", "01.m4b"))
	copyFixtureM4B(t, filepath.Join(root, "Single.m4b"))
	copyFixtureM4B(t, filepath.Join(root, "Author", "Covered", "01.m4b"))
	writeFile(t, root, "Author/Covered/cover.jpg", []byte("img"))
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "C", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	scan := func() *ScanResult {
		t.Helper()
		res, err := s.Scan(ctx, *lib)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	coverOf := func(rel string) string {
		t.Helper()
		b, err := cat.GetBookByPath(ctx, lib.ID, rel)
		if err != nil {
			t.Fatal(err)
		}
		return b.CoverPath
	}
	scan()
	if got := coverOf("Author/Book"); got != "" {
		t.Fatalf("before: cover %q, want none", got)
	}

	writeFile(t, root, "Author/Book/Some Book - Front.png", []byte("img"))
	writeFile(t, root, "folder.jpg", []byte("img"))
	res := scan()
	if got := coverOf("Author/Book"); got != "Author/Book/Some Book - Front.png" {
		t.Fatalf("folder book: cover %q, want the image added", got)
	}
	if got := coverOf("Single.m4b"); got != "folder.jpg" {
		t.Fatalf("loose file: cover %q, want the folder image added", got)
	}
	if res.Updated != 2 {
		t.Fatalf("updated %d, want the two books whose cover appeared", res.Updated)
	}

	// Nothing new: no book is read again.
	if res := scan(); res.Updated != 0 {
		t.Fatalf("rescan updated %d, want 0", res.Updated)
	}
	if got := coverOf("Author/Covered"); got != "Author/Covered/cover.jpg" {
		t.Fatalf("covered book: %q", got)
	}
}

// An image beside a book is no reason to read the book again on every scan
// when the read can store no cover for it: a book kept as stored over a part
// the share refuses is left to its problem's check (one file's read) until the
// part opens, and a conventionally named link with nothing behind it, which
// the cover look can't take for a loose file, marks no book to read again.
func TestScanDoesNotRereadForACoverItCannotStore(t *testing.T) {
	t.Parallel()
	cat, s, ctx := newScanEnv(t)
	root := t.TempDir()
	copyFixtureM4B(t, filepath.Join(root, "Author", "Book", "01.m4b"))
	copyFixtureM4B(t, filepath.Join(root, "Author", "Book", "02.m4b"))
	copyFixtureM4B(t, filepath.Join(root, "Single.m4b"))
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "C", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var reads []string
	s.readFile = func(ctx context.Context, root, p, ffprobe string, timeout time.Duration) *metadata.Metadata {
		mu.Lock()
		reads = append(reads, filepath.Base(p))
		mu.Unlock()
		return extractFile(ctx, root, p, ffprobe, timeout)
	}
	scan := func() (*ScanResult, string) {
		t.Helper()
		mu.Lock()
		reads = nil
		mu.Unlock()
		res, err := s.Scan(ctx, *lib)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		return res, strings.Join(reads, " ")
	}
	scan()

	if err := os.Symlink(filepath.Join(root, "nowhere.jpg"), filepath.Join(root, "cover.jpg")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	part := filepath.Join(root, "Author", "Book", "02.m4b")
	if err := os.Chmod(part, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(part, 0o644) })
	if f, err := os.Open(part); err == nil {
		_ = f.Close()
		t.Skip("file permissions don't stop this user reading (root?)")
	}
	writeFile(t, root, "Author/Book/front.png", []byte("img"))

	// The image is new: the folder book is read once, and kept as stored
	// with the refused part's problem. The loose file is not read at all.
	if _, read := scan(); strings.Contains(read, "Single.m4b") || !strings.Contains(read, "01.m4b") {
		t.Fatalf("first scan with the image read %q, want the folder book and not the loose file", read)
	}
	for i := range 2 {
		res, read := scan()
		if read != "02.m4b" || res.Updated != 0 {
			t.Errorf("scan %d with the part still refused read %q, updated %d; want only the refused part asked again", i+2, read, res.Updated)
		}
	}

	// The part opens: the book is read whole, the cover with it.
	if err := os.Chmod(part, 0o644); err != nil {
		t.Fatal(err)
	}
	scan()
	b, err := cat.GetBookByPath(ctx, lib.ID, "Author/Book")
	if err != nil {
		t.Fatal(err)
	}
	if b.CoverPath != "Author/Book/front.png" {
		t.Errorf("once the part opens: cover %q, want the image", b.CoverPath)
	}
	if res, read := scan(); read != "" || res.Updated != 0 {
		t.Errorf("rescan read %q, updated %d; want nothing read", read, res.Updated)
	}
}
