package library

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// TestScannerReadsABSFile: an Audiobookshelf metadata.json in a folder book's
// folder is read at the scan, re-read when it changes, appears or goes while the
// audio stays the same (no re-index), read once for a book indexed before the
// column existed, and left alone where an ignore rule names it or for a loose
// file at the root.
func TestScannerReadsABSFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	scanner := NewScanner(cat, "", slog.Default())
	root := t.TempDir()
	const p = "Will Wight/Cradle/01 - Unsouled"
	copyFixtureM4B(t, filepath.Join(root, filepath.FromSlash(p), "audio.m4b"))
	copyFixtureM4B(t, filepath.Join(root, "Loose.m4b"))
	absPath := filepath.Join(root, filepath.FromSlash(p), "metadata.json")
	writeABS := func(body string, at time.Time) {
		t.Helper()
		if err := os.WriteFile(absPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(absPath, at, at); err != nil {
			t.Fatal(err)
		}
	}
	// A root-level metadata.json belongs to no folder book.
	if err := os.WriteFile(filepath.Join(root, "metadata.json"), []byte(`{"asin":"B000000009"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeABS(`{"title":"Unsouled","authors":["Will Wight"],"series":["Cradle #1","Cradle Omnibus #1-3"],
		"asin":"B06XDLCHPF","isbn":"978-0-9967190-6-8","description":"<p>Sacred artists follow a thousand Paths.</p>"}`,
		time.Now().Add(-time.Hour))

	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "ABS", Root: root})
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	d, err := cat.AdminBookDetail(ctx, lib.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]string{
		catalog.FieldASIN: "B06XDLCHPF", catalog.FieldISBN: "9780996719068", catalog.FieldTitle: "Unsouled",
		catalog.FieldDescription: "Sacred artists follow a thousand Paths.",
		catalog.FieldMoreSeries:  `[{"name":"Cradle Omnibus","position":1,"position_end":3}]`,
	} {
		if got := d.Fields[field]; got.Value != want || got.Source != catalog.SourceABS {
			t.Errorf("%s = %q (%s), want %q from the file", field, got.Value, got.Source, want)
		}
	}
	if b, _ := cat.GetBookByPath(ctx, lib.ID, "Loose.m4b"); b.ASIN != "" {
		t.Errorf("a loose file took the root's metadata.json: asin %q", b.ASIN)
	}

	// A folder book without a file, never read (as migration 0046 leaves every
	// row), has nothing to add: the scan leaves its row unwritten.
	copyFixtureM4B(t, filepath.Join(root, "No", "File", "audio.m4b"))
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE books SET abs_indexed_at = NULL WHERE rel_path = 'No/File'`); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	var readAt *string
	if err := db.QueryRowContext(ctx, `SELECT abs_indexed_at FROM books WHERE rel_path = 'No/File'`).Scan(&readAt); err != nil || readAt != nil {
		t.Errorf("a book with no file was rewritten: abs_indexed_at %v err %v", readAt, err)
	}

	// Edited in ABS: the audio is the same, so the book isn't re-indexed, but the
	// file is read again.
	writeABS(`{"title":"Unsouled","asin":"B0NEWASIN1"}`, time.Now())
	res, err := scanner.Scan(ctx, *lib)
	if err != nil || res.Added+res.Updated != 0 {
		t.Fatalf("rescan: %+v err=%v, want no re-index", res.ScanCounts, err)
	}
	if b, _ := cat.GetBookByPath(ctx, lib.ID, p); b.ASIN != "B0NEWASIN1" || b.ISBN != "" {
		t.Errorf("after the edit: asin %q isbn %q", b.ASIN, b.ISBN)
	}

	// A book indexed before the column existed reads its file once.
	if _, err := db.ExecContext(ctx, `UPDATE books SET abs_indexed_at = NULL, abs_sig = '', abs_file = '', asin = ''`); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if b, _ := cat.GetBookByPath(ctx, lib.ID, p); b.ASIN != "B0NEWASIN1" {
		t.Errorf("after the backfill: asin %q", b.ASIN)
	}

	// An ignore rule naming the file turns it off for the library.
	ignoring := *lib
	ignoring.IgnorePatterns = []string{"metadata.json"}
	if _, err := scanner.Scan(ctx, ignoring); err != nil {
		t.Fatal(err)
	}
	if b, _ := cat.GetBookByPath(ctx, lib.ID, p); b.ASIN != "" {
		t.Errorf("an ignored file still read: asin %q", b.ASIN)
	}

	// Gone: its values go with it.
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(absPath); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	d, _ = cat.AdminBookDetail(ctx, lib.ID, p)
	if got := d.Fields[catalog.FieldASIN]; got.Value != "" {
		t.Errorf("after the file went: asin %+v", got)
	}
	if got := d.Fields[catalog.FieldTitle]; got.Source == catalog.SourceABS {
		t.Errorf("after the file went: title %+v still the file's", got)
	}

	// Indexed on demand, the file is read too.
	writeABS(`{"asin":"B0ONDEMAND"}`, time.Now().Add(time.Minute))
	if b, err := scanner.IndexPath(ctx, *lib, p); err != nil || b.ASIN != "B0ONDEMAND" {
		t.Errorf("IndexPath: asin %q err %v", b.ASIN, err)
	}
}

// TestScannerRetriesUnreadableABSFile: a metadata.json that can't be read right
// now (here, no read permission) is not taken for one that says nothing: an
// unchanged book keeps its last reading, a re-index records none, and either way
// the next scan that can read the file does.
func TestScannerRetriesUnreadableABSFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	scanner := NewScanner(cat, "", slog.Default())
	root := t.TempDir()
	const p = "Author/Book"
	copyFixtureM4B(t, filepath.Join(root, filepath.FromSlash(p), "audio.m4b"))
	absPath := filepath.Join(root, filepath.FromSlash(p), "metadata.json")
	writeABS := func(body string, at time.Time) {
		t.Helper()
		if err := os.WriteFile(absPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(absPath, at, at); err != nil {
			t.Fatal(err)
		}
	}
	setReadable := func(ok bool) {
		t.Helper()
		mode := os.FileMode(0o644)
		if !ok {
			mode = 0
		}
		if err := os.Chmod(absPath, mode); err != nil {
			t.Fatal(err)
		}
	}
	writeABS(`{"asin":"B0FIRST001"}`, time.Now().Add(-time.Hour))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "ABS", Root: root})
	asin := func() string {
		t.Helper()
		b, err := cat.GetBookByPath(ctx, lib.ID, p)
		if err != nil {
			t.Fatal(err)
		}
		return b.ASIN
	}
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if got := asin(); got != "B0FIRST001" {
		t.Fatalf("first scan: asin %q", got)
	}

	// ABS rewrote it, and it can't be read at this scan: the last reading stays.
	writeABS(`{"asin":"B0SECOND01"}`, time.Now())
	setReadable(false)
	t.Cleanup(func() { _ = os.Chmod(absPath, 0o644) })
	if _, err := os.ReadFile(absPath); err == nil {
		t.Skip("file permissions don't stop this user reading (root?)")
	}
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if got := asin(); got != "B0FIRST001" {
		t.Errorf("unreadable at the backfill: asin %q, want the last reading kept", got)
	}
	setReadable(true)
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if got := asin(); got != "B0SECOND01" {
		t.Errorf("readable again: asin %q, want the new reading", got)
	}

	// Unreadable at a re-index: the last reading stays, read again at the next scan.
	writeABS(`{"asin":"B0THIRD001"}`, time.Now().Add(time.Minute))
	setReadable(false)
	if _, err := scanner.IndexPath(ctx, *lib, p); err != nil {
		t.Fatal(err)
	}
	if got := asin(); got != "B0SECOND01" {
		t.Errorf("unreadable at a re-index: asin %q, want the last reading kept", got)
	}
	setReadable(true)
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if got := asin(); got != "B0THIRD001" {
		t.Errorf("after a re-index that couldn't read it: asin %q, want it read again", got)
	}
}

