package library

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

const (
	testCharacters = `{"work":"w","characters":[{"id":"hero","name":"Hero","reveal":{"chapter":1}}]}`
	testRecaps     = `{"work":"w","recaps":[{"through":{"chapter":2},"text":"Through two."}],"in_short":"Short."}`
	testBridge     = `{"generator":"audiosilo-bridge","version":"0.2.0","kind":"sidecars.generate","job_id":7,"generated_at":"2026-10-15T10:00:00Z","source":"audio"}`
)

// writeAt writes body at the library-relative rel under root, with mtime at.
func writeAt(t *testing.T, root, rel, body string, at time.Time) {
	t.Helper()
	writeFile(t, root, rel, []byte(body))
	if err := os.Chtimes(filepath.Join(root, filepath.FromSlash(rel)), at, at); err != nil {
		t.Fatal(err)
	}
}

// attachmentsAt is the stored attachments of the book at rel (nil for none).
func attachmentsAt(t *testing.T, cat *catalog.Catalog, libID int64, rel string) *metadata.AttachmentFiles {
	t.Helper()
	b, err := cat.GetBookByPath(t.Context(), libID, rel)
	if errors.Is(err, catalog.ErrNotFound) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	a, err := cat.BookAttachments(t.Context(), b.ID, metadata.AllAttachmentParts)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// indexedAt is the book's indexed_at, which changes only when it is re-indexed
// (its audio read again).
func indexedAt(t *testing.T, db *store.DB, libID int64, rel string) string {
	t.Helper()
	var at string
	if err := db.QueryRowContext(t.Context(), `SELECT indexed_at FROM books WHERE library_id = ? AND rel_path = ?`, libID, rel).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// TestScannerReadsAttachments: a full scan lists each .audiosilo folder once and
// reads its known files for the book they belong to: a folder book's own, a
// collection's and a root file's per stem, a joined disc set's in its folder. The
// folder is never a book, nothing in it is audio.
func TestScannerReadsAttachments(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	scanner := NewScanner(cat, "", slog.Default())
	root := t.TempDir()
	past := time.Now().Add(-time.Hour)
	for _, p := range []string{"A/Book/audio.m4b", "Coll/One.m4b", "Coll/Two.m4b", "Loose.m4b", "Discs/Book/CD1/a.m4b", "Discs/Book/CD2/b.m4b"} {
		copyFixtureM4B(t, filepath.Join(root, filepath.FromSlash(p)))
	}
	writeAt(t, root, "A/Book/.audiosilo/characters.json", testCharacters, past)
	writeAt(t, root, "A/Book/.audiosilo/recaps.json", testRecaps, past)
	writeAt(t, root, "A/Book/.audiosilo/bridge.json", testBridge, past)
	writeAt(t, root, "A/Book/.audiosilo/notes.txt", "not ours", past)
	writeAt(t, root, "Coll/.audiosilo/One.characters.json", testCharacters, past)
	writeAt(t, root, "Coll/.audiosilo/Two.recaps.json", testRecaps, past)
	writeAt(t, root, ".audiosilo/Loose.recaps.json", testRecaps, past)
	writeAt(t, root, "Discs/Book/.audiosilo/characters.json", testCharacters, past)
	copyFixtureM4B(t, filepath.Join(root, "A", "Book", ".audiosilo", "stray.m4b"))

	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if err := cat.SetFolderOverride(ctx, lib.ID, "Coll", catalog.OverrideCollection); err != nil {
		t.Fatal(err)
	}
	if err := cat.SetFolderOverride(ctx, lib.ID, "Discs/Book", catalog.OverrideBook); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if got := bookPaths(t, cat, lib.ID); !slices.Equal(got, []string{"A/Book", "Coll/One.m4b", "Coll/Two.m4b", "Discs/Book", "Loose.m4b"}) {
		t.Fatalf("books = %q", got)
	}

	a := attachmentsAt(t, cat, lib.ID, "A/Book")
	if a == nil || a.Characters == nil || a.Characters.Characters[0].ID != "hero" || a.Recaps == nil || a.Recaps.InShort != "Short." ||
		a.Provenance == nil || a.Provenance.JobID != 7 || a.Provenance.Kind != "sidecars.generate" {
		t.Fatalf("folder book = %+v", a)
	}
	if a := attachmentsAt(t, cat, lib.ID, "Coll/One.m4b"); a == nil || a.Characters == nil || a.Recaps != nil {
		t.Errorf("collection book one = %+v, want characters only", a)
	}
	if a := attachmentsAt(t, cat, lib.ID, "Coll/Two.m4b"); a == nil || a.Characters != nil || a.Recaps == nil {
		t.Errorf("collection book two = %+v, want recaps only", a)
	}
	if a := attachmentsAt(t, cat, lib.ID, "Loose.m4b"); a == nil || a.Recaps == nil {
		t.Errorf("root file = %+v, want its recaps", a)
	}
	if a := attachmentsAt(t, cat, lib.ID, "Discs/Book"); a == nil || a.Characters == nil {
		t.Errorf("joined disc set = %+v, want its folder's characters", a)
	}

	// The Bridge rewrites characters.json and drops recaps.json: the audio is the
	// same, so the book is not re-indexed (no tag or ffprobe read: indexed_at holds),
	// but its attachments are read again.
	before := indexedAt(t, db, lib.ID, "A/Book")
	writeAt(t, root, "A/Book/.audiosilo/characters.json",
		`{"work":"w","characters":[{"id":"villain","name":"Villain","reveal":{"chapter":4}}]}`, time.Now())
	if err := os.Remove(filepath.Join(root, "A", "Book", ".audiosilo", "recaps.json")); err != nil {
		t.Fatal(err)
	}
	res, err := scanner.Scan(ctx, *lib)
	if err != nil || res.Added+res.Updated != 0 {
		t.Fatalf("rescan: %+v err=%v, want no re-index", res.ScanCounts, err)
	}
	if got := indexedAt(t, db, lib.ID, "A/Book"); got != before {
		t.Errorf("indexed_at %q -> %q: the audio was read again", before, got)
	}
	if a := attachmentsAt(t, cat, lib.ID, "A/Book"); a == nil || a.Characters.Characters[0].ID != "villain" || a.Recaps != nil || a.Provenance == nil {
		t.Errorf("after the rewrite = %+v", a)
	}

	// A book indexed before the columns existed reads its files once.
	if _, err := db.ExecContext(ctx, `UPDATE books SET attachments_indexed_at = NULL, attachments_sig = ''`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM book_attachments`); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if a := attachmentsAt(t, cat, lib.ID, "Coll/Two.m4b"); a == nil || a.Recaps == nil {
		t.Errorf("after the backfill = %+v", a)
	}

	// All gone: the book has none.
	if err := os.RemoveAll(filepath.Join(root, "A", "Book", ".audiosilo")); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if a := attachmentsAt(t, cat, lib.ID, "A/Book"); a != nil {
		t.Errorf("after the folder went = %+v", a)
	}

	// Indexed on demand, they are read too; a delete of the book takes them.
	writeAt(t, root, "A/Book/.audiosilo/recaps.json", testRecaps, time.Now())
	if _, err := scanner.IndexPath(ctx, *lib, "A/Book"); err != nil {
		t.Fatal(err)
	}
	if a := attachmentsAt(t, cat, lib.ID, "A/Book"); a == nil || a.Recaps == nil {
		t.Errorf("IndexPath = %+v", a)
	}
	if err := cat.DeleteBookAt(ctx, lib.ID, "A/Book"); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM book_attachments a LEFT JOIN books b ON b.id = a.book_id WHERE b.id IS NULL`).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("orphaned attachment rows = %d, %v", rows, err)
	}
}

// testDescription is a description.json the server takes (200 code points or
// more), and testShortDescription one it refuses (too short).
var (
	testDescription      = `{"work":"w","text":"` + strings.Repeat("A harbour town waits. ", 12) + `","license":"CC-BY-SA-4.0","sources":[]}`
	testShortDescription = `{"work":"w","text":"Too short.","license":"CC-BY-SA-4.0","sources":[]}`
)

// TestScannerReadsDescription: description.json is a known attachment for a
// folder book and, by stem, for a single-file book; a book with only a
// description has generated content (Book.Generated); one that fails its checks
// is left out; the refresh answers it. A book without one keeps the signature it
// had before the name was known (characters, recaps, bridge, in that order).
func TestScannerReadsDescription(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	scanner := NewScanner(cat, "", slog.Default())
	root := t.TempDir()
	past := time.Now().Add(-time.Hour)
	for _, p := range []string{"A/Book/audio.m4b", "A/Other/audio.m4b", "Loose.m4b", "Bad/Book/audio.m4b"} {
		copyFixtureM4B(t, filepath.Join(root, filepath.FromSlash(p)))
	}
	writeAt(t, root, "A/Book/.audiosilo/description.json", testDescription, past)
	writeAt(t, root, ".audiosilo/Loose.description.json", testDescription, past)
	writeAt(t, root, "Bad/Book/.audiosilo/description.json", testShortDescription, past)
	writeAt(t, root, "Bad/Book/.audiosilo/recaps.json", testRecaps, past)
	for _, name := range []string{"characters.json", "recaps.json", "bridge.json"} {
		body := map[string]string{"characters.json": testCharacters, "recaps.json": testRecaps, "bridge.json": testBridge}[name]
		writeAt(t, root, "A/Other/.audiosilo/"+name, body, past)
	}
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"A/Book", "Loose.m4b"} {
		a := attachmentsAt(t, cat, lib.ID, rel)
		if a == nil || a.Description == nil || a.Description.Work != "w" || a.Characters != nil || a.Recaps != nil {
			t.Fatalf("%s = %+v, want the description alone", rel, a)
		}
		if b, err := cat.GetBookByPath(ctx, lib.ID, rel); err != nil || !b.Generated {
			t.Errorf("%s: generated = %v, %v; want true for a description alone", rel, b != nil && b.Generated, err)
		}
	}
	if a := attachmentsAt(t, cat, lib.ID, "Bad/Book"); a == nil || a.Description != nil || a.Recaps == nil {
		t.Errorf("a refused description = %+v, want the recaps beside it only", a)
	}
	var sig string
	if err := db.QueryRowContext(ctx, `SELECT attachments_sig FROM books WHERE rel_path = 'A/Other'`).Scan(&sig); err != nil {
		t.Fatal(err)
	}
	if got := metadata.AttachmentSigNames(sig); !slices.Equal(got, []string{"characters.json", "recaps.json", "bridge.json"}) {
		t.Errorf("signature without a description names %q", got)
	}
	writeAt(t, root, "A/Other/.audiosilo/description.json", testDescription, time.Now())
	got, err := scanner.RefreshAttachments(ctx, *lib, "A/Other")
	if err != nil || got.Attachments != (AttachmentsHeld{Characters: true, Recaps: true, Description: true, Bridge: true}) {
		t.Fatalf("refresh = %+v, %v", got, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT attachments_sig FROM books WHERE rel_path = 'A/Other'`).Scan(&sig); err != nil {
		t.Fatal(err)
	}
	if got := metadata.AttachmentSigNames(sig); !slices.Equal(got, []string{"characters.json", "recaps.json", "description.json", "bridge.json"}) {
		t.Errorf("signature with a description names %q", got)
	}
}

// testChapterList is a chapters.json for Loose.m4b.
const testChapterList = `{"version":1,"generator":"audiosilo-bridge","from":"narration",
	"files":[{"path":"Loose.m4b","size":1,"duration_ms":2000}],
	"chapters":[{"title":"Chapter 1","start_ms":0,"length_ms":1000},{"title":"Chapter 2","start_ms":1000,"length_ms":1000}]}`

// TestScannerReadsChapterList: a scan reads a book's chapters.json (checked)
// into its attachments and its signature, a file that fails the checks is
// stored as none (its signature kept, so it isn't read again), and the
// attachments rescan answers it.
func TestScannerReadsChapterList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	scanner := NewScanner(cat, "", slog.Default())
	root := t.TempDir()
	past := time.Now().Add(-time.Hour)
	for _, p := range []string{"Loose.m4b", "Bad.m4b", "Later.m4b"} {
		copyFixtureM4B(t, filepath.Join(root, p))
	}
	writeAt(t, root, ".audiosilo/Loose.chapters.json", testChapterList, past)
	writeAt(t, root, ".audiosilo/Bad.chapters.json", strings.Replace(testChapterList, `"version":1`, `"version":9`, 1), past)
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	stored := func(rel string) (sig, chapters string) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT b.attachments_sig, COALESCE(a.chapters, '') FROM books b
		      LEFT JOIN book_attachments a ON a.book_id = b.id WHERE b.rel_path = ?`, rel).Scan(&sig, &chapters); err != nil {
			t.Fatal(err)
		}
		return sig, chapters
	}
	if sig, chapters := stored("Loose.m4b"); !slices.Equal(metadata.AttachmentSigNames(sig), []string{"chapters.json"}) ||
		!strings.Contains(chapters, `"title":"Chapter 2"`) {
		t.Fatalf("loose = %q, %q", sig, chapters)
	}
	if sig, chapters := stored("Bad.m4b"); !slices.Equal(metadata.AttachmentSigNames(sig), []string{"chapters.json"}) || chapters != "" {
		t.Fatalf("a refused file = %q, %q; want its signature and no chapters", sig, chapters)
	}
	if b, err := cat.GetBookByPath(ctx, lib.ID, "Loose.m4b"); err != nil || b.Generated {
		t.Errorf("chapters alone: generated = %v, %v; want false", b != nil && b.Generated, err)
	}
	writeAt(t, root, ".audiosilo/Later.chapters.json", strings.ReplaceAll(testChapterList, "Loose.m4b", "Later.m4b"), time.Now())
	got, err := scanner.RefreshAttachments(ctx, *lib, "Later.m4b")
	if err != nil || got.Attachments != (AttachmentsHeld{Chapters: true}) {
		t.Fatalf("refresh = %+v, %v", got, err)
	}
	if _, chapters := stored("Later.m4b"); chapters == "" {
		t.Errorf("the refreshed chapters weren't stored")
	}
}

