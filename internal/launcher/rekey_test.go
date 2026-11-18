package launcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// A scan that indexes a new book moves the rows a client stored under its
// track before it was indexed (server#210): the scanner's OnRunFinished
// re-keys the library, then hands the run on (the notifications).
func TestScanEndRekeysANewBook(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	res, err := db.ExecContext(ctx, `INSERT INTO users(username, password_hash, role, created_at, updated_at)
		VALUES('ann', 'x', 'admin', 't', 't')`)
	if err != nil {
		t.Fatal(err)
	}
	ann, _ := res.LastInsertId()
	// The book's track, saved before any scan saw the book.
	const book, track = "Author/Book", "Author/Book/01 - Unsouled.m4b"
	if _, err := db.ExecContext(ctx, `INSERT INTO progress(user_id, library_id, rel_path, position, duration, updated_at)
		VALUES(?, ?, ?, 42, 0, '2026-01-01T00:00:00Z')`, ann, lib.ID, track); err != nil {
		t.Fatal(err)
	}
	audio, err := os.ReadFile(filepath.Join("..", "..", "testdata", "library", "Will Wight", "Cradle", "01 - Unsouled.m4b"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Author", "Book"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(track)), audio, 0o644); err != nil {
		t.Fatal(err)
	}

	scanner := library.NewScanner(cat, "", discardLog())
	heard := make(chan library.RunReport, 1)
	scanner.OnRunFinished = rekeyAfterScan(ctx, cat, discardLog(), func(_ context.Context, r library.RunReport) { heard <- r })
	scanner.Start(ctx)
	scanner.Enqueue(*lib, library.TriggerManual, nil)
	select {
	case r := <-heard:
		if r.Status != catalog.RunOK {
			t.Fatalf("scan ended %s", r.Status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the scan never finished")
	}
	p, err := cat.GetProgress(ctx, ann, catalog.Ref{LibraryID: lib.ID, Path: book})
	if err != nil || p == nil || p.Position != 42 {
		t.Fatalf("progress on the new book = %+v (%v), want the track's 42", p, err)
	}
	if left, _ := cat.GetProgress(ctx, ann, catalog.Ref{LibraryID: lib.ID, Path: track}); left != nil {
		t.Errorf("progress left on the track: %+v", left)
	}
	var logged int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM path_rekeys WHERE from_path = ?`, track).Scan(&logged); err != nil || logged != 1 {
		t.Errorf("logged %d rows (%v), want the moved one", logged, err)
	}
}
