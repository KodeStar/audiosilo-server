package library

import (
	"fmt"
	"log/slog"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/store"
)

// Disc folders join in natural order, ties broken by path, so folders whose names
// read alike never interleave their files.
func TestDiscOrder(t *testing.T) {
	want := []string{
		"Book/CD01", // reads as "CD1"; the path breaks the tie
		"Book/CD1",
		"Book/cd 2", // case and spaces don't count
		"Book/CD10",
		"Book/Disc 3",
	}
	got := slices.Clone(want)
	slices.Reverse(got)
	slices.SortFunc(got, discOrder)
	if !slices.Equal(got, want) {
		t.Fatalf("order = %q\nwant %q", got, want)
	}
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"Part 2", "Part 10", -1},
		{"Disc 01", "Disc 1", 0},
		{"CD2", "cd 2", 0},
		{"a10b", "a9c", 1},
		{"Track", "Track 1", -1},
	} {
		if got := naturalCompare(tc.a, tc.b); got != tc.want {
			t.Errorf("naturalCompare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestJoinedPartTitle(t *testing.T) {
	for _, tc := range []struct{ book, file, want string }{
		{"A/Book", "A/Book/CD2/03 - The Dragon.mp3", "CD2 - The Dragon"},
		{"A/Book", "A/Book/01 - Intro.mp3", "Intro"}, // a plain folder book is unchanged
	} {
		if got := joinedPartTitle(tc.book, tc.file); got != tc.want {
			t.Errorf("joinedPartTitle(%q) = %q, want %q", tc.file, got, tc.want)
		}
	}
}

// bookPaths lists a library's indexed book paths, sorted.
func bookPaths(t *testing.T, cat *catalog.Catalog, libID int64) []string {
	t.Helper()
	page, err := cat.ListBooks(t.Context(), catalog.ListOptions{LibraryID: libID, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, b := range page.Books {
		out = append(out, b.RelPath)
	}
	sort.Strings(out)
	return out
}

func filePaths(b *catalog.Book) []string {
	var out []string
	for _, f := range b.Files {
		out = append(out, f.RelPath)
	}
	return out
}

// A `book` override on a folder whose audio is only in its disc folders reads them
// as one book, the folders in natural order, with hidden and ignored files left out;
// no override (or removing it) leaves each disc its own book, as before. The join
// is a reshape: nothing is counted or announced as added. Without ffprobe no disc's
// length is known, so only the first disc's offset is (0): its listening state
// carries over, and the later discs' stays on their own paths, logged as such.
func TestJoinedDiscFolders(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	audio := testAudio(t)
	for _, p := range []string{
		"Cowell/Dragonese/CD1/01.m4b",
		"Cowell/Dragonese/CD1/02.m4b",
		"Cowell/Dragonese/CD2/01.m4b",
		"Cowell/Dragonese/CD10/01.m4b",
		"Cowell/Dragonese/CD2/.hidden.m4b",      // hidden file
		"Cowell/Dragonese/.trash/old.m4b",       // hidden folder
		"Cowell/Dragonese/CD1/promo.sample.m4b", // ignored
		"Cowell/Other/01.m4b",
	} {
		writeFile(t, root, p, audio)
	}
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root, IgnorePatterns: []string{"*.sample.m4b"}})
	s := NewScanner(cat, "", slog.Default())
	scan := func() *ScanResult {
		t.Helper()
		res, err := s.Scan(ctx, *lib)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	separate := []string{"Cowell/Dragonese/CD1", "Cowell/Dragonese/CD10", "Cowell/Dragonese/CD2", "Cowell/Other"}

	// No override: one book per disc folder, as always.
	scan()
	if got := bookPaths(t, cat, lib.ID); !slices.Equal(got, separate) {
		t.Fatalf("without an override: %q", got)
	}
	ann := namedUser(t, db, "ann")
	ref := func(p string) catalog.Ref { return catalog.Ref{LibraryID: lib.ID, Path: p} }
	discPos := map[string]float64{"Cowell/Dragonese/CD1": 1, "Cowell/Dragonese/CD2": 2, "Cowell/Dragonese/CD10": 3}
	for p, pos := range discPos {
		if b, err := cat.GetBookByPath(ctx, lib.ID, p); err != nil || b.Duration != 0 {
			t.Fatalf("disc %s = %+v (err %v), want no known length", p, b, err)
		}
		if _, err := cat.SaveProgress(ctx, ann, catalog.Progress{Ref: ref(p), Position: pos}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cat.AddBookmark(ctx, ann, catalog.Bookmark{Ref: ref("Cowell/Dragonese/CD2"), Position: 2}); err != nil {
		t.Fatal(err)
	}

	if err := cat.SetFolderOverride(ctx, lib.ID, "Cowell/Dragonese", catalog.OverrideBook); err != nil {
		t.Fatal(err)
	}
	res := scan()
	if got := bookPaths(t, cat, lib.ID); !slices.Equal(got, []string{"Cowell/Dragonese", "Cowell/Other"}) {
		t.Fatalf("joined: %q", got)
	}
	if res.Removed != 0 || res.Added != 0 || len(res.AddedTitles) != 0 {
		t.Errorf("joining counted the discs as removed or the book as added: %+v %q", res.ScanCounts, res.AddedTitles)
	}
	// Disc 1 (offset 0) carried; the later discs, whose offsets are unknown, kept
	// their state, and every disc is logged joined, the later ones saying why.
	if p, _ := cat.GetProgress(ctx, ann, ref("Cowell/Dragonese")); p == nil || p.Position != 1 {
		t.Errorf("joined progress = %+v, want disc 1's (1)", p)
	}
	if p, _ := cat.GetProgress(ctx, ann, ref("Cowell/Dragonese/CD1")); p != nil {
		t.Errorf("disc 1's progress stayed behind: %+v", p)
	}
	for _, d := range []string{"Cowell/Dragonese/CD2", "Cowell/Dragonese/CD10"} {
		if p, _ := cat.GetProgress(ctx, ann, ref(d)); p == nil || p.Position != discPos[d] {
			t.Errorf("%s's progress = %+v, want it left at %v", d, p, discPos[d])
		}
	}
	if bms, _ := cat.ListBookmarks(ctx, ann, ref("Cowell/Dragonese/CD2")); len(bms) != 1 || bms[0].Position != 2 {
		t.Errorf("disc 2's bookmarks = %+v, want them left on it", bms)
	}
	if bms, _ := cat.ListBookmarks(ctx, ann, ref("Cowell/Dragonese")); len(bms) != 0 {
		t.Errorf("a bookmark of an unplaced disc landed on the joined book: %+v", bms)
	}
	joinCodes := map[string]string{}
	for _, e := range res.Log {
		if e.Kind == "removed" {
			t.Errorf("a joined disc is logged as removed: %+v", e)
		}
		if e.Kind == "joined" && e.To == "Cowell/Dragonese" {
			joinCodes[e.Path] = e.Code
		}
	}
	if want := map[string]string{
		"Cowell/Dragonese/CD1":  "",
		"Cowell/Dragonese/CD2":  joinLengthUnknown,
		"Cowell/Dragonese/CD10": joinLengthUnknown,
	}; !maps.Equal(joinCodes, want) {
		t.Errorf("joined events = %v, want %v", joinCodes, want)
	}
	b, err := cat.GetBookByPath(ctx, lib.ID, "Cowell/Dragonese")
	if err != nil {
		t.Fatal(err)
	}
	wantFiles := []string{
		"Cowell/Dragonese/CD1/01.m4b",
		"Cowell/Dragonese/CD1/02.m4b",
		"Cowell/Dragonese/CD2/01.m4b",
		"Cowell/Dragonese/CD10/01.m4b",
	}
	if got := filePaths(b); !b.IsFolder || !slices.Equal(got, wantFiles) {
		t.Fatalf("joined files = %q", got)
	}
	// Every chapter plays a real file of the book, on one timeline.
	if len(b.Chapters) < len(wantFiles) {
		t.Fatalf("chapters = %+v", b.Chapters)
	}
	for _, ch := range b.Chapters {
		if !slices.Contains(wantFiles, ch.FilePath) {
			t.Errorf("chapter %+v plays a file outside the book", ch)
		}
	}

	// On demand, a disc folder, a file in it and the folder itself all resolve to
	// the joined book.
	for _, p := range []string{"Cowell/Dragonese", "Cowell/Dragonese/CD2", "Cowell/Dragonese/CD10/01.m4b"} {
		got, err := s.IndexPath(ctx, *lib, p)
		if err != nil || got.RelPath != "Cowell/Dragonese" || len(got.Files) != len(wantFiles) {
			t.Errorf("IndexPath(%q) = %+v (err %v), want the joined book", p, got, err)
		}
	}
	// What the rules leave out is not in it on demand either.
	if _, err := s.IndexPath(ctx, *lib, "Cowell/Dragonese/CD1/promo.sample.m4b"); err == nil {
		t.Error("an ignored file resolved to a book")
	}

	// Removing the override: one book per disc again. That is a reshape too: the
	// discs aren't counted or announced as added, and the joined book is logged as
	// split, not removed.
	if err := cat.DeleteFolderOverride(ctx, lib.ID, "Cowell/Dragonese"); err != nil {
		t.Fatal(err)
	}
	res = scan()
	if got := bookPaths(t, cat, lib.ID); !slices.Equal(got, separate) {
		t.Fatalf("after removing the override: %q", got)
	}
	if res.Added != 0 || len(res.AddedTitles) != 0 || res.Removed != 0 {
		t.Errorf("splitting counted the discs as added or the book as removed: %+v %q", res.ScanCounts, res.AddedTitles)
	}
	var split []string
	for _, e := range res.Log {
		if e.Kind == "removed" {
			t.Errorf("the split book is logged as removed: %+v", e)
		}
		if e.Kind == "split" {
			split = append(split, e.Path)
		}
	}
	if !slices.Equal(split, []string{"Cowell/Dragonese"}) {
		t.Errorf("split logged for %q, want the joined folder", split)
	}
	// Split again, the discs whose state never moved have it back.
	if p, _ := cat.GetProgress(ctx, ann, ref("Cowell/Dragonese/CD2")); p == nil || p.Position != 2 {
		t.Errorf("disc 2's progress after the split = %+v, want 2", p)
	}
}

// A joined folder renamed away from its `book` override reads as one book per disc
// again; its first disc shares the joined book's fingerprint but is no move of it,
// so the joined book's whole-book progress doesn't land on that one disc.
func TestRenamedJoinedFolderIsNoMoveToADisc(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	audio := testAudio(t)
	for _, p := range []string{"Book/CD1/01.m4b", "Book/CD2/01.m4b"} {
		writeFile(t, root, p, audio)
	}
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if err := cat.SetFolderOverride(ctx, lib.ID, "Book", catalog.OverrideBook); err != nil {
		t.Fatal(err)
	}
	s := NewScanner(cat, "", slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if got := bookPaths(t, cat, lib.ID); !slices.Equal(got, []string{"Book"}) {
		t.Fatalf("joined books = %q", got)
	}
	uid := namedUser(t, db, "ann")
	if _, err := cat.SaveProgress(ctx, uid, catalog.Progress{Ref: catalog.Ref{LibraryID: lib.ID, Path: "Book"},
		Position: 99, Finished: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "Book"), filepath.Join(root, "Renamed")); err != nil {
		t.Fatal(err)
	}
	res, err := s.Scan(ctx, *lib)
	if err != nil {
		t.Fatal(err)
	}
	if got := bookPaths(t, cat, lib.ID); !slices.Equal(got, []string{"Renamed/CD1", "Renamed/CD2"}) {
		t.Fatalf("books after the rename = %q", got)
	}
	if res.Moved != 0 {
		t.Errorf("the joined book was paired with a disc as a move: %+v", res.ScanCounts)
	}
	for _, d := range []string{"Renamed/CD1", "Renamed/CD2"} {
		if p, _ := cat.GetProgress(ctx, uid, catalog.Ref{LibraryID: lib.ID, Path: d}); p != nil {
			t.Errorf("the joined book's progress landed on %s: %+v", d, p)
		}
	}
}

// A folder book with audio of its own was never joined: when its own files go,
// the books in its disc folders beside them stay as they were and the folder's
// book is removed, not split.
func TestRemovedFolderWithDiscsIsNoSplit(t *testing.T) {
	cat, ctx := newHealthCatalog(t)
	root := t.TempDir()
	audio := testAudio(t)
	for _, p := range []string{"Book/01.m4b", "Book/CD1/01.m4b", "Book/CD2/01.m4b"} {
		writeFile(t, root, p, audio)
	}
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "Book", "01.m4b")); err != nil {
		t.Fatal(err)
	}
	res, err := s.Scan(ctx, *lib)
	if err != nil {
		t.Fatal(err)
	}
	if got := bookPaths(t, cat, lib.ID); !slices.Equal(got, []string{"Book/CD1", "Book/CD2"}) {
		t.Fatalf("books = %q", got)
	}
	if res.Removed != 1 || res.Added != 0 {
		t.Fatalf("counts = %+v, want one removed", res.ScanCounts)
	}
	for _, e := range res.Log {
		if e.Kind == "split" {
			t.Errorf("a folder book that was never joined is logged as split: %+v", e)
		}
	}
}

// joinParts places each disc on the joined timeline from the joined book's file
// lengths, falling back to the disc's indexed length when its files' are unknown,
// and marks the disc that ends the book; a newly found disc ends it instead. A disc
// after one of no known length has no known offset: it is unplaced, and the book's
// length is unknown.
func TestJoinParts(t *testing.T) {
	joined := &catalog.Book{Files: []catalog.BookFile{
		{RelPath: "B/CD1/01.mp3", Duration: 10},
		{RelPath: "B/CD1/02.mp3", Duration: 5},
		{RelPath: "B/CD2/01.mp3"}, // unknown length
		{RelPath: "B/CD3/01.mp3", Duration: 7},
	}}
	sigs := map[string]catalog.Signature{"B/CD2": {Duration: 20}}
	parts, total := joinParts(joined, []string{"B/CD3", "B/CD1", "B/CD2"}, sigs)
	want := []catalog.JoinPart{
		{Path: "B/CD1", Offset: 0, Duration: 15},
		{Path: "B/CD2", Offset: 15, Duration: 20},
		{Path: "B/CD3", Offset: 35, Duration: 7, Last: true},
	}
	if !slices.Equal(parts, want) || total != 42 {
		t.Fatalf("parts = %+v total %v, want %+v total 42", parts, total, want)
	}
	// CD3 wasn't indexed before: CD2 doesn't end the book.
	parts, _ = joinParts(joined, []string{"B/CD1", "B/CD2"}, sigs)
	if len(parts) != 2 || parts[1].Last {
		t.Fatalf("parts = %+v, want CD2 not last", parts)
	}
	// CD2's length unknown everywhere: CD2 itself is placed (CD1's length is known),
	// CD3 after it isn't.
	parts, total = joinParts(joined, []string{"B/CD1", "B/CD2", "B/CD3"}, nil)
	want = []catalog.JoinPart{
		{Path: "B/CD1", Offset: 0, Duration: 15},
		{Path: "B/CD2", Offset: 15},
		{Path: "B/CD3", Offset: 15, Duration: 7, Last: true, Unplaced: true},
	}
	if !slices.Equal(parts, want) || total != 0 {
		t.Fatalf("parts = %+v total %v, want %+v total 0", parts, total, want)
	}
	// No length known (ffprobe off): only the first disc is placed.
	parts, total = joinParts(&catalog.Book{Files: []catalog.BookFile{
		{RelPath: "B/CD1/01.mp3"}, {RelPath: "B/CD2/01.mp3"}, {RelPath: "B/CD3/01.mp3"},
	}}, []string{"B/CD1", "B/CD2", "B/CD3"}, nil)
	want = []catalog.JoinPart{
		{Path: "B/CD1"},
		{Path: "B/CD2", Unplaced: true},
		{Path: "B/CD3", Last: true, Unplaced: true},
	}
	if !slices.Equal(parts, want) || total != 0 {
		t.Fatalf("parts = %+v total %v, want %+v total 0", parts, total, want)
	}
	// A disc with only some of its files' lengths known (one failed to probe) is
	// placed by its indexed length, not the short sum of the rest, and the joined
	// book's own (file-summed) length doesn't shorten the total.
	parts, total = joinParts(&catalog.Book{Duration: 13, Files: []catalog.BookFile{
		{RelPath: "B/CD1/01.mp3", Duration: 4}, {RelPath: "B/CD1/02.mp3"}, {RelPath: "B/CD2/01.mp3", Duration: 9},
	}}, []string{"B/CD1", "B/CD2"}, map[string]catalog.Signature{"B/CD1": {Duration: 10}})
	want = []catalog.JoinPart{
		{Path: "B/CD1", Offset: 0, Duration: 10},
		{Path: "B/CD2", Offset: 10, Duration: 9, Last: true},
	}
	if !slices.Equal(parts, want) || total != 19 {
		t.Fatalf("parts = %+v total %v, want %+v total 19", parts, total, want)
	}
	// Without an indexed length either, the disc after it is unplaced.
	parts, _ = joinParts(&catalog.Book{Files: []catalog.BookFile{
		{RelPath: "B/CD1/01.mp3", Duration: 4}, {RelPath: "B/CD1/02.mp3"}, {RelPath: "B/CD2/01.mp3", Duration: 9},
	}}, []string{"B/CD1", "B/CD2"}, nil)
	if len(parts) != 2 || !parts[1].Unplaced {
		t.Fatalf("parts = %+v, want CD2 unplaced", parts)
	}
}

// The joined book's cover is its folder's own image first, then the first disc's.
func TestJoinedCover(t *testing.T) {
	cat, ctx := newHealthCatalog(t)
	root := t.TempDir()
	audio := testAudio(t)
	writeFile(t, root, "A/Book/CD1/01.m4b", audio)
	writeFile(t, root, "A/Book/CD2/01.m4b", audio)
	writeFile(t, root, "A/Book/CD1/folder.jpg", []byte("jpg"))
	writeFile(t, root, "A/Book/CD2/cover.jpg", []byte("jpg"))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if err := cat.SetFolderOverride(ctx, lib.ID, "A/Book", catalog.OverrideBook); err != nil {
		t.Fatal(err)
	}
	s := NewScanner(cat, "", slog.Default())
	cover := func() string {
		t.Helper()
		b, err := s.IndexPath(ctx, *lib, "A/Book")
		if err != nil {
			t.Fatal(err)
		}
		return b.CoverPath
	}
	if got := cover(); got != "A/Book/CD1/folder.jpg" {
		t.Fatalf("cover = %q, want the first disc's", got)
	}
	writeFile(t, root, "A/Book/cover.jpg", []byte("jpg"))
	if got := cover(); got != "A/Book/cover.jpg" {
		t.Fatalf("cover = %q, want the folder's own", got)
	}
}

// Only a disc set joins: a `book` override anywhere else keeps the meaning it had
// before joining existed (an author's or a series' folder from the old detection
// dialog reads as nothing; a folder with its own audio is its own files), so the
// upgrade re-shapes no books and carries no listener's state.
func TestJoinOnlyDiscSets(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	audio := testAudio(t)
	for _, p := range []string{
		"Pratchett/Mort/01.m4b", // an author: a book, a series of books
		"Pratchett/Watch/Guards/01.m4b",
		"Pratchett/Watch/Feet of Clay/01.m4b",
		"Wight/Cradle/Unsouled/01.m4b", // a series of books
		"Wight/Cradle/Soulsmith/01.m4b",
		"Discs/Book/CD1/01.m4b", // a disc set
		"Discs/Book/CD2/01.m4b",
		"Discs/Book/CD3/01.m4b",
		"Bonus/Book/CD1/01.m4b", // discs and a folder that isn't one
		"Bonus/Book/CD2/01.m4b",
		"Bonus/Book/Bonus/01.m4b",
		"Deep/Book/Part A/CD1/01.m4b", // discs deeper down
		"Deep/Book/Part A/CD2/01.m4b",
		"Deep/Book/Part B/01.m4b",
		"Mixed/intro.m4b", // audio of its own beside discs
		"Mixed/CD1/01.m4b",
		"Mixed/CD2/01.m4b",
		"Single/CD1/01.m4b", // one disc
	} {
		writeFile(t, root, p, audio)
	}
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	before := bookPaths(t, cat, lib.ID)
	ann := namedUser(t, db, "ann")
	mort := catalog.Ref{LibraryID: lib.ID, Path: "Pratchett/Mort"}
	if _, err := cat.SaveProgress(ctx, ann, catalog.Progress{Ref: mort, Position: 3}); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"", "Pratchett", "Pratchett/Watch", "Wight/Cradle", "Discs/Book", "Bonus/Book",
		"Deep/Book", "Mixed", "Single"} {
		if err := cat.SetFolderOverride(ctx, lib.ID, p, catalog.OverrideBook); err != nil {
			t.Fatal(err)
		}
	}
	res, err := s.Scan(ctx, *lib)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, p := range before {
		if !strings.HasPrefix(p, "Discs/Book/") {
			want = append(want, p)
		}
	}
	want = append(want, "Discs/Book")
	sort.Strings(want)
	if got := bookPaths(t, cat, lib.ID); !slices.Equal(got, want) {
		t.Fatalf("books = %q\nwant %q", got, want)
	}
	if !slices.Contains(before, "Mixed") || !slices.Contains(before, "Mixed/CD1") {
		t.Fatalf("before = %q, want Mixed and its discs", before)
	}
	mixed, _ := cat.GetBookByPath(ctx, lib.ID, "Mixed")
	if got := filePaths(mixed); !slices.Equal(got, []string{"Mixed/intro.m4b"}) {
		t.Fatalf("Mixed files = %q, want only its own", got)
	}
	// Only the disc set's discs were joined; nothing else moved, went or came.
	for _, e := range res.Log {
		if (e.Kind == "joined" && !strings.HasPrefix(e.Path, "Discs/Book/")) || e.Kind == "removed" || e.Kind == "moved" {
			t.Errorf("event %+v", e)
		}
	}
	if res.Added+res.Removed+res.Moved != 0 {
		t.Errorf("counts = %+v", res.ScanCounts)
	}
	if p, _ := cat.GetProgress(ctx, ann, mort); p == nil || p.Position != 3 {
		t.Fatalf("Mort's progress = %+v, want it left where it was", p)
	}
	if p, _ := cat.GetProgress(ctx, ann, catalog.Ref{LibraryID: lib.ID, Path: "Pratchett"}); p != nil {
		t.Fatalf("progress carried to the author folder: %+v", p)
	}

	// On demand the same: only a disc set's folders resolve to a joined book.
	for p, want := range map[string]string{
		"Pratchett/Mort":                "Pratchett/Mort",
		"Pratchett/Watch/Guards/01.m4b": "Pratchett/Watch/Guards",
		"Wight/Cradle/Soulsmith":        "Wight/Cradle/Soulsmith",
		"Discs/Book/CD2":                "Discs/Book",
		"Discs/Book":                    "Discs/Book",
		"Bonus/Book/CD2":                "Bonus/Book/CD2",
		"Deep/Book/Part A/CD1/01.m4b":   "Deep/Book/Part A/CD1",
		"Single/CD1":                    "Single/CD1",
	} {
		if b, err := s.IndexPath(ctx, *lib, p); err != nil || b.RelPath != want {
			t.Errorf("IndexPath(%q) = %+v (err %v), want %s", p, b, err, want)
		}
	}
	for _, p := range []string{"Pratchett", "Wight/Cradle", "Bonus/Book", "Deep/Book"} {
		if _, err := s.IndexPath(ctx, *lib, p); err == nil {
			t.Errorf("IndexPath(%q) made a book of a folder that isn't a disc set", p)
		}
	}
}

// Inside a disc, a joined book keeps the order a book of that disc alone has (the
// scanner's name order: 1, 10, 2), so a position on the disc book, offset onto the
// joined book, lands in the same file; only the disc folders go in natural order.
func TestJoinKeepsDiscFileOrder(t *testing.T) {
	ffprobe := lookFFprobe(t)
	ctx := t.Context()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	for _, disc := range []string{"CD1", "CD2"} {
		silence(t, root, "A/Book/"+disc+"/1.mp3", 1)
		silence(t, root, "A/Book/"+disc+"/2.mp3", 3)
		silence(t, root, "A/Book/"+disc+"/10.mp3", 2)
	}
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, ffprobe, slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	// fileAt is the file of b playing at pos on its timeline.
	fileAt := func(b *catalog.Book, pos float64) string {
		at := 0.0
		for _, f := range b.Files {
			if pos < at+f.Duration {
				return f.RelPath
			}
			at += f.Duration
		}
		return ""
	}
	ref := func(p string) catalog.Ref { return catalog.Ref{LibraryID: lib.ID, Path: p} }
	users := map[string]int64{}
	wantFile := map[string]string{}
	for _, disc := range []string{"CD1", "CD2"} {
		b, err := cat.GetBookByPath(ctx, lib.ID, "A/Book/"+disc)
		if err != nil {
			t.Fatal(err)
		}
		if got := fileAt(b, 2); got != "A/Book/"+disc+"/10.mp3" {
			t.Fatalf("%s at 2s plays %q, want its 10.mp3", disc, got)
		}
		users[disc] = namedUser(t, db, disc)
		wantFile[disc] = "A/Book/" + disc + "/10.mp3"
		if _, err := cat.SaveProgress(ctx, users[disc], catalog.Progress{Ref: ref(b.RelPath), Position: 2}); err != nil {
			t.Fatal(err)
		}
	}

	if err := cat.SetFolderOverride(ctx, lib.ID, "A/Book", catalog.OverrideBook); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	joined, err := cat.GetBookByPath(ctx, lib.ID, "A/Book")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"A/Book/CD1/1.mp3", "A/Book/CD1/10.mp3", "A/Book/CD1/2.mp3",
		"A/Book/CD2/1.mp3", "A/Book/CD2/10.mp3", "A/Book/CD2/2.mp3"}
	if got := filePaths(joined); !slices.Equal(got, want) {
		t.Fatalf("joined files = %q\nwant %q", got, want)
	}
	for disc, user := range users {
		p, _ := cat.GetProgress(ctx, user, ref("A/Book"))
		if p == nil {
			t.Fatalf("%s's progress didn't carry over", disc)
		}
		if got := fileAt(joined, p.Position); got != wantFile[disc] {
			t.Errorf("%s's progress at %v plays %q, want %q", disc, p.Position, got, wantFile[disc])
		}
	}
}

// silence writes secs seconds of silent MP3 at rel, tagged with any meta
// ("key=value" pairs), or skips the test without ffmpeg.
func silence(t *testing.T, root, rel string, secs float64, meta ...string) {
	t.Helper()
	if !media.HasFFmpeg("ffmpeg") {
		t.Skip("ffmpeg not available")
	}
	dst := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"-nostdin", "-loglevel", "error", "-f", "lavfi",
		"-i", "anullsrc=r=22050:cl=mono", "-t", fmt.Sprint(secs), "-c:a", "libmp3lame", "-q:a", "9"}
	for _, m := range meta {
		args = append(args, "-metadata", m)
	}
	out, err := exec.Command("ffmpeg", append(args, "-y", dst)...).CombinedOutput()
	if err != nil {
		t.Skipf("ffmpeg could not write silence: %v\n%s", err, out)
	}
}

// The scan where a join takes effect carries the disc books' listening state onto
// the joined book's timeline (offset = the earlier discs' lengths), logs each disc
// as joined, not removed, and streams/chapters point at the real files. Removing
// the override splits the discs out again and keeps the joined book's state on the
// folder's path.
func TestJoinCarriesListeningState(t *testing.T) {
	ffprobe := lookFFprobe(t)
	ctx := t.Context()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	silence(t, root, "A/Book/CD1/01.mp3", 2)
	silence(t, root, "A/Book/CD1/02.mp3", 3)
	silence(t, root, "A/Book/CD2/01.mp3", 4)
	silence(t, root, "A/Book/CD3/01.mp3", 2)
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, ffprobe, slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	discs := map[string]*catalog.Book{}
	for _, p := range []string{"A/Book/CD1", "A/Book/CD2", "A/Book/CD3"} {
		b, err := cat.GetBookByPath(ctx, lib.ID, p)
		if err != nil || b.Duration <= 0 {
			t.Fatalf("disc %s = %+v (err %v)", p, b, err)
		}
		discs[p] = b
	}
	users := map[string]int64{}
	for _, name := range []string{"ann", "bob"} {
		users[name] = namedUser(t, db, name)
	}
	ref := func(p string) catalog.Ref { return catalog.Ref{LibraryID: lib.ID, Path: p} }
	save := func(user int64, p string, pos float64, finished bool) {
		t.Helper()
		if _, err := cat.SaveProgress(ctx, user, catalog.Progress{Ref: ref(p), Position: pos, Finished: finished}); err != nil {
			t.Fatal(err)
		}
	}
	save(users["ann"], "A/Book/CD1", 4, false)
	save(users["ann"], "A/Book/CD2", 1.5, false) // further on
	save(users["bob"], "A/Book/CD3", 2, true)    // finished the last disc
	if _, err := cat.AddBookmark(ctx, users["ann"], catalog.Bookmark{Ref: ref("A/Book/CD2"), Position: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.AddNote(ctx, users["ann"], catalog.Note{Ref: ref("A/Book/CD3"), Position: 1, Body: "end"}); err != nil {
		t.Fatal(err)
	}

	if err := cat.SetFolderOverride(ctx, lib.ID, "A/Book", catalog.OverrideBook); err != nil {
		t.Fatal(err)
	}
	res, err := s.Scan(ctx, *lib)
	if err != nil {
		t.Fatal(err)
	}
	joined, err := cat.GetBookByPath(ctx, lib.ID, "A/Book")
	if err != nil {
		t.Fatal(err)
	}
	// Offsets from the joined book's own file lengths.
	var cd2At, cd3At float64
	for _, f := range joined.Files {
		switch {
		case strings.HasPrefix(f.RelPath, "A/Book/CD1/"):
			cd2At += f.Duration
			cd3At += f.Duration
		case strings.HasPrefix(f.RelPath, "A/Book/CD2/"):
			cd3At += f.Duration
		}
	}
	if math.Abs(cd2At-discs["A/Book/CD1"].Duration) > 0.1 || joined.Duration <= cd3At {
		t.Fatalf("joined timeline: CD2 at %v, CD3 at %v of %v", cd2At, cd3At, joined.Duration)
	}
	// Chapters run on one timeline through the real files, titled by disc.
	if len(joined.Chapters) != 4 || joined.Chapters[2].FilePath != "A/Book/CD2/01.mp3" ||
		math.Abs(joined.Chapters[2].BookOffset-cd2At) > 1e-6 || joined.Chapters[2].Title != "CD2 - 01" {
		t.Fatalf("chapters = %+v", joined.Chapters)
	}

	ann, _ := cat.GetProgress(ctx, users["ann"], ref("A/Book"))
	if ann == nil || math.Abs(ann.Position-(cd2At+1.5)) > 1e-6 || ann.Finished {
		t.Fatalf("ann's progress = %+v, want %v", ann, cd2At+1.5)
	}
	bob, _ := cat.GetProgress(ctx, users["bob"], ref("A/Book"))
	if bob == nil || !bob.Finished || math.Abs(bob.Position-joined.Duration) > 0.1 {
		t.Fatalf("bob's progress = %+v, want finished", bob)
	}
	if bms, _ := cat.ListBookmarks(ctx, users["ann"], ref("A/Book")); len(bms) != 1 || math.Abs(bms[0].Position-(cd2At+2)) > 1e-6 {
		t.Fatalf("bookmarks = %+v", bms)
	}
	if notes, _ := cat.ListNotes(ctx, users["ann"], ref("A/Book")); len(notes) != 1 || math.Abs(notes[0].Position-(cd3At+1)) > 1e-6 {
		t.Fatalf("notes = %+v", notes)
	}
	// The run log says what happened to each disc.
	var joinedFrom []string
	for _, e := range res.Log {
		if e.Kind == "removed" {
			t.Errorf("a joined disc is logged as removed: %+v", e)
		}
		if e.Kind == "joined" && e.To == "A/Book" {
			joinedFrom = append(joinedFrom, e.Path)
			if e.Code != "" {
				t.Errorf("a disc of known length is logged as not carried: %+v", e)
			}
		}
	}
	sort.Strings(joinedFrom)
	if !slices.Equal(joinedFrom, []string{"A/Book/CD1", "A/Book/CD2", "A/Book/CD3"}) || res.Removed != 0 || res.Added != 0 || len(res.AddedTitles) != 0 {
		t.Fatalf("log joined %q, counts %+v", joinedFrom, res.ScanCounts)
	}

	// A rescan finds nothing more to carry.
	if res, err := s.Scan(ctx, *lib); err != nil || res.Added+res.Removed+res.Moved != 0 {
		t.Fatalf("rescan = %+v (err %v)", res, err)
	}

	// Removing the override: the discs are books again, without listening state;
	// the joined book's stays on its path, for when the folder is joined again.
	if err := cat.DeleteFolderOverride(ctx, lib.ID, "A/Book"); err != nil {
		t.Fatal(err)
	}
	res, err = s.Scan(ctx, *lib)
	if err != nil {
		t.Fatal(err)
	}
	if got := bookPaths(t, cat, lib.ID); !slices.Equal(got, []string{"A/Book/CD1", "A/Book/CD2", "A/Book/CD3"}) {
		t.Fatalf("after removing the override: %q", got)
	}
	if res.Added != 0 || len(res.AddedTitles) != 0 || res.Removed != 0 {
		t.Fatalf("the split counted as added or removed: %+v %q", res.ScanCounts, res.AddedTitles)
	}
	if p, _ := cat.GetProgress(ctx, users["ann"], ref("A/Book")); p == nil || p.Position != ann.Position {
		t.Fatalf("the joined book's progress went: %+v", p)
	}
	if p, _ := cat.GetProgress(ctx, users["ann"], ref("A/Book/CD2")); p != nil {
		t.Fatalf("progress reappeared on a disc: %+v", p)
	}
}

// namedUser adds a user called name and returns its id.
func namedUser(t *testing.T, db *store.DB, name string) int64 {
	t.Helper()
	res, err := db.ExecContext(t.Context(),
		`INSERT INTO users(username, password_hash, role, created_at, updated_at) VALUES(?,'x','user','t','t')`, name)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// discSets finds the folders whose audio is only in disc folders directly in them,
// at least two, and nothing else; markSplitDiscs marks those discs.
func TestDiscSets(t *testing.T) {
	folders := []string{
		// A disc set: disc names as isDiscFolder reads them.
		"Cowell/Dragonese/CD 1", "Cowell/Dragonese/CD 2", "Cowell/Dragonese/cd-10",
		// Not: the folder holds audio itself beside its discs.
		"Mixed/Book", "Mixed/Book/Disc1", "Mixed/Book/Disc2",
		// Not: a sibling that isn't a disc folder.
		"Extras/Book/CD1", "Extras/Book/CD2", "Extras/Book/Bonus",
		// Not: audio deeper down, inside a disc.
		"Deep/Book/CD1", "Deep/Book/CD2", "Deep/Book/CD2/Bonus",
		// Discs a level down: their own folder is a set, the one above isn't.
		"Nested/Book/Part A/CD1", "Nested/Book/Part A/CD2",
		// Not: an author's books, a series' books, a single disc, discs at the root.
		"Pratchett/Mort", "Pratchett/Watch/Guards", "Pratchett/Watch/Men at Arms",
		"Single/Book/CD1",
		"CD1", "CD2", "",
	}
	sets := discSets(slices.Values(folders))
	if got := slices.Sorted(maps.Keys(sets)); !slices.Equal(got, []string{"Cowell/Dragonese", "Nested/Book/Part A"}) {
		t.Fatalf("disc sets = %q", got)
	}

	folder := func(p string) *catalog.Book { return &catalog.Book{RelPath: p, IsFolder: true} }
	books := []*catalog.Book{
		folder("Cowell/Dragonese/CD 1"), folder("Cowell/Dragonese/cd-10"),
		folder("Extras/Book/CD1"), folder("Single/Book/CD1"),
		{RelPath: "Nested/Book/Part A/CD1/01.mp3"}, // a disc read as a collection: no disc book
	}
	markSplitDiscs(books, sets)
	for _, b := range books {
		want := ""
		if strings.HasPrefix(b.RelPath, "Cowell/Dragonese/") {
			want = "Cowell/Dragonese"
		}
		if b.SplitParent != want {
			t.Errorf("%s: split parent %q, want %q", b.RelPath, b.SplitParent, want)
		}
	}
}

// splitParents reads books.split_parent as stored, by path.
func splitParents(t *testing.T, cat *catalog.Catalog, libID int64) map[string]string {
	t.Helper()
	sigs, err := cat.Signatures(t.Context(), libID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for p, sig := range sigs {
		if sig.SplitParent != "" {
			out[p] = sig.SplitParent
		}
	}
	return out
}

// The scan records the discs of a split book (books.split_parent), and keeps the
// record true as their siblings come and go even while the discs themselves are
// unchanged and not re-indexed; a row from before the column (or a rebuilt index)
// gets it from the next scan. IndexPath agrees with the scan.
func TestSplitDiscsScanned(t *testing.T) {
	cat, ctx := newHealthCatalog(t)
	root := t.TempDir()
	audio := testAudio(t)
	writeFile(t, root, "A/Book/CD1/01.m4b", audio)
	writeFile(t, root, "A/Book/CD2/01.m4b", audio)
	writeFile(t, root, "A/Other/01.m4b", audio)
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	scan := func() {
		t.Helper()
		if _, err := s.Scan(ctx, *lib); err != nil {
			t.Fatal(err)
		}
	}
	split := map[string]string{"A/Book/CD1": "A/Book", "A/Book/CD2": "A/Book"}

	scan()
	if got := splitParents(t, cat, lib.ID); !maps.Equal(got, split) {
		t.Fatalf("split discs = %v, want %v", got, split)
	}
	// On demand a disc keeps it; a book beside them is none.
	for p, want := range map[string]string{"A/Book/CD2": "A/Book", "A/Other": ""} {
		if _, err := s.IndexPath(ctx, *lib, p); err != nil {
			t.Fatal(err)
		}
		if got := splitParents(t, cat, lib.ID)[p]; got != want {
			t.Fatalf("IndexPath(%q): split parent %q, want %q", p, got, want)
		}
	}

	// A sibling that isn't a disc: no longer split, though the discs didn't change.
	writeFile(t, root, "A/Book/Bonus/01.m4b", audio)
	scan()
	if got := splitParents(t, cat, lib.ID); len(got) != 0 {
		t.Fatalf("with a bonus folder: split discs = %v, want none", got)
	}
	if _, err := s.IndexPath(ctx, *lib, "A/Book/CD1"); err != nil {
		t.Fatal(err)
	}
	if got := splitParents(t, cat, lib.ID); len(got) != 0 {
		t.Fatalf("IndexPath with a bonus folder: split discs = %v, want none", got)
	}
	if err := os.RemoveAll(filepath.Join(root, "A", "Book", "Bonus")); err != nil {
		t.Fatal(err)
	}
	scan()
	if got := splitParents(t, cat, lib.ID); !maps.Equal(got, split) {
		t.Fatalf("bonus gone: split discs = %v, want %v", got, split)
	}

	// Rows indexed before migration 0022 start blank: the next scan fills them in.
	if err := cat.SetSplitParent(ctx, lib.ID, map[string]string{"A/Book/CD1": "", "A/Book/CD2": ""}); err != nil {
		t.Fatal(err)
	}
	scan()
	if got := splitParents(t, cat, lib.ID); !maps.Equal(got, split) {
		t.Fatalf("after an upgrade: split discs = %v, want %v", got, split)
	}
}

// The discovery walk reads each folder's audio files as audioEntries does: the same
// files, in the same order, with hidden, non-audio and ignored files and hidden or
// ignored folders left out, so reading them in the walk changes nothing.
func TestWalkReadsWhatAudioEntriesReads(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{
		"A/Book/01.mp3",
		"A/Book/02.m4b",
		"A/Book/10.mp3",
		"A/Book/.01.mp3",          // hidden file
		"A/Book/notes.txt",        // not audio
		"A/Book/promo.sample.mp3", // ignored file
		"A/Book/CD1/01.mp3",
		"A/.trash/old.mp3", // hidden folder
		"A/Extras/01.mp3",  // ignored folder
		"loose.mp3",
	} {
		writeFile(t, root, p, []byte("x"))
	}
	lib := catalog.Library{Name: "L", Root: root}
	ignore := ParseIgnore([]string{"*.sample.mp3", "Extras/"})
	dirs, partial, err := audioDirs(t.Context(), lib, root, "", ignore, slog.Default(), nil)
	if err != nil || partial {
		t.Fatalf("walk: partial %v, err %v", partial, err)
	}
	names := func(files []audioFile) []string {
		var out []string
		for _, f := range files {
			out = append(out, relPathOf(root, f.abs))
		}
		return out
	}
	want := map[string][]string{
		"":           {"loose.mp3"},
		"A/Book":     {"A/Book/01.mp3", "A/Book/02.m4b", "A/Book/10.mp3"},
		"A/Book/CD1": {"A/Book/CD1/01.mp3"},
	}
	if len(dirs) != len(want) {
		t.Fatalf("walk found folders %v, want %v", slices.Collect(maps.Keys(dirs)), slices.Collect(maps.Keys(want)))
	}
	for dir, files := range want {
		if got := names(dirs[dir]); !slices.Equal(got, files) {
			t.Errorf("walk %q = %q, want %q", dir, got, files)
		}
		if got := names(audioEntries(root, absOf(lib, dir), ignore)); !slices.Equal(got, files) {
			t.Errorf("audioEntries %q = %q, want %q", dir, got, files)
		}
	}
}
