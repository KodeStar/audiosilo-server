package library

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/store"
)

// indexCounts is how many books and file rows the index holds, and how many
// books have no file rows (a write cut short).
func indexCounts(t *testing.T, db *store.DB) (books, files, bare int) {
	t.Helper()
	if err := db.QueryRowContext(context.Background(), `SELECT
		(SELECT COUNT(*) FROM books), (SELECT COUNT(*) FROM book_files),
		(SELECT COUNT(*) FROM books b WHERE NOT EXISTS (SELECT 1 FROM book_files f WHERE f.book_id = b.id))`).
		Scan(&books, &files, &bare); err != nil {
		t.Fatal(err)
	}
	return books, files, bare
}

// A scan of more books than a batch writes them all, no batch over writeBatch,
// with its progress at the end as before; a scan finding them unchanged writes
// none.
func TestScanWritesInBatches(t *testing.T) {
	t.Parallel()
	const n = 2*writeBatch + 22
	s, _, lib, db := newTestScanner(t, smallLibrary(t, n, 1), "", 8)
	var most, taken atomic.Int64
	s.batched = func(size int) {
		taken.Add(1)
		if int64(size) > most.Load() {
			most.Store(int64(size))
		}
	}
	res, err := s.Scan(context.Background(), *lib)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != n || res.Errors != 0 {
		t.Fatalf("added %d (%d errors), want %d", res.Added, res.Errors, n)
	}
	if taken.Load() != n || most.Load() > writeBatch {
		t.Errorf("%d books taken into batches, the largest %d; want %d, at most %d", taken.Load(), most.Load(), n, writeBatch)
	}
	if books, files, bare := indexCounts(t, db); books != n || files != n || bare != 0 {
		t.Errorf("index has %d books, %d files, %d without files; want %d, %d, 0", books, files, bare, n, n)
	}
	if p := s.Progress(lib.ID); p.Total != n || p.Done != n || p.Indexed != n || p.Added != n {
		t.Errorf("progress %+v, want every book done and indexed", p)
	}

	taken.Store(0)
	res, err = s.Scan(context.Background(), *lib)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 0 || res.Updated != 0 || taken.Load() != 0 {
		t.Errorf("rescan added %d, updated %d, batched %d; want nothing written", res.Added, res.Updated, taken.Load())
	}
}

// A book whose write fails is logged and counted as an error, and the books
// written beside it (in its batch) are kept.
func TestScanWriteFailureIsTheBooksAlone(t *testing.T) {
	t.Parallel()
	s, _, lib, db := newTestScanner(t, smallLibrary(t, 5, 1), "", 4)
	// The writer pool is one connection, so its temporary trigger sees the scan's writes.
	if _, err := db.ExecContext(context.Background(), `CREATE TEMP TRIGGER fail_book AFTER INSERT ON main.book_files
		WHEN NEW.rel_path = 'Author/Book 2/01.mp3' BEGIN SELECT RAISE(ABORT, 'disk says no'); END`); err != nil {
		t.Fatal(err)
	}
	res, err := s.Scan(context.Background(), *lib)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 4 || res.Errors != 1 {
		t.Fatalf("added %d, %d errors; want 4 and 1", res.Added, res.Errors)
	}
	var logged bool
	for _, e := range res.Log {
		if e.Kind == "error" && e.Path == "Author/Book 2" {
			logged = true
		}
	}
	if !logged {
		t.Errorf("no error event for Author/Book 2 in %+v", res.Log)
	}
	if books, _, bare := indexCounts(t, db); books != 4 || bare != 0 {
		t.Errorf("index has %d books (%d without files), want the other 4, whole", books, bare)
	}
}

// A scan stopped with books waiting in its batch writes and counts them (they
// were read before the stop), and writes nothing after.
func TestScanStoppedMidBatchKeepsTheBatch(t *testing.T) {
	t.Parallel()
	const n = 3 * writeBatch
	s, _, lib, db := newTestScanner(t, smallLibrary(t, n, 1), "", 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.batched = func(int) { cancel() } // the first book batched stops the scan
	res, err := s.Scan(ctx, *lib)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the stop's", err)
	}
	books, _, bare := indexCounts(t, db)
	if books == 0 || books != res.Added || books >= n || bare != 0 {
		t.Errorf("index has %d books (%d without files), the scan counted %d added; want the batched book(s), counted and whole",
			books, bare, res.Added)
	}
}
