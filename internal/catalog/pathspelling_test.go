package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	unorm "golang.org/x/text/unicode/norm"

	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// Path spellings built from explicit code points (never pasted): NFC is the
// precomposed letter, NFD the letter then the combining mark.
const (
	nfcEmile  = "Émile Zola/Germinal"  // NFC on disk
	nfdEmile  = "Émile Zola/Germinal" // the same, decomposed
	nfdAmelie = "Amélie/Le Fabuleux"  // NFD on disk
	nfcAmelie = "Amélie/Le Fabuleux"   // the same, precomposed
	mixedBook = "Château"              // an NFC folder ...
	mixedFile = "Château/Préface.mp3" // ... with an NFD file name
	joinedEte = "Été"                  // a book joined from its disc folders
	navBook   = "Auteurs/François/Livre"
	nestedOut = "Série" // a folder book holding a book
	nestedIn  = "Série/Livre 2"
)

// seedSpellings indexes the spelling fixtures in a new library and returns its id.
func seedSpellings(t *testing.T, c *Catalog) int64 {
	t.Helper()
	ctx := t.Context()
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if err != nil {
		t.Fatal(err)
	}
	folder := func(rel string, files ...string) {
		t.Helper()
		b := &Book{LibraryID: lib.ID, RelPath: rel, IsFolder: true, Title: rel, AddedAt: "2020-01-01"}
		for i, f := range files {
			b.Files = append(b.Files, BookFile{RelPath: f, Seq: i, Format: "mp3"})
		}
		upsert(t, c, b)
	}
	folder(nfcEmile, nfcEmile+"/01.mp3", nfcEmile+"/02.mp3")
	folder(nfdAmelie, nfdAmelie+"/01.mp3")
	folder(mixedBook, mixedFile, mixedBook+"/Fin.mp3")
	folder(joinedEte, joinedEte+"/CD 1/01.mp3", joinedEte+"/CD 2/01.mp3")
	folder(navBook, navBook+"/01.mp3")
	folder(nestedOut, nestedOut+"/01.mp3")
	folder(nestedIn, nestedIn+"/01.m4b")
	// Two books whose names differ only by normalisation (a disk that tells
	// them apart), and two navigation folders the same way.
	folder("Éléna", "Éléna/01.mp3")
	folder("Éléna", "Éléna/01.mp3")
	folder("Coll/Éléna/A", "Coll/Éléna/A/01.mp3")
	folder("Coll/Éléna/B", "Coll/Éléna/B/01.mp3")
	return lib.ID
}

// TestSpelling (server#210): a path in another Unicode spelling reads as the
// index's; a path the index has as given, one it has nothing for, and one two
// indexed paths both read as stay as given.
func TestSpelling(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib := seedSpellings(t, c)
	for _, tc := range []struct{ name, in, want string }{
		{"ascii", "Some/Plain Path", "Some/Plain Path"},
		{"exact NFC book", nfcEmile, nfcEmile},
		{"exact NFD book", nfdAmelie, nfdAmelie},
		{"NFD request, NFC book", nfdEmile, nfcEmile},
		{"NFC request, NFD book", nfcAmelie, nfdAmelie},
		{"NFD request, NFC folder of an NFD-named book", "Château", mixedBook},
		{"track, other spelling", nfdEmile + "/02.mp3", nfcEmile + "/02.mp3"},
		{"track of an NFD book, NFC request", nfcAmelie + "/01.mp3", nfdAmelie + "/01.mp3"},
		{"mixed track, all NFC request", "Château/Préface.mp3", mixedFile},
		{"mixed track, all NFD request", "Château/Préface.mp3", mixedFile},
		{"disc folder, other spelling", "Été/CD 2", joinedEte + "/CD 2"},
		{"nested book's file", "Série/Livre 2/01.m4b", nestedIn + "/01.m4b"},
		{"nested book", "Série/Livre 2", nestedIn},
		{"navigation folder", "Auteurs/François", "Auteurs/François"},
		{"track not in the book", nfdEmile + "/03.mp3", nfdEmile + "/03.mp3"},
		{"nothing indexed", "Rien/Icí", "Rien/Icí"},
		{"colliding books", "Éléna", "Éléna"},
		{"colliding navigation folders", "Coll/Éléna", "Coll/Éléna"},
	} {
		got, _, err := c.Spelling(ctx, lib, tc.in, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: Spelling(%+q) = %+q, want %+q", tc.name, tc.in, got, tc.want)
		}
	}
	// Another library holds none of them.
	other, _ := c.CreateLibrary(ctx, Library{Name: "M", Root: "/tmp/m"})
	if got, _, err := c.Spelling(ctx, other.ID, nfdEmile, nil); err != nil || got != nfdEmile {
		t.Errorf("another library: %+q %v, want the path as given", got, err)
	}
}

