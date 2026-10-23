package library

import (
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/scan"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// TestNaturalFileOrder (D2, #160): a folder book plays its files in the order
// people number them, whatever the punctuation after the number ("1- ", "1.",
// "1. ", "1_"), never byte order ("10-" before "2-"); a disc-track name keeps
// its disc first ("1-10" before "2-1"); and a part named after its number plays
// after it ("01a", "1.5").
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
		{"lettered part", []string{"01 - Prologue.mp3", "01a - Interlude.mp3", "02 - Arrival.mp3"}},
		{"decimal part", []string{"Chapter 12 - Twelve.mp3", "Chapter 12.5 - Interlude.mp3", "Chapter 13 - Thirteen.mp3"}},
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

// TestMoveBeforeReindexAfterNaturalOrder (#160): a folder book indexed in byte
// order holds the fingerprint of the file byte order put first ("Intro" before
// "chapter 1"); moved before a scan re-indexed it, by a scan or by the Bridge
// (IndexMoved), it is still found by that file and its listener's place follows.
func TestMoveBeforeReindexAfterNaturalOrder(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	for _, b := range []string{"A/Book", "B/Book"} {
		writeFile(t, root, b+"/Intro.mp3", []byte(b+": the intro, first by bytes"))
		writeFile(t, root, b+"/chapter 1.mp3", []byte(b+": chapter one, first in natural order"))
	}
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	uid := seedUserID(t, db)
	for _, b := range []string{"A/Book", "B/Book"} {
		// As an index from before natural order has it: the Intro's fingerprint.
		if _, err := db.ExecContext(ctx, `UPDATE books SET content_hash = ? WHERE rel_path = ?`,
			fingerprintFile(filepath.Join(root, filepath.FromSlash(b), "Intro.mp3")), b); err != nil {
			t.Fatal(err)
		}
		if _, err := cat.SaveProgress(ctx, uid, catalog.Progress{Ref: catalog.Ref{LibraryID: lib.ID, Path: b}, Position: 42}); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(root, filepath.FromSlash(b)), filepath.Join(root, filepath.FromSlash(b)+" Renamed")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.IndexMoved(ctx, *lib, "B/Book Renamed", "B/Book"); err != nil {
		t.Fatal(err)
	}
	if res, err := s.Scan(ctx, *lib); err != nil || res.Moved != 1 {
		t.Fatalf("scan after the move = %+v, %v; want one move", res.ScanCounts, err)
	}
	for _, b := range []string{"A/Book Renamed", "B/Book Renamed"} {
		if p, _ := cat.GetProgress(ctx, uid, catalog.Ref{LibraryID: lib.ID, Path: b}); p == nil || p.Position != 42 {
			t.Errorf("progress at %s = %+v, want 42", b, p)
		}
	}
}

// TestNaturalLessIsMetas (#160): naturalLess is audiosilo-meta's scan.NaturalLess,
// so a book plays its files in the order the Bridge and the community metadata
// number them (the chapters their spoiler positions name).
func TestNaturalLessIsMetas(t *testing.T) {
	t.Parallel()
	check := func(a, b string) {
		t.Helper()
		if got, want := naturalLess(a, b), scan.NaturalLess(a, b); got != want {
			t.Errorf("naturalLess(%q, %q) = %v, scan.NaturalLess says %v", a, b, got, want)
		}
	}
	names := []string{
		"", "0", "00", "1", "01", "001", "2", "10", "a", "A", "Intro.mp3", "intro.mp3",
		"1- First Part.mp3", "2- Second Part.mp3", "10- Tenth Part.mp3", "1.First Part.mp3",
		"1. First Part.mp3", "01 - Prologue.mp3", "01a - Interlude.mp3", "1.5 - Interlude.mp3",
		"Chapter 1.mp3", "Chapter 1 Part 2.mp3", "chapter 1A - Aside.mp3", "Part_1.mp3",
		"Part 10.mp3", "Book_2.mp3", "Book 10.mp3", "1-10.mp3", "2-1.mp3", "007.mp3", "7.mp3",
		"CD1 Track 03.mp3", "cd 1 track 3.mp3", "Ünïcode 2.mp3", "ünïcode 10.mp3",
		"İstanbul 3.mp3", "\uff11\uff10.mp3", "\uff12.mp3", "99999999999999999999.mp3",
		"100000000000000000000.mp3",
	}
	for _, a := range names {
		for _, b := range names {
			check(a, b)
		}
	}
	r := rand.New(rand.NewPCG(160, 160))
	alphabet := []rune("aAzZ0123456789 -_.()\u00e9\u00c9\u0130\u0131")
	word := func() string {
		w := make([]rune, r.IntN(9))
		for i := range w {
			w[i] = alphabet[r.IntN(len(alphabet))]
		}
		return string(w)
	}
	for range 20000 {
		check(word(), word())
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

// TestFolderAudioDropsShadowedMKA (#161): an .mka beside a file of the same name
// in another audio format is that file's source (or copy) and stays out, as it
// did before .mka was audio, so a folder that held Book.m4b and Book.mka is still
// one book of one file (denied); an .mka of its own name, or a folder of .mka
// alone, is audio (allowed).
func TestFolderAudioDropsShadowedMKA(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		files, want []string
	}{
		{[]string{"Book.m4b", "Book.mka"}, []string{"Book.m4b"}},
		{[]string{"book.MP3", "BOOK.MKA"}, []string{"book.MP3"}},
		{[]string{"01 Intro.mp3", "02 Main.mka"}, []string{"01 Intro.mp3", "02 Main.mka"}},
		{[]string{"Book.mka"}, []string{"Book.mka"}},
		{[]string{"Book.mka", "Book.part2.mka"}, []string{"Book.mka", "Book.part2.mka"}},
	} {
		audio := make([]audioFile, len(tc.files))
		for i, f := range tc.files {
			audio[i] = audioFile{abs: f, de: fakeEntry(f)}
		}
		if got := names(folderAudio(audio)); !slices.Equal(got, tc.want) {
			t.Errorf("folderAudio(%q) = %q, want %q", tc.files, got, tc.want)
		}
	}

	// A scan of such a folder, and an on-demand index of it, has one file.
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	writeFile(t, root, "A/Book/Book.m4b", []byte("the converted copy"))
	writeFile(t, root, "A/Book/Book.mka", []byte("its source"))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	b, err := cat.GetBookByPath(ctx, lib.ID, "A/Book")
	if err != nil {
		t.Fatal(err)
	}
	if got := filePaths(b); !slices.Equal(got, []string{"A/Book/Book.m4b"}) || b.Format != "m4b" {
		t.Fatalf("scanned files = %q (format %s), want only the m4b", got, b.Format)
	}
	if b, err = s.IndexPath(ctx, *lib, "A/Book"); err != nil || !slices.Equal(filePaths(b), []string{"A/Book/Book.m4b"}) {
		t.Fatalf("on-demand files = %v, %v; want only the m4b", b, err)
	}
}
