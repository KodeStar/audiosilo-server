package library

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// stampAddedAt stamps a book with the earliest added_at of its files, skips a
// file gone since discovery, and stamps a book with none left as now.
func TestStampAddedAt(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	lib := catalog.Library{Root: root}
	// 02.mp3, listed after 01.mp3, is the earliest file: its mtime is older (a
	// Mac moves its birth time back with it), and 01.mp3 is made a second later
	// for a filesystem whose birth times can't be set (Linux).
	writeFile(t, root, "A/Book/02.mp3", []byte("x"))
	old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(root, "A/Book/02.mp3"), old, old); err != nil {
		t.Fatal(err)
	}
	// Past the next second by more than a file timestamp's coarse clock lags.
	time.Sleep(time.Until(time.Unix(time.Now().Unix()+1, int64(100*time.Millisecond))))
	writeFile(t, root, "A/Book/01.mp3", []byte("x"))
	writeFile(t, root, "loose.mp3", []byte("x"))
	of := func(rel string) string {
		t.Helper()
		abs := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(abs)
		if err != nil {
			t.Fatal(err)
		}
		return addedAt(abs, info)
	}
	want := of("A/Book/02.mp3")
	if first := of("A/Book/01.mp3"); want >= first {
		t.Fatalf("02.mp3 (%s) isn't earlier than 01.mp3 (%s): nothing tells the earliest file", want, first)
	}

	folder := catalog.Book{RelPath: "A/Book", IsFolder: true, Files: []catalog.BookFile{
		{RelPath: "A/Book/gone.mp3"}, {RelPath: "A/Book/01.mp3"}, {RelPath: "A/Book/02.mp3"},
	}}
	stampAddedAt(lib, &folder)
	if folder.AddedAt != want {
		t.Errorf("folder book added_at = %q, want the earliest file's %q", folder.AddedAt, want)
	}

	loose := catalog.Book{RelPath: "loose.mp3"}
	stampAddedAt(lib, &loose)
	if loose.AddedAt != of("loose.mp3") {
		t.Errorf("single-file book added_at = %q, want %q", loose.AddedAt, of("loose.mp3"))
	}

	before := time.Now().UTC().Add(-time.Second)
	gone := catalog.Book{RelPath: "Gone", IsFolder: true, Files: []catalog.BookFile{{RelPath: "Gone/x.mp3"}}}
	stampAddedAt(lib, &gone)
	at, err := time.Parse(time.RFC3339, gone.AddedAt)
	if err != nil || at.Before(before.Truncate(time.Second)) {
		t.Errorf("a book with no files left added_at = %q (%v), want now", gone.AddedAt, err)
	}
}

// The added_at stamped from discovery's lstat (Book.FoundAdded) is the one a
// fresh lstat of each file gives: a folder book whose earliest file is not its
// first, a single-file book and a book whose file is a link (an lstat reads
// the link; on Linux statx the target's birth time, as before).
func TestStampAddedAtFromDiscovery(t *testing.T) {
	t.Parallel()
	root, elsewhere := t.TempDir(), t.TempDir()
	writeFile(t, root, "A/Book/02.mp3", []byte("x"))
	old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(root, "A/Book/02.mp3"), old, old); err != nil {
		t.Fatal(err)
	}
	writeFile(t, elsewhere, "target.mp3", []byte("x"))
	if err := os.Chtimes(filepath.Join(elsewhere, "target.mp3"), old, old); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(time.Unix(time.Now().Unix()+1, int64(100*time.Millisecond))))
	writeFile(t, root, "A/Book/01.mp3", []byte("x"))
	writeFile(t, root, "loose.mp3", []byte("x"))
	if err := os.MkdirAll(filepath.Join(root, "B/Linked"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "target.mp3"), filepath.Join(root, "B/Linked/01.mp3")); err != nil {
		t.Fatal(err)
	}
	lib := catalog.Library{Root: root}
	books, _, _, err := discoverAuto(context.Background(), lib, nil, nil, slog.New(slog.DiscardHandler), nil, nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(books) != 3 {
		t.Fatalf("discovered %d books, want 3", len(books))
	}
	for _, b := range books {
		want := len(b.Files)
		if !b.IsFolder {
			want = 1
		}
		if len(b.FoundAdded) != want {
			t.Fatalf("%s: %d found times for %d files", b.RelPath, len(b.FoundAdded), want)
		}
		fresh := *b
		fresh.FoundAdded = nil // the old path: an lstat (and on Linux a statx) a file
		stampAddedAt(lib, &fresh)
		stampAddedAt(lib, b)
		if b.AddedAt != fresh.AddedAt {
			t.Errorf("%s: added_at from discovery %q, from a fresh lstat %q", b.RelPath, b.AddedAt, fresh.AddedAt)
		}
	}
}

// The found time is used as it is where the walk's lstat is all addedAt reads
// (no system call: a file gone since keeps it); on Linux statx still reads the
// file, and one gone since discovery is left to the lstat, which skips it.
func TestStampAddedAtTakesNoSecondLstat(t *testing.T) {
	t.Parallel()
	lib := catalog.Library{Root: t.TempDir()}
	found := time.Date(2003, 4, 5, 6, 7, 8, 0, time.UTC)
	b := catalog.Book{RelPath: "gone.mp3", FoundAdded: []int64{found.Unix()}}
	stampAddedAt(lib, &b)
	if runtime.GOOS == "linux" {
		if b.AddedAt == found.Format(time.RFC3339) {
			t.Errorf("a file gone since discovery stamped %q, want now (as an lstat finding it gone)", b.AddedAt)
		}
		return
	}
	if want := found.Format(time.RFC3339); b.AddedAt != want {
		t.Errorf("added_at = %q, want discovery's %q, read without looking at the file", b.AddedAt, want)
	}
}