// TestSpellingScope (server#210, security): a spelling the caller's scope
// refuses counts as none. With an NFC and an NFD folder side by side and a
// grant on the NFC one only, a third spelling reads as the NFC one, as a book,
// a track or a navigation folder, and as a list ref, exactly as it does when
// the NFD one is not there: nothing outside the grant changes the answer. A
// caller granted neither gets the path as given.
func TestSpellingScope(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib := seedSpellings(t, c)
	const (
		nfc = "\u00c9l\u00e9na"
		nfd = "E\u0301le\u0301na"
		mix = "\u00c9le\u0301na"
	)
	within := func(rule string) func(string) bool {
		return func(p string) bool { return p == rule || strings.HasPrefix(p, rule+"/") }
	}
	onlyNFC := func(p string) bool { return within(nfc)(p) || within("Coll/"+nfc)(p) }
	none := func(string) bool { return false }
	cases := []struct{ in, want string }{
		{mix, nfc},
		{mix + "/01.mp3", nfc + "/01.mp3"},
		{"Coll/" + mix, "Coll/" + nfc},
	}
	check := func(when string) {
		t.Helper()
		for _, tc := range cases {
			if got, _, err := c.Spelling(ctx, lib, tc.in, onlyNFC); err != nil || got != tc.want {
				t.Errorf("%s: Spelling(%+q) in the NFC grant = %+q %v, want %+q", when, tc.in, got, err, tc.want)
			}
			if got, _, err := c.Spelling(ctx, lib, tc.in, none); err != nil || got != tc.in {
				t.Errorf("%s: Spelling(%+q) with no grant = %+q %v, want it as given", when, tc.in, got, err)
			}
		}
		scopes := []Scope{{LibraryID: lib, Paths: []string{nfc}}}
		got, err := c.listableRefs(ctx, []Ref{{LibraryID: lib, Path: mix}}, scopes)
		if want := []Ref{{LibraryID: lib, Path: nfc}}; err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: listable %+v %v, want %+v", when, got, err, want)
		}
	}
	check("both there")
	// Without a scope, the two side by side are a collision: as given.
	if got, _, err := c.Spelling(ctx, lib, mix, nil); err != nil || got != mix {
		t.Errorf("Spelling(%+q) of every spelling = %+q %v, want it as given", mix, got, err)
	}
	if _, err := c.db.ExecContext(ctx, `DELETE FROM books WHERE library_id = ? AND rel_path IN (?, ?)`,
		lib, nfd, "Coll/"+nfd+"/B"); err != nil {
		t.Fatal(err)
	}
	check("the NFD one gone")

	// outside: a rule written in NFD admits the NFD spelling of the NFC book
	// as given, and nothing it could mean; the caller answers it as missing.
	nfdRule := within(nfdEmile)
	for _, p := range []string{nfdEmile, nfdEmile + "/02.mp3"} {
		if got, outside, err := c.Spelling(ctx, lib, p, nfdRule); err != nil || got != p || !outside {
			t.Errorf("Spelling(%+q) under an NFD rule = %+q outside=%v %v, want it as given, outside", p, got, outside, err)
		}
		if got, outside, err := c.Spelling(ctx, lib, p, nil); err != nil || got == p || outside {
			t.Errorf("Spelling(%+q) of every spelling = %+q outside=%v %v, want the index's", p, got, outside, err)
		}
	}
	if _, outside, err := c.Spelling(ctx, lib, "Rien/Ic\u00ed", nfdRule); err != nil || outside {
		t.Errorf("Spelling of a path nothing reads as = outside %v %v", outside, err)
	}
}