// TestScannerRefusesBadAttachments: a link (to a valid file) is never read, nor
// is a file over the size cap or one that fails its checks; the other files
// beside them still are. A link to a whole .audiosilo folder is never listed.
func TestScannerRefusesBadAttachments(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	scanner := NewScanner(cat, "", slog.Default())
	root := t.TempDir()
	outside := t.TempDir()
	writeAt(t, outside, "characters.json", testCharacters, time.Now())
	writeAt(t, outside, ".audiosilo/characters.json", testCharacters, time.Now())
	for _, p := range []string{"Linked/audio.m4b", "Big/audio.m4b", "Bad/audio.m4b", "LinkedDir/audio.m4b"} {
		copyFixtureM4B(t, filepath.Join(root, filepath.FromSlash(p)))
	}
	if err := os.MkdirAll(filepath.Join(root, "Linked", ".audiosilo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "characters.json"), filepath.Join(root, "Linked", ".audiosilo", "characters.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, ".audiosilo"), filepath.Join(root, "LinkedDir", ".audiosilo")); err != nil {
		t.Fatal(err)
	}
	writeAt(t, root, "Big/.audiosilo/characters.json", `{"characters":[],"pad":"`+strings.Repeat("x", 4<<20)+`"}`, time.Now())
	writeAt(t, root, "Big/.audiosilo/recaps.json", testRecaps, time.Now())
	writeAt(t, root, "Bad/.audiosilo/characters.json", `{"characters":[{"id":"","name":"No id","reveal":{"chapter":1}}]}`, time.Now())

	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"Linked", "Bad", "LinkedDir"} {
		if a := attachmentsAt(t, cat, lib.ID, p); a != nil {
			t.Errorf("%s = %+v, want none", p, a)
		}
	}
	if a := attachmentsAt(t, cat, lib.ID, "Big"); a == nil || a.Characters != nil || a.Recaps == nil {
		t.Errorf("Big = %+v, want its recaps only", a)
	}
	// Refused files keep their signature: an unchanged one isn't read again.
	var sig string
	if err := db.QueryRowContext(ctx, `SELECT attachments_sig FROM books WHERE rel_path = 'Bad'`).Scan(&sig); err != nil || !strings.HasPrefix(sig, "characters.json:") {
		t.Errorf("Bad's signature = %q, %v", sig, err)
	}
	// Through IndexPath and the attachments rescan too.
	for _, p := range []string{"Linked", "LinkedDir"} {
		if _, err := scanner.IndexPath(ctx, *lib, p); err != nil {
			t.Fatal(err)
		}
		if got, err := scanner.RefreshAttachments(ctx, *lib, p); err != nil || got.Attachments.Characters {
			t.Errorf("refresh %s = %+v, %v; want nothing", p, got, err)
		}
		if a := attachmentsAt(t, cat, lib.ID, p); a != nil {
			t.Errorf("%s after IndexPath = %+v, want none", p, a)
		}
	}
}

