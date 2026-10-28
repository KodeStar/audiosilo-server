package library

import (
	"os"
	"path/filepath"
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
	writeFile(t, root, "A/Book/01.mp3", []byte("x"))
	writeFile(t, root, "A/Book/02.mp3", []byte("x"))
	writeFile(t, root, "loose.mp3", []byte("x"))
	old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(root, "A/Book/02.mp3"), old, old); err != nil {
		t.Fatal(err)
	}
	of := func(rel string) string {
		t.Helper()
		abs := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(abs)
		if err != nil {
			t.Fatal(err)
		}
		return addedAt(abs, info)
	}
	want := min(of("A/Book/01.mp3"), of("A/Book/02.mp3"))

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