// TestOwnEntry: p is an entry of its own when it is on the disk and none of
// the others is the same file; another spelling of an indexed name is not,
// whether the disk ignores the spelling (it finds the same folder) or keeps
// them apart (it finds nothing by p).
func TestOwnEntry(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	root := t.TempDir()
	for _, d := range []string{"a", "b", "E\u0301mile"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		p      string
		others []string
		want   bool
	}{
		{"a", []string{"b"}, true},
		{"a", []string{"missing"}, true},
		{"a", []string{"a"}, false},
		{"missing", []string{"a"}, false},
		{"\u00c9mile", []string{"E\u0301mile"}, false},
	} {
		if got := c.ownEntry(ctx, lib.ID, tc.p, tc.others); got != tc.want {
			t.Errorf("ownEntry(%+q, %+q) = %v, want %v", tc.p, tc.others, got, tc.want)
		}
	}
	if c.ownEntry(ctx, lib.ID+99, "a", nil) {
		t.Error("ownEntry in no library = true")
	}
	// Whatever the disk's file ids say, a name is its own only as its folder
	// lists it: the decomposed folder is listed, its precomposed spelling never
	// (on a disk that finds it by that spelling too, or one that has no such
	// entry), and a name with no other spelling is not looked up.
	if err := os.WriteFile(filepath.Join(root, "E\u0301mile", "01.mp3"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{
		"E\u0301mile/01.mp3": true, "\u00c9mile/01.mp3": false, "E\u0301mile": true, "\u00c9mile": false, "a": true,
	} {
		if got := spelledOnDisk(ctx, root, p); got != want {
			t.Errorf("spelledOnDisk(%+q) = %v, want %v", p, got, want)
		}
	}
}

// TestSpellingOwnEntry (server#210): on a disk that keeps an NFC and an NFD
// name apart, a spelling that is a folder of its own there (not indexed yet)
// is never read as the indexed one beside it, nor reported outside a scope;
// once it is gone, it is again. On a disk that ignores the spelling (macOS)
// the two are one folder, read as the index's.
func TestSpellingOwnEntry(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	root := t.TempDir()
	const (
		nfc = "\u00c9mile/Germinal"
		nfd = "E\u0301mile/Germinal"
	)
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(nfc)), 0o755); err != nil {
		t.Fatal(err)
	}
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	upsert(t, c, &Book{LibraryID: lib.ID, RelPath: nfc, IsFolder: true, Title: "G", AddedAt: "2020-01-01",
		Files: []BookFile{{RelPath: nfc + "/01.mp3", Format: "mp3"}}})
	nfdRule := func(p string) bool { return p == nfd || strings.HasPrefix(p, nfd+"/") }
	spell := func(allow func(string) bool) (string, bool) {
		t.Helper()
		got, outside, err := c.Spelling(ctx, lib.ID, nfd, allow)
		if err != nil {
			t.Fatal(err)
		}
		return got, outside
	}
	if got, _ := spell(nil); got != nfc {
		t.Fatalf("NFD with only the NFC folder on disk = %+q, want %+q", got, nfc)
	}
	if _, outside := spell(nfdRule); !outside {
		t.Fatal("NFD under an NFD rule, only the NFC folder on disk: not outside")
	}
	err = os.MkdirAll(filepath.Join(root, filepath.FromSlash(nfd)), 0o755)
	if fi1, _ := os.Stat(filepath.Join(root, filepath.FromSlash(nfd))); err != nil || fi1 == nil {
		t.Fatalf("mkdir NFD: %v", err)
	}
	fiC, _ := os.Stat(filepath.Join(root, filepath.FromSlash(nfc)))
	fiD, _ := os.Stat(filepath.Join(root, filepath.FromSlash(nfd)))
	if os.SameFile(fiC, fiD) {
		t.Log("this disk ignores the Unicode spelling: one folder")
		if got, _ := spell(nil); got != nfc {
			t.Errorf("NFD on a disk ignoring the spelling = %+q, want %+q", got, nfc)
		}
		return
	}
	if got, outside := spell(nil); got != nfd || outside {
		t.Errorf("NFD, a folder of its own beside the NFC one = %+q outside=%v, want it as given", got, outside)
	}
	if got, outside := spell(nfdRule); got != nfd || outside {
		t.Errorf("NFD under an NFD rule, a folder of its own = %+q outside=%v, want it as given, not outside", got, outside)
	}
	if err := os.RemoveAll(filepath.Join(root, "E\u0301mile")); err != nil {
		t.Fatal(err)
	}
	if got, _ := spell(nil); got != nfc {
		t.Errorf("NFD once its folder is gone = %+q, want %+q", got, nfc)
	}
}