// TestScannerKeepsAttachmentsItCantRead: a .audiosilo folder that can't be listed
// right now, or a file in it that can't be read, never reads as "no
// attachments": a scan, IndexPath and the attachments refresh (which answers an
// error) keep what is stored, never a part of the set, and the next scan that
// can read them records them; a folder that is gone reads as none.
func TestScannerKeepsAttachmentsItCantRead(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions that bind (not root)")
	}
	t.Parallel()
	ctx := context.Background()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	scanner := NewScanner(cat, "", slog.Default())
	root := t.TempDir()
	copyFixtureM4B(t, filepath.Join(root, "A", "Book", "audio.m4b"))
	past := time.Now().Add(-time.Hour)
	writeAt(t, root, "A/Book/.audiosilo/characters.json", testCharacters, past)
	writeAt(t, root, "A/Book/.audiosilo/recaps.json", testRecaps, past)
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	stored := func(when, character string) {
		t.Helper()
		a := attachmentsAt(t, cat, lib.ID, "A/Book")
		if a == nil || a.Characters == nil || a.Characters.Characters[0].ID != character || a.Recaps == nil {
			t.Fatalf("%s: stored = %+v, want %s's characters and the recaps", when, a, character)
		}
	}
	stored("first scan", "hero")

	// The folder can't be listed: a scan, the refresh and IndexPath keep them.
	dir := filepath.Join(root, "A", "Book", ".audiosilo")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	stored("a scan that can't list the folder", "hero")
	if _, err := scanner.RefreshAttachments(ctx, *lib, "A/Book"); err == nil || errors.Is(err, ErrNotIndexable) {
		t.Errorf("refresh of a folder that can't be listed: %v, want an error", err)
	}
	stored("the refresh", "hero")
	if _, err := scanner.IndexPath(ctx, *lib, "A/Book"); err != nil {
		t.Fatal(err)
	}
	stored("IndexPath", "hero")

	// One file can't be read: the other's new version isn't recorded alone.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	recaps := filepath.Join(dir, "recaps.json")
	if err := os.Chmod(recaps, 0o000); err != nil {
		t.Fatal(err)
	}
	writeAt(t, root, "A/Book/.audiosilo/characters.json",
		`{"work":"w","characters":[{"id":"villain","name":"Villain","reveal":{"chapter":4}}]}`, time.Now())
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	stored("a scan that can't read one file", "hero")
	if _, err := scanner.IndexPath(ctx, *lib, "A/Book"); err != nil {
		t.Fatal(err)
	}
	stored("IndexPath that can't read one file", "hero")

	// Readable again: the next scan reads them all (the row was left unchecked).
	if err := os.Chmod(recaps, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	stored("readable again", "villain")

	// Gone: none.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if a := attachmentsAt(t, cat, lib.ID, "A/Book"); a != nil {
		t.Errorf("after the folder went = %+v, want none", a)
	}
}