// TestABSSigAt: the walk's metadata.json signature is its lstat's; gone since
// the listing it is none, and not to be looked at it is an error (its book keeps
// the stored reading: ABSReading.Unread), never a signature of "" (no file).
func TestABSSigAt(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	lib := catalog.Library{Name: "L", Root: root}
	const rel = "Author/Book/metadata.json"
	dir := filepath.Join(root, "Author", "Book")
	file := filepath.Join(dir, "metadata.json")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(file)
	if err != nil {
		t.Fatal(err)
	}
	if sig, err := absSigAt(t.Context(), lib, dirOf(rel), nil, time.Minute); err != nil || sig != metadata.ABSSignature(info) {
		t.Errorf("file there: %q, %v; want %q", sig, err, metadata.ABSSignature(info))
	}

	// The folder can be listed but not looked into: the lstat fails too.
	if err := os.Chmod(dir, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := os.Lstat(file); err == nil {
		t.Skip("folder permissions don't stop this user (root?)")
	}
	if sig, err := absSigAt(t.Context(), lib, dirOf(rel), nil, time.Minute); err == nil {
		t.Errorf("not to be looked at: %q, nil; want an error", sig)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if sig, err := absSigAt(t.Context(), lib, dirOf(rel), nil, time.Minute); err != nil || sig != "" {
		t.Errorf("gone since the listing: %q, %v; want none", sig, err)
	}
}

// A metadata.json read by an older revision of the reading (its stored
// signature without today's absReadRev: here a credit whose names hold commas
// joined with ", ", before server#213) is read once more by the next scan, its
// audio unchanged, and then left be (server#454).
func TestScannerRereadsABSFileOfAnOlderReading(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	scanner := NewScanner(cat, "", slog.Default())
	root := t.TempDir()
	const p = "Ursula K. Le Guin/A Wizard of Earthsea"
	copyFixtureM4B(t, filepath.Join(root, filepath.FromSlash(p), "audio.m4b"))
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(p), "metadata.json"),
		[]byte(`{"title":"A Wizard of Earthsea","authors":["Le Guin, Ursula K.","Vess, Charles"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "ABS", Root: root})
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	const joined = "Le Guin, Ursula K.; Vess, Charles"
	author := func() string {
		t.Helper()
		d, err := cat.AdminBookDetail(ctx, lib.ID, p)
		if err != nil {
			t.Fatal(err)
		}
		return d.Fields[catalog.FieldAuthor].Value
	}
	if got := author(); got != joined {
		t.Fatalf("author %q, want %q", got, joined)
	}

	// As the older reading stored it: its signature with no revision, the
	// credit joined with ", ".
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(p), "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE books SET abs_sig = ?,
		abs_file = json_set(abs_file, '$.author', 'Le Guin, Ursula K., Vess, Charles')`,
		fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())); err != nil {
		t.Fatal(err)
	}
	res, err := scanner.Scan(ctx, *lib)
	if err != nil || res.Added+res.Updated != 0 {
		t.Fatalf("rescan: %+v err=%v, want no re-index", res.ScanCounts, err)
	}
	if got := author(); got != joined {
		t.Errorf("after the rescan: author %q, want %q, the file read again", got, joined)
	}

	// Read by today's revision: the next scan leaves the stored reading be.
	if _, err := db.ExecContext(ctx, `UPDATE books SET abs_file = json_set(abs_file, '$.author', 'Marker')`); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := db.QueryRowContext(ctx, `SELECT json_extract(abs_file, '$.author') FROM books`).Scan(&stored); err != nil || stored != "Marker" {
		t.Errorf("the scan after read the file again: author %q, %v", stored, err)
	}
}
