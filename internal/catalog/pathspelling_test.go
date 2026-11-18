package catalog

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

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
		got, err := c.Spelling(ctx, lib, tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: Spelling(%+q) = %+q, want %+q", tc.name, tc.in, got, tc.want)
		}
	}
	// Another library holds none of them.
	other, _ := c.CreateLibrary(ctx, Library{Name: "M", Root: "/tmp/m"})
	if got, err := c.Spelling(ctx, other.ID, nfdEmile); err != nil || got != nfdEmile {
		t.Errorf("another library: %+q %v, want the path as given", got, err)
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
	if got, err := c.Spelling(ctx, 1, "Author/Book/01.mp3"); err != nil || got != "Author/Book/01.mp3" {
		t.Fatalf("ASCII path: %q %v, want it without a lookup", got, err)
	}
	if _, err := c.Spelling(ctx, 1, nfdEmile); err == nil {
		t.Fatal("an accented path answered without its lookup")
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
	if n, err := c.KeyPaths(ctx); err != nil || n == 0 {
		t.Fatalf("first KeyPaths: %d %v, want every book", n, err)
	}
	if n, err := c.KeyPaths(ctx); err != nil || n != 0 {
		t.Fatalf("second KeyPaths: %d %v, want 0", n, err)
	}
	// A row an older server inserted (NULL) is keyed at the next start.
	if _, err := c.db.ExecContext(ctx, `UPDATE books SET path_key = NULL WHERE library_id = ? AND rel_path = ?`, lib, nfdAmelie); err != nil {
		t.Fatal(err)
	}
	if n, err := c.KeyPaths(ctx); err != nil || n != 1 {
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
	if n, err := c.KeyPaths(ctx); err != nil || n != total {
		t.Fatalf("KeyPaths after a version change: %d %v, want %d", n, err, total)
	}
	if got, ok := key(nfcEmile); !ok || got != "" {
		t.Errorf("stale key after the version change = %+q", got)
	}
}

// TestSpellingPlans: a lookup by key reads the small partial index, and the
// start's "anything unkeyed?" check its own, never the whole table.
func TestSpellingPlans(t *testing.T) {
	t.Parallel()
	c, _ := newTestCatalog(t)
	for q, index := range map[string]string{
		`SELECT id FROM books WHERE library_id = 1 AND path_key IN ('x') AND path_key != ''`: "idx_books_path_key",
		`SELECT EXISTS(SELECT 1 FROM books WHERE path_key IS NULL)`:                          "idx_books_path_key_unset",
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