// TestSpellingASCIINoLookup: a path with one Unicode spelling (every ASCII
// path) is answered without a lookup, so most libraries pay nothing: on a
// closed database it still answers, while another spelling's lookup fails.
func TestSpellingASCIINoLookup(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	c, ctx := New(db, time.Now), t.Context()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if got, _, err := c.Spelling(ctx, 1, "Author/Book/01.mp3", nil); err != nil || got != "Author/Book/01.mp3" {
		t.Fatalf("ASCII path: %q %v, want it without a lookup", got, err)
	}
	if _, _, err := c.Spelling(ctx, 1, nfdEmile, nil); err == nil {
		t.Fatal("an accented path answered without its lookup")
	}
}

// TestHasOtherSpelling: true only for a name with another NFC or NFD spelling
// (an accented letter in either form), never for ASCII, a curly apostrophe or
// a Han name.
func TestHasOtherSpelling(t *testing.T) {
	t.Parallel()
	nfc := "Soir\u00e9e.epub"
	for name, want := range map[string]bool{
		"Author/Book/01.mp3": false, "Don\u2019t Panic.epub": false, "\u4e09\u4f53.epub": false,
		nfc: true, unorm.NFD.String(nfc): true,
	} {
		if got := HasOtherSpelling(name); got != want {
			t.Errorf("HasOtherSpelling(%+q) = %v, want %v", name, got, want)
		}
	}
}

// TestKeyPaths: both book inserts write the key (” for an NFC path, the NFC
// form for any other); KeyPaths keys a row left NULL (a book an older server
// inserted after a rollback) and, after a change of Unicode tables, every row.
func TestKeyPaths(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib := seedSpellings(t, c)
	key := func(rel string) (string, bool) {
		t.Helper()
		var k *string
		if err := c.db.QueryRowContext(ctx, `SELECT path_key FROM books WHERE library_id = ? AND rel_path = ?`, lib, rel).Scan(&k); err != nil {
			t.Fatal(err)
		}
		if k == nil {
			return "", false
		}
		return *k, true
	}
	for rel, want := range map[string]string{nfcEmile: "", nfdAmelie: nfcAmelie, mixedBook: "", nestedIn: ""} {
		if got, ok := key(rel); !ok || got != want {
			t.Errorf("upsert keyed %+q as %+q (set %v), want %+q", rel, got, ok, want)
		}
	}
	// The podcast episode insert keys its rows too.
	pods, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	f, err := feed.ReadFile("../podcast/testdata/feed-v1.json", "Example Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.IndexPodcastShow(ctx, pods.ID, "Example Show", "s", "", "", f, nil, false); err != nil {
		t.Fatal(err)
	}
	var unkeyed int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM books WHERE library_id = ? AND path_key IS NULL`, pods.ID).Scan(&unkeyed); err != nil {
		t.Fatal(err)
	}
	if unkeyed != 0 {
		t.Errorf("%d podcast episodes inserted with no key", unkeyed)
	}

	// The first start keys every book (no version stored yet), then nothing.
	if n, err := c.KeyPaths(ctx, nil); err != nil || n == 0 {
		t.Fatalf("first KeyPaths: %d %v, want every book", n, err)
	}
	if n, err := c.KeyPaths(ctx, nil); err != nil || n != 0 {
		t.Fatalf("second KeyPaths: %d %v, want 0", n, err)
	}
	// A row an older server inserted (NULL) is keyed at the next start.
	if _, err := c.db.ExecContext(ctx, `UPDATE books SET path_key = NULL WHERE library_id = ? AND rel_path = ?`, lib, nfdAmelie); err != nil {
		t.Fatal(err)
	}
	if n, err := c.KeyPaths(ctx, nil); err != nil || n != 1 {
		t.Fatalf("KeyPaths over a NULL row: %d %v, want 1", n, err)
	}
	if got, _ := key(nfdAmelie); got != nfcAmelie {
		t.Errorf("re-keyed NULL row = %+q, want %+q", got, nfcAmelie)
	}
	// Other Unicode tables: every row again, a wrong key put right.
	if _, err := c.db.ExecContext(ctx, `UPDATE books SET path_key = 'stale' WHERE library_id = ? AND rel_path = ?`, lib, nfcEmile); err != nil {
		t.Fatal(err)
	}
	if err := putServerState(ctx, c.db, pathKeysKey, "0/older", c.stamp()); err != nil {
		t.Fatal(err)
	}
	var total int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM books`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if n, err := c.KeyPaths(ctx, nil); err != nil || n != total {
		t.Fatalf("KeyPaths after a version change: %d %v, want %d", n, err, total)
	}
	if got, ok := key(nfcEmile); !ok || got != "" {
		t.Errorf("stale key after the version change = %+q", got)
	}
}