// TestRefreshAttachments: the Bridge's attachments-only rescan records the
// book's files without re-indexing it, from the book's path or a path inside it,
// and finds no book where none is indexed.
func TestRefreshAttachments(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	scanner := NewScanner(cat, "", slog.Default())
	root := t.TempDir()
	copyFixtureM4B(t, filepath.Join(root, "A", "Book", "01.m4b"))
	copyFixtureM4B(t, filepath.Join(root, "A", "Book", "02.m4b"))
	copyFixtureM4B(t, filepath.Join(root, "Loose.m4b"))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	before := indexedAt(t, db, lib.ID, "A/Book")
	writeAt(t, root, "A/Book/.audiosilo/characters.json", testCharacters, time.Now())
	writeAt(t, root, "A/Book/.audiosilo/bridge.json", testBridge, time.Now())
	got, err := scanner.RefreshAttachments(ctx, *lib, "A/Book/02.m4b")
	if err != nil || *got != (AttachmentsRefresh{Path: "A/Book", Attachments: AttachmentsHeld{Characters: true, Bridge: true}}) {
		t.Fatalf("refresh = %+v, %v", got, err)
	}
	if after := indexedAt(t, db, lib.ID, "A/Book"); after != before {
		t.Errorf("indexed_at %q -> %q: the audio was read again", before, after)
	}
	if a := attachmentsAt(t, cat, lib.ID, "A/Book"); a == nil || a.Characters == nil || a.Provenance == nil {
		t.Errorf("stored = %+v", a)
	}
	// The next scan has nothing to read again: the signature is recorded.
	var sig string
	if err := db.QueryRowContext(ctx, `SELECT attachments_sig FROM books WHERE rel_path = 'A/Book'`).Scan(&sig); err != nil || sig == "" {
		t.Errorf("signature = %q, %v", sig, err)
	}
	writeAt(t, root, ".audiosilo/Loose.characters.json", testCharacters, time.Now())
	if got, err := scanner.RefreshAttachments(ctx, *lib, "Loose.m4b"); err != nil || !got.Attachments.Characters {
		t.Errorf("root file = %+v, %v", got, err)
	}
	for _, p := range []string{"Nobody/Here", "A/Book/.audiosilo/characters.json", "A"} {
		if _, err := scanner.RefreshAttachments(ctx, *lib, p); !errors.Is(err, ErrNotIndexable) {
			t.Errorf("refresh %q: %v, want ErrNotIndexable", p, err)
		}
	}
}

