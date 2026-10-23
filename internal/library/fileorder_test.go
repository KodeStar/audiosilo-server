package library

import (
	"io/fs"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// TestNaturalFileOrder (D2, #160): a folder book plays its files in the order
// people number them, whatever the punctuation after the number ("1- ", "1.",
// "1. ", "1_"), never byte order ("10-" before "2-"); a disc-track name keeps
// its disc first ("1-10" before "2-1").
func TestNaturalFileOrder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		files []string // in the order wanted
	}{
		{"dash", []string{"1- First Part.mp3", "2- Second Part.mp3", "10- Tenth Part.mp3"}},
		{"dot no space", []string{"1.First Part.mp3", "2.Second Part.mp3", "10.Tenth Part.mp3"}},
		{"dot space", []string{"1. First Part.mp3", "2. Second Part.mp3", "10. Tenth Part.mp3"}},
		{"underscore", []string{"Part_1.mp3", "Part_2.mp3", "Part_10.mp3"}},
		{"disc-track", []string{"1-1.mp3", "1-2.mp3", "1-10.mp3", "2-1.mp3", "2-10.mp3"}},
		{"padded and not", []string{"01 Intro.mp3", "1 Intro.mp3", "2 Next.mp3"}}, // a tie keeps byte order
	} {
		got := make([]audioFile, len(tc.files))
		for i, f := range tc.files {
			got[len(tc.files)-1-i] = audioFile{abs: f, de: fakeEntry(f)}
		}
		sortFileOrder(got)
		for i, f := range got {
			if f.de.Name() != tc.files[i] {
				t.Errorf("%s: order = %v, want %v", tc.name, names(got), tc.files)
				break
			}
		}
	}
}

// TestScanReordersBookIndexedInByteOrder (D2): a folder book indexed before
// natural order (its files stored in byte order) is read again in natural order
// by the next scan though no file changed; once in order, a scan leaves it be.
func TestScanReordersBookIndexedInByteOrder(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	want := []string{"A/Book/1- First.mp3", "A/Book/2- Second.mp3", "A/Book/10- Tenth.mp3"}
	for _, f := range want {
		writeFile(t, root, f, []byte("not really audio"))
	}
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	order := func() []string {
		t.Helper()
		b, err := cat.GetBookByPath(ctx, lib.ID, "A/Book")
		if err != nil {
			t.Fatal(err)
		}
		return filePaths(b)
	}
	if got := order(); !slices.Equal(got, want) {
		t.Fatalf("first scan order = %q, want %q", got, want)
	}
	// As an index from before natural order has it: 1-, 10-, 2-.
	for seq, f := range []string{want[0], want[2], want[1]} {
		if _, err := db.ExecContext(ctx, `UPDATE book_files SET seq = ? WHERE rel_path = ?`, seq, f); err != nil {
			t.Fatal(err)
		}
	}
	res, err := s.Scan(ctx, *lib)
	if err != nil {
		t.Fatal(err)
	}
	if got := order(); !slices.Equal(got, want) || res.Updated != 1 {
		t.Fatalf("rescan order = %q (updated %d), want %q (updated 1)", got, res.Updated, want)
	}
	if res, err = s.Scan(ctx, *lib); err != nil || res.Updated != 0 || res.Added != 0 {
		t.Fatalf("a scan of a book in order = %+v, %v; want nothing re-indexed", res.ScanCounts, err)
	}
}

// namedEntry is a directory entry that has only a name: all sortFileOrder reads.
type namedEntry struct {
	fs.DirEntry
	name string
}

func (e namedEntry) Name() string { return e.name }

func fakeEntry(name string) fs.DirEntry { return namedEntry{name: name} }

func names(files []audioFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.de.Name()
	}
	return out
}