// TestKeyPathsResumes (server#210): KeyPaths keys in transactions of a few
// books, each committed before the next, so a start stopped part way keeps
// what it keyed and the next one does only the rest: the NULL rows, or, for a
// re-key of every book (a version change), the books past the last id done.
// The version is stored only once every book is keyed.
func TestKeyPathsResumes(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib := seedSpellings(t, c)
	c.chunkKeyPaths = 3
	var total int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM books`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total <= 2*c.chunkKeyPaths {
		t.Fatalf("%d books: too few for three transactions", total)
	}
	count := func(where string) int {
		t.Helper()
		var n int
		if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM books WHERE `+where).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	version := func() string {
		t.Helper()
		v, err := getServerState[string](ctx, c.db, pathKeysKey)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	// stopAfterOne is KeyPaths stopped once its first transaction committed.
	stopAfterOne := func() {
		t.Helper()
		stop, cancel := context.WithCancel(ctx)
		defer cancel()
		var told [][2]int
		_, err := c.KeyPaths(stop, func(done, of int) {
			told = append(told, [2]int{done, of})
			cancel()
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stopped KeyPaths = %v, want context.Canceled", err)
		}
		if len(told) != 1 || told[0][0] != c.chunkKeyPaths {
			t.Fatalf("progress = %v, want one transaction of %d", told, c.chunkKeyPaths)
		}
	}

	// Books an older server inserted (NULL): the stopped start keyed one
	// transaction's worth; the next keys the rest.
	if err := putServerState(ctx, c.db, pathKeysKey, pathKeysVersion, c.stamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE books SET path_key = NULL`); err != nil {
		t.Fatal(err)
	}
	stopAfterOne()
	if n := count(`path_key IS NULL`); n != total-c.chunkKeyPaths {
		t.Fatalf("after the stopped start %d books unkeyed, want %d", n, total-c.chunkKeyPaths)
	}
	if n, err := c.KeyPaths(ctx, nil); err != nil || n != total-c.chunkKeyPaths {
		t.Fatalf("next start keyed %d %v, want the %d left", n, err, total-c.chunkKeyPaths)
	}
	if n := count(`path_key IS NULL`); n != 0 {
		t.Fatalf("%d books unkeyed after the next start", n)
	}

	// A version change keys every book again: stopped part way, the version
	// stays the old one and the next start goes on past the last id done.
	if err := putServerState(ctx, c.db, pathKeysKey, "0/older", c.stamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE books SET path_key = 'stale'`); err != nil {
		t.Fatal(err)
	}
	stopAfterOne()
	if v := version(); v != "0/older" {
		t.Fatalf("version after a stopped re-key = %q, want the old one", v)
	}
	if n := count(`path_key = 'stale'`); n != total-c.chunkKeyPaths {
		t.Fatalf("after the stopped re-key %d books stale, want %d", n, total-c.chunkKeyPaths)
	}
	if n, err := c.KeyPaths(ctx, nil); err != nil || n != total-c.chunkKeyPaths {
		t.Fatalf("next start re-keyed %d %v, want the %d left", n, err, total-c.chunkKeyPaths)
	}
	if n := count(`path_key = 'stale' OR path_key IS NULL`); n != 0 {
		t.Fatalf("%d books not re-keyed", n)
	}
	if v := version(); v != pathKeysVersion {
		t.Fatalf("version = %q, want %q", v, pathKeysVersion)
	}
	if r, err := getServerState[pathKeysResume](ctx, c.db, pathKeysResumeKey); err != nil || r != (pathKeysResume{}) {
		t.Fatalf("resume row left behind: %+v %v", r, err)
	}
	var k string
	if err := c.db.QueryRowContext(ctx, `SELECT path_key FROM books WHERE library_id = ? AND rel_path = ?`, lib, nfdAmelie).Scan(&k); err != nil || k != nfcAmelie {
		t.Fatalf("key of %+q = %+q %v, want %+q", nfdAmelie, k, err, nfcAmelie)
	}
}

// TestSpellingPlans: a lookup by key reads the small partial index, and the
// start's count of the unkeyed books and its reads of them their own, never
// the whole table.
func TestSpellingPlans(t *testing.T) {
	t.Parallel()
	c, _ := newTestCatalog(t)
	for q, index := range map[string]string{
		`SELECT id FROM books WHERE library_id = 1 AND path_key IN ('x') AND path_key != ''`:                "idx_books_path_key",
		`SELECT COUNT(*) FROM books WHERE path_key IS NULL AND id > 0`:                                      "idx_books_path_key_unset",
		`SELECT id, rel_path, path_key FROM books WHERE path_key IS NULL AND id > 0 ORDER BY id LIMIT 2000`: "idx_books_path_key_unset",
	} {
		if plan := strings.Join(queryPlan(t, c, q, nil), "\n"); !strings.Contains(plan, index) {
			t.Errorf("%s does not use %s:\n%s", q, index, plan)
		}
	}
}

// TestListableRefsMapToBooks (server#210): a whole-list replace maps a track,
// a disc folder and another spelling to the book they name, collapses refs
// that name one book after mapping (the first place wins), skips a book outside
// scope once mapped and a path that names none; a remove by a track or another
// spelling takes the book's entry off, and the literal one too.
func TestListableRefsMapToBooks(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib := seedSpellings(t, c)
	user := seedUser(t, c, ctx)
	ref := func(p string) Ref { return Ref{LibraryID: lib, Path: p} }
	all := []Scope{{LibraryID: lib, AllowAll: true}}
	got, err := c.listableRefs(ctx, []Ref{
		ref(nfdEmile + "/02.mp3"), ref(nfcEmile), ref(nfcAmelie), ref("Été/CD 2"),
		ref("Nowhere/01.mp3"), ref("Auteurs"),
	}, all)
	if err != nil {
		t.Fatal(err)
	}
	want := []Ref{ref(nfcEmile), ref(nfdAmelie), ref(joinedEte)}
	if !slices.Equal(got, want) {
		t.Fatalf("listable = %+v, want %+v", got, want)
	}
	// Scope is the mapped book's: a grant reaching only one track keeps nothing.
	track := []Scope{{LibraryID: lib, Paths: []string{nfcEmile + "/02.mp3"}}}
	if got, err := c.listableRefs(ctx, []Ref{ref(nfcEmile + "/02.mp3")}, track); err != nil || len(got) != 0 {
		t.Fatalf("listable under a track grant = %+v %v, want none", got, err)
	}

	if err := c.SetQueue(ctx, user, []Ref{ref(nfcEmile), ref(nfdAmelie)}, all, nil); err != nil {
		t.Fatal(err)
	}
	// A stray row at a track (an old server's), cleaned up by its literal path.
	if _, err := c.db.ExecContext(ctx, `INSERT INTO up_next(user_id, library_id, rel_path, position, added_at) VALUES(?, ?, ?, 9, 't')`,
		user, lib, nfdEmile+"/01.mp3"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{nfdEmile + "/01.mp3", nfcAmelie} {
		if err := c.RemoveFromQueue(ctx, user, ref(p)); err != nil {
			t.Fatal(err)
		}
	}
	var left int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM up_next WHERE user_id = ?`, user).Scan(&left); err != nil || left != 0 {
		t.Fatalf("queue rows after the removes = %d %v, want none", left, err)
	}
}

// TestListableRefsBounded: a replace's refs are looked up as deep as the index
// has books, never a path's every folder: a ref over MaxPathBytes is skipped,
// and a thousand deep refs below a folder holding books answer at once. A track
// below a nested book still maps to it.
func TestListableRefsBounded(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib := seedSpellings(t, c)
	upsert(t, c, &Book{LibraryID: lib, RelPath: "Plain/Series", IsFolder: true, Title: "S", AddedAt: "2020-01-01",
		Files: []BookFile{{RelPath: "Plain/Series/01.mp3", Format: "mp3"}}})
	upsert(t, c, &Book{LibraryID: lib, RelPath: "Plain/Series/Book 2", IsFolder: true, Title: "B", AddedAt: "2020-01-01",
		Files: []BookFile{{RelPath: "Plain/Series/Book 2/CD 1/01.mp3", Format: "mp3"}}})
	all := []Scope{{LibraryID: lib, AllowAll: true}}
	ref := func(p string) Ref { return Ref{LibraryID: lib, Path: p} }
	refs := []Ref{ref(strings.Repeat("a/", MaxPathBytes) + "x"), ref("Plain/Series/Book 2/CD 1/01.mp3"), ref("Plain/Series/01.mp3")}
	for i := range 1000 {
		refs = append(refs, ref(fmt.Sprintf("Plain/%04d/", i)+strings.Repeat("a/", 2000)+"x"))
	}
	start := time.Now()
	got, err := c.listableRefs(ctx, refs, all)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Ref{ref("Plain/Series/Book 2"), ref("Plain/Series")}; !slices.Equal(got, want) {
		t.Fatalf("listable = %+v, want %+v", got, want)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("a thousand deep refs took %v", took)
	}
}

// countingDB counts the bytes of the text a catalog's reads bind.
type countingDB struct {
	catalogDB
	bound atomic.Int64
}

func (d *countingDB) count(args []any) {
	for _, a := range args {
		if s, ok := a.(string); ok {
			d.bound.Add(int64(len(s)))
		}
	}
}

func (d *countingDB) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	d.count(args)
	return d.catalogDB.QueryContext(ctx, q, args...)
}

func (d *countingDB) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	d.count(args)
	return d.catalogDB.QueryRowContext(ctx, q, args...)
}

// TestBookHoldingBounded (server#210): the folders of a path far deeper than
// the index are looked up only as deep as books lie, never every one of them
// (which bound the square of the path's length: a thousand deep refs below a
// book took 45 s to replace a list with), in the spelling given (bookHolding)
// or another (Spelling's step 4). A book's own deep file and folder, past one
// read's folders, are still held by it, in either spelling.
func TestBookHoldingBounded(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib := seedSpellings(t, c)
	upsert(t, c, &Book{LibraryID: lib, RelPath: "Plain/Series", IsFolder: true, Title: "S", AddedAt: "2020-01-01",
		Files: []BookFile{{RelPath: "Plain/Series/01.mp3", Format: "mp3"}}})
	deep := "Plain/D\u00e9ep/" + strings.Repeat("d/", 2*holdingFolders)
	upsert(t, c, &Book{LibraryID: lib, RelPath: "Plain/D\u00e9ep", IsFolder: true, Title: "D", AddedAt: "2020-01-01",
		Files: []BookFile{{RelPath: deep + "01.mp3", Format: "mp3"}}})
	ref := func(p string) Ref { return Ref{LibraryID: lib, Path: p} }
	var refs []Ref
	for i := range 1000 {
		refs = append(refs, ref(fmt.Sprintf("Plain/Series/%04d/", i)+strings.Repeat("a/", 2000)+"x"))
		if i%10 == 0 {
			refs = append(refs, ref(fmt.Sprintf("Plain/Series/\u00c9%04d/", i)+strings.Repeat("a/", 2000)+"x"))
		}
	}
	db := &countingDB{catalogDB: c.db}
	c.db = db
	got, err := c.listableRefs(ctx, refs, []Scope{{LibraryID: lib, AllowAll: true}})
	if err != nil || len(got) != 0 {
		t.Fatalf("listable = %+v %v, want none (no ref names a file)", got, err)
	}
	// Every folder bound would be about 4 MB a ref; the index's depth is a
	// few of them, plus the path itself.
	if per := db.bound.Load() / int64(len(refs)); per > 64<<10 {
		t.Fatalf("a deep ref bound %d bytes of folders", per)
	}

	folder := strings.TrimSuffix(deep, "/")
	for _, p := range []string{deep + "01.mp3", folder} {
		if book, err := c.BookPathAt(ctx, lib, p); err != nil || book != "Plain/D\u00e9ep" {
			t.Errorf("deep %+q held by %+q %v", p, book, err)
		}
	}
	if _, err := c.BookPathOfFile(ctx, lib, folder); !errors.Is(err, ErrNotFound) {
		t.Errorf("deep folder inside a book as a file: %v, want ErrNotFound", err)
	}
	for _, p := range []string{deep + "01.mp3", folder} {
		if spelled, _, err := c.Spelling(ctx, lib, unorm.NFD.String(p), nil); err != nil || spelled != p {
			t.Errorf("deep NFD %+q spelled %+q %v, want the index's", p, spelled, err)
		}
	}
}