// TestIndexPathFollowsAMove: a book folder renamed on disk and opened by its new
// path before a scan (a Folders tap) takes its listening state with it, as the
// scan's move detection would (server#209); a copy whose source is still on disk
// does not, and a book indexed already is left as it is.
func TestIndexPathFollowsAMove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	scanner := NewScanner(cat, "", slog.Default())
	root := t.TempDir()
	copyFixtureM4B(t, filepath.Join(root, "Old", "Book", "audio.m4b"))
	writeFile(t, root, "Other/Book/audio.m4b", []byte("not the same audio at all"))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	uid := namedUser(t, db, "ann")
	old := catalog.Ref{LibraryID: lib.ID, Path: "Old/Book"}
	if _, err := cat.SaveProgress(ctx, uid, catalog.Progress{Ref: old, Position: 42}); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.AddBookmark(ctx, uid, catalog.Bookmark{Ref: old, Position: 7, Note: "here"}); err != nil {
		t.Fatal(err)
	}
	state := func(p string) (*catalog.Progress, []catalog.Bookmark) {
		t.Helper()
		ref := catalog.Ref{LibraryID: lib.ID, Path: p}
		prog, err := cat.GetProgress(ctx, uid, ref)
		if err != nil {
			t.Fatal(err)
		}
		marks, err := cat.ListBookmarks(ctx, uid, ref)
		if err != nil {
			t.Fatal(err)
		}
		return prog, marks
	}

	// Renamed, then opened by the new path with no scan between.
	if err := os.Rename(filepath.Join(root, "Old"), filepath.Join(root, "New")); err != nil {
		t.Fatal(err)
	}
	b, err := scanner.IndexPath(ctx, *lib, "New/Book")
	if err != nil || b.RelPath != "New/Book" {
		t.Fatalf("IndexPath = %+v, %v", b, err)
	}
	if p, marks := state("New/Book"); p == nil || p.Position != 42 || len(marks) != 1 || marks[0].Note != "here" {
		t.Errorf("state at the new path = %+v, %+v", p, marks)
	}
	if p, marks := state("Old/Book"); p != nil || len(marks) != 0 {
		t.Errorf("state left at the old path = %+v, %+v", p, marks)
	}
	if got := bookPaths(t, cat, lib.ID); slices.Contains(got, "Old/Book") {
		t.Errorf("the old row stayed: %q", got)
	}

	// A copy opened while its source is still on disk: indexed as a new book, the
	// source keeps its state.
	copyFixtureM4B(t, filepath.Join(root, "Copy", "Book", "audio.m4b"))
	if _, err := scanner.IndexPath(ctx, *lib, "Copy/Book"); err != nil {
		t.Fatal(err)
	}
	if p, _ := state("Copy/Book"); p != nil {
		t.Errorf("progress copied to the copy: %+v", p)
	}
	if p, marks := state("New/Book"); p == nil || p.Position != 42 || len(marks) != 1 {
		t.Errorf("the source lost its state: %+v, %+v", p, marks)
	}

	// A book indexed already is re-read in place, even with a gone twin: the
	// Copy's twin New is removed from disk, and Copy keeps its own (empty) state.
	if err := os.RemoveAll(filepath.Join(root, "New")); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.IndexPath(ctx, *lib, "Copy/Book"); err != nil {
		t.Fatal(err)
	}
	if p, _ := state("Copy/Book"); p != nil {
		t.Errorf("an indexed book took a gone twin's progress: %+v", p)
	}
	if p, _ := state("New/Book"); p == nil {
		t.Error("a gone book's state moved to an indexed book")
	}
}

