package library

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
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

	// Unreadable at a re-index: recorded as none for now, read at the next scan.
	setReadable(false)
	if _, err := scanner.IndexPath(ctx, *lib, p); err != nil {
		t.Fatal(err)
	}
	setReadable(true)
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if got := asin(); got != "B0SECOND01" {
		t.Errorf("after a re-index that couldn't read it: asin %q, want it read again", got)
	}
}
