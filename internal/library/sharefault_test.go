package library

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// failLooks makes readDirLook and statLook fail at the paths in fail, each
// with its errno, until the test ends. Not for parallel tests.
func failLooks(t *testing.T, fail map[string]syscall.Errno) {
	t.Helper()
	wasDir, wasStat := readDirLook, statLook
	t.Cleanup(func() { readDirLook, statLook = wasDir, wasStat })
	readDirLook = func(ctx context.Context, p string, timeout time.Duration) ([]fs.DirEntry, error) {
		if errno, ok := fail[p]; ok {
			return nil, &fs.PathError{Op: "open", Path: p, Err: errno}
		}
		return wasDir(ctx, p, timeout)
	}
	statLook = func(ctx context.Context, p string, timeout time.Duration) (fs.FileInfo, error) {
		if errno, ok := fail[p]; ok {
			return nil, &fs.PathError{Op: "stat", Path: p, Err: errno}
		}
		return wasStat(ctx, p, timeout)
	}
}

// bookOnDisk reads the share-fault rule on its own looks and resolveBook's
// (server#360): a book's folder listing or stat failing as a share does (EIO,
// ENOTCONN: unknown for now; a permission: present, its state kept) is never
// the book gone, as a listing read as empty once made it; only the path gone
// (ENOENT, or ENOTDIR: a folder above it replaced by a file) is. Not
// parallel: it swaps the looks.
func TestBookOnDiskReadsAShareFaultAsNoAnswer(t *testing.T) {
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "Loose.mp3", make([]byte, 1<<10))
	writeFile(t, root, "Author/Book/01.mp3", make([]byte, 1<<10))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	book := filepath.Join(root, "Author", "Book")
	for _, c := range []struct {
		name, rel, at string
		errno         syscall.Errno
		want          onDisk
	}{
		{"the library folder's listing EIO", "Loose.mp3", root, syscall.EIO, diskUnknown},
		{"the library folder's listing ENOTCONN", "Loose.mp3", root, syscall.ENOTCONN, diskUnknown},
		{"the library folder's listing EACCES", "Loose.mp3", root, syscall.EACCES, diskPresent},
		{"the library folder's listing ENXIO", "Loose.mp3", root, syscall.ENXIO, diskPresent},
		{"the library folder's listing ENOENT", "Loose.mp3", root, syscall.ENOENT, diskGone},
		{"its own listing EIO", "Author/Book", book, syscall.EIO, diskUnknown},
		{"its own listing ENXIO", "Author/Book", book, syscall.ENXIO, diskPresent},
		{"its own stat EPERM", "Author/Book", book, syscall.EPERM, diskPresent},
		{"its own stat ENOTDIR", "Author/Book", book, syscall.ENOTDIR, diskGone},
	} {
		t.Run(c.name, func(t *testing.T) {
			failLooks(t, map[string]syscall.Errno{c.at: c.errno})
			if got := s.bookOnDisk(ctx, *lib, c.rel); got != c.want {
				t.Errorf("bookOnDisk(%s) with %s = %v, want %v", c.rel, c.name, got, c.want)
			}
		})
	}
	if got := s.bookOnDisk(ctx, *lib, "Loose.mp3"); got != diskPresent {
		t.Errorf("bookOnDisk with every look answering = %v, want present", got)
	}
}