// TestIndexPathFollowsAMoveWhenCancelled: once a renamed book's state has moved
// to its new path, the on-demand index finishes and the old row goes even when
// its caller gives up meanwhile (the request ended mid-read), so the state never
// sits on a path with no row while the gone book stays listed without it.
func TestIndexPathFollowsAMoveWhenCancelled(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in ffprobe is a shell script")
	}
	ctx := context.Background()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	copyFixtureM4B(t, filepath.Join(root, "Old", "Book", "audio.m4b"))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if _, err := NewScanner(cat, "", slog.Default()).Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	uid := namedUser(t, db, "ann")
	if _, err := cat.SaveProgress(ctx, uid, catalog.Progress{Ref: catalog.Ref{LibraryID: lib.ID, Path: "Old/Book"}, Position: 42}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "Old"), filepath.Join(root, "New")); err != nil {
		t.Fatal(err)
	}
	// A stand-in ffprobe, run once the state has moved (the read after the
	// carry), that notes its start and takes a while.
	started := filepath.Join(t.TempDir(), "started")
	probe := filepath.Join(t.TempDir(), "ffprobe")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\ntouch '"+started+"'\nsleep 0.5\necho '{}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		for reqCtx.Err() == nil {
			if _, err := os.Stat(started); err == nil {
				cancel() // the request goes away mid-read
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	if _, err := NewScanner(cat, probe, slog.Default()).IndexPath(reqCtx, *lib, "New/Book"); err != nil {
		t.Fatalf("IndexPath after its state moved: %v", err)
	}
	if reqCtx.Err() == nil {
		t.Fatal("the request was never cancelled mid-read")
	}
	if got := bookPaths(t, cat, lib.ID); !slices.Equal(got, []string{"New/Book"}) {
		t.Fatalf("books = %q, want the moved book only", got)
	}
	if p, err := cat.GetProgress(ctx, uid, catalog.Ref{LibraryID: lib.ID, Path: "New/Book"}); err != nil || p == nil || p.Position != 42 {
		t.Fatalf("progress at the new path = %+v, %v", p, err)
	}
}