// A disc folder whose listing fails as a share does (EIO) in a scan's walk is
// as unknown as one that didn't answer: the run is partial and the disc set it
// is a disc of is not worked out from the discs that listed, so the disc that
// did keeps its split parent rather than reading as a book of its own. The
// path is recorded unreadable. Not parallel: it swaps the looks.
func TestAScanKeepsADiscSetWhoseDiscFailsToList(t *testing.T) {
	cat, ctx := newHealthCatalog(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	audio := testAudio(t)
	writeFile(t, root, "A/Book/CD1/01.m4b", audio)
	writeFile(t, root, "A/Book/CD2/01.m4b", audio)
	writeFile(t, root, "A/Other/01.m4b", audio)
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	if res := scanWithin(t, s, *lib, 20*time.Second); res.Books != 3 {
		t.Fatalf("%d books, want both discs and the other book", res.Books)
	}
	want := map[string]string{"A/Book/CD1": "A/Book", "A/Book/CD2": "A/Book"}
	if got := splitParents(t, cat, lib.ID); len(got) != 2 || got["A/Book/CD1"] != "A/Book" {
		t.Fatalf("split parents %v, want %v", got, want)
	}

	failLooks(t, map[string]syscall.Errno{filepath.Join(root, "A", "Book", "CD2"): syscall.EIO})
	res := scanWithin(t, s, *lib, 20*time.Second)
	if !res.Partial || res.Removed != 0 {
		t.Errorf("scan with a disc's listing EIO: partial %v, removed %d; want a partial run, none removed", res.Partial, res.Removed)
	}
	if got := splitParents(t, cat, lib.ID); got["A/Book/CD1"] != "A/Book" || got["A/Book/CD2"] != "A/Book" {
		t.Errorf("split parents after a disc's listing failed %v, want %v kept", got, want)
	}
}

// A cover look failing as a share does that a retry may get past (EIO,
// ENOTCONN) is a look given up on, so the read keeps the stored book and its
// cover (givenUp.keep), never "no cover"; a permission, which stays, is an
// answer as before. Not parallel: it swaps the looks.
func TestCoverLookReadsATransientFaultAsNoAnswer(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "Book/cover.jpg", []byte("img"))
	writeFile(t, root, "Book/Front.png", []byte("img"))
	dir := filepath.Join(root, "Book")
	gone := map[string]syscall.Errno{}
	for _, name := range coverNames {
		gone[filepath.Join(dir, name)] = syscall.ENOENT
	}
	listingFails := func(errno syscall.Errno) map[string]syscall.Errno {
		fail := maps.Clone(gone)
		fail[dir] = errno
		return fail
	}
	for _, c := range []struct {
		name      string
		fail      map[string]syscall.Errno
		want      string
		givenUpOn bool
	}{
		{"cover stat EIO", map[string]syscall.Errno{filepath.Join(dir, "cover.jpg"): syscall.EIO}, "", true},
		{"cover stat ENOTCONN", map[string]syscall.Errno{filepath.Join(dir, "cover.jpg"): syscall.ENOTCONN}, "", true},
		{"cover stat ECONNRESET", map[string]syscall.Errno{filepath.Join(dir, "cover.jpg"): syscall.ECONNRESET}, "", true},
		{"folder listing EIO", listingFails(syscall.EIO), "", true},
		// An answer: the listing then picks the image, as before.
		{"cover stat EACCES", map[string]syscall.Errno{filepath.Join(dir, "cover.jpg"): syscall.EACCES}, "Book/cover.jpg", false},
		{"cover stat ENXIO", map[string]syscall.Errno{filepath.Join(dir, "cover.jpg"): syscall.ENXIO}, "Book/cover.jpg", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			failLooks(t, c.fail)
			var g givenUp
			got := coverLook{ctx: t.Context(), root: root, timeout: time.Minute, gaveUp: &g}.find(dir, true)
			if got != c.want || (g.err != nil) != c.givenUpOn {
				t.Errorf("cover %q, given up on %v; want %q, given up on %v", got, g.err, c.want, c.givenUpOn)
			}
		})
	}
}

// A caller who asks for a book (an on-demand index, an admin's Re-read, the
// Bridge's IndexMoved) whose folder refuses the server's user for good (a
// permission on its stat or its listing) hears there is no book to index
// there, as before the share-fault rule, never a look given up on (the API's
// 503 share_not_answering, which says to try again): only a fault a retry may
// get past is one. bookOnDisk still reads the same refusal as the book
// present (TestBookOnDiskReadsAShareFaultAsNoAnswer). Not parallel: it swaps
// the looks.
func TestAskedBookRefusedForGoodIsNotIndexable(t *testing.T) {
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "Author/Book/01.mp3", make([]byte, 1<<10))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	book := filepath.Join(root, "Author", "Book")
	asks := map[string]func() error{
		"IndexPath":   func() error { _, err := s.IndexPath(ctx, *lib, "Author/Book"); return err },
		"ReindexPath": func() error { _, err := s.ReindexPath(ctx, *lib, "Author/Book"); return err },
		"IndexMoved":  func() error { _, err := s.IndexMoved(ctx, *lib, "Author/Book", ""); return err },
	}
	for _, c := range []struct {
		name    string
		errno   syscall.Errno
		refused bool
	}{
		{"EACCES", syscall.EACCES, true},
		{"EPERM", syscall.EPERM, true},
		{"EIO", syscall.EIO, false},
		{"ESTALE", syscall.ESTALE, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The stat and the listing both fail at the folder: the stat is
			// met first, then (its error lifted) the listing.
			for _, look := range []string{"stat", "listing"} {
				fail := map[string]syscall.Errno{book: c.errno}
				failLooks(t, fail)
				if look == "listing" {
					wasStat := statLook
					statLook = func(ctx context.Context, p string, timeout time.Duration) (fs.FileInfo, error) {
						return metadata.StatBounded(ctx, p, timeout)
					}
					t.Cleanup(func() { statLook = wasStat })
				}
				for name, ask := range asks {
					err := ask()
					if c.refused && (!errors.Is(err, ErrNotIndexable) || metadata.GaveUp(err)) {
						t.Errorf("%s with the folder's %s %s: %v, want ErrNotIndexable, no look given up on", name, look, c.name, err)
					}
					if !c.refused && !metadata.GaveUp(err) {
						t.Errorf("%s with the folder's %s %s: %v, want a look given up on", name, look, c.name, err)
					}
				}
			}
		})
	}
	if _, err := s.IndexPath(ctx, *lib, "Author/Book"); err != nil {
		t.Errorf("IndexPath with every look answering: %v", err)
	}
}