// TestIndexMoved: a book the Bridge moved keeps its listeners' state when the
// old path is gone and the fingerprints match, and its old row goes; a `from`
// still on disk, or another book's, is ignored.
func TestIndexMoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	scanner := NewScanner(cat, "", slog.Default())
	root := t.TempDir()
	copyFixtureM4B(t, filepath.Join(root, "Old", "Book", "audio.m4b"))
	copyFixtureM4B(t, filepath.Join(root, "Kept", "Book", "audio.m4b"))
	writeFile(t, root, "Other/Book/audio.m4b", []byte("not the same audio at all"))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	uid := namedUser(t, db, "ann")
	for _, p := range []string{"Old/Book", "Kept/Book", "Other/Book"} {
		if _, err := cat.SaveProgress(ctx, uid, catalog.Progress{Ref: catalog.Ref{LibraryID: lib.ID, Path: p}, Position: 42}); err != nil {
			t.Fatal(err)
		}
	}
	progressAt := func(p string) *catalog.Progress {
		t.Helper()
		got, err := cat.GetProgress(ctx, uid, catalog.Ref{LibraryID: lib.ID, Path: p})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	// Moved: the state follows, the old row goes, and no scan was needed.
	if err := os.Rename(filepath.Join(root, "Old"), filepath.Join(root, "New")); err != nil {
		t.Fatal(err)
	}
	b, err := scanner.IndexMoved(ctx, *lib, "New/Book", "Old/Book")
	if err != nil || b.RelPath != "New/Book" {
		t.Fatalf("IndexMoved = %+v, %v", b, err)
	}
	if p := progressAt("New/Book"); p == nil || p.Position != 42 {
		t.Errorf("progress at the new path = %+v", p)
	}
	if p := progressAt("Old/Book"); p != nil {
		t.Errorf("progress left at the old path = %+v", p)
	}
	if got := bookPaths(t, cat, lib.ID); slices.Contains(got, "Old/Book") {
		t.Errorf("the old row stayed: %q", got)
	}

	// A copy, its source still on disk: ignored, the copy indexed as new.
	copyFixtureM4B(t, filepath.Join(root, "Copy", "Book", "audio.m4b"))
	if _, err := scanner.IndexMoved(ctx, *lib, "Copy/Book", "Kept/Book"); err != nil {
		t.Fatal(err)
	}
	if p := progressAt("Kept/Book"); p == nil || p.Position != 42 {
		t.Errorf("a book still on disk lost its progress: %+v", p)
	}
	if p := progressAt("Copy/Book"); p != nil {
		t.Errorf("progress copied to the copy: %+v", p)
	}

	// A source still on disk that can't be checked right now (its folder
	// unlistable: a permission, a flaky share) counts as present: ignored, its
	// state stays where it is.
	if os.Geteuid() != 0 { // root lists any folder
		copyFixtureM4B(t, filepath.Join(root, "Locked", "Book", "audio.m4b"))
		if _, err := scanner.IndexPath(ctx, *lib, "Locked/Book"); err != nil {
			t.Fatal(err)
		}
		if _, err := cat.SaveProgress(ctx, uid, catalog.Progress{Ref: catalog.Ref{LibraryID: lib.ID, Path: "Locked/Book"}, Position: 42}); err != nil {
			t.Fatal(err)
		}
		copyFixtureM4B(t, filepath.Join(root, "Unlocked", "Book", "audio.m4b"))
		locked := filepath.Join(root, "Locked", "Book")
		if err := os.Chmod(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		_, err := scanner.IndexMoved(ctx, *lib, "Unlocked/Book", "Locked/Book")
		if cerr := os.Chmod(locked, 0o755); cerr != nil {
			t.Fatal(cerr)
		}
		if err != nil {
			t.Fatal(err)
		}
		if p := progressAt("Locked/Book"); p == nil || p.Position != 42 {
			t.Errorf("an unlistable book still on disk lost its progress: %+v", p)
		}
		if got := bookPaths(t, cat, lib.ID); !slices.Contains(got, "Locked/Book") {
			t.Errorf("an unlistable book still on disk lost its row: %q", got)
		}
	}

	// Another book's path (its fingerprint differs): ignored too.
	if err := os.RemoveAll(filepath.Join(root, "Other")); err != nil {
		t.Fatal(err)
	}
	copyFixtureM4B(t, filepath.Join(root, "Third", "Book", "audio.m4b"))
	if _, err := scanner.IndexMoved(ctx, *lib, "Third/Book", "Other/Book"); err != nil {
		t.Fatal(err)
	}
	if p := progressAt("Third/Book"); p != nil {
		t.Errorf("another book's progress carried: %+v", p)
	}
	if got := bookPaths(t, cat, lib.ID); !slices.Contains(got, "Other/Book") {
		t.Errorf("an ignored from lost its row: %q", got)
	}
}
