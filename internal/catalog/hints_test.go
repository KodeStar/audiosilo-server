package catalog

import (
	"testing"
)

// applyHints applies a new import for userID (from the fixture's ABS) that
// writes only hints, and returns its id.
func (f *importFixture) applyHints(t *testing.T, userID int64, hints ...BookHint) int64 {
	t.Helper()
	imps, err := f.c.CreateImports(f.ctx, []Import{{UserID: userID, Source: ImportSourceABS, SourceURL: "http://abs"}})
	if err != nil {
		t.Fatal(err)
	}
	id := imps[0].ID
	if err := f.c.FinishImportFetch(f.ctx, id, "abs", []byte("payload"), ImportPlan{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.ApplyImport(f.ctx, id, func(ImportState) (*ImportWrite, error) {
		return &ImportWrite{Hints: hints}, nil
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// hintOf is the book at ref's ABS hint as the match dialog reads it (GetBook),
// "" "" for none.
func (f *importFixture) hintOf(t *testing.T, ref Ref) (asin, isbn string) {
	t.Helper()
	b, err := f.c.GetBookByPath(f.ctx, ref.LibraryID, ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	return b.HintASIN, b.HintISBN
}

// TestImportHints: an apply writes its hints normalized (an invalid identifier
// dropped, a hint left with none skipped), never as the book's identifiers, so
// the book stays unmatched and a match run's list of it carries them; the undo
// takes them out again.
func TestImportHints(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t)
	id := f.applyHints(t, f.user,
		BookHint{Ref: f.book, ASIN: " b0hint0001 ", ISBN: "978-0-00-000000-2"},
		BookHint{Ref: f.other, ASIN: "not an asin", ISBN: "12"})

	if asin, isbn := f.hintOf(t, f.book); asin != "B0HINT0001" || isbn != "9780000000002" {
		t.Fatalf("hint = %q %q, want both normalized", asin, isbn)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM book_hints`); n != 1 {
		t.Fatalf("%d hint rows, want one (the invalid hint skipped)", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM book_hints WHERE import_id = ? AND source = 'abs'`, id); n != 1 {
		t.Fatalf("the row isn't the import's: %d", n)
	}
	b, _ := f.c.GetBookByPath(f.ctx, f.lib, f.book.Path)
	if b.ASIN != "" || b.ISBN != "" {
		t.Fatalf("book identifiers = %q %q, want none: a hint is never the book's", b.ASIN, b.ISBN)
	}
	unmatched, err := f.c.UnmatchedBooks(f.ctx, f.lib)
	if err != nil {
		t.Fatal(err)
	}
	var listed *Book
	for i := range unmatched {
		if unmatched[i].RelPath == f.book.Path {
			listed = &unmatched[i]
		}
	}
	if listed == nil || listed.HintASIN != "B0HINT0001" || listed.HintISBN != "9780000000002" {
		t.Fatalf("unmatched list = %+v, want the book with its hint", listed)
	}

	if _, err := f.c.UndoImport(f.ctx, id); err != nil {
		t.Fatal(err)
	}
	if asin, isbn := f.hintOf(t, f.book); asin != "" || isbn != "" {
		t.Fatalf("after undo: %q %q, want none", asin, isbn)
	}
}

// TestImportHintsReimportAndOwner: a re-import replaces the first's hints whole
// (one it no longer carries goes), and the last import applied owns a hint: an
// earlier import's undo leaves the hints a later one wrote over, the later one's
// undo takes them.
func TestImportHintsReimportAndOwner(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t)
	first := f.applyHints(t, f.user, BookHint{Ref: f.book, ASIN: "B0FIRST001"}, BookHint{Ref: f.other, ISBN: "9780000000003"})
	second := f.applyHints(t, f.user, BookHint{Ref: f.book, ASIN: "B0SECOND01"})
	if asin, _ := f.hintOf(t, f.book); asin != "B0SECOND01" {
		t.Fatalf("after the re-import: %q", asin)
	}
	if _, isbn := f.hintOf(t, f.other); isbn != "" {
		t.Fatalf("the first import's hint the re-import doesn't carry stayed: %q", isbn)
	}
	if d, _ := f.c.GetImport(f.ctx, first); d.Status != ImportUndone {
		t.Fatalf("first = %s, want undone", d.Status)
	}

	// Another person's import over the same books takes the hint over.
	other := seedUserNamed(t, f.c, f.ctx, "jo")
	third := f.applyHints(t, other, BookHint{Ref: f.book, ASIN: "B0THIRD001"}, BookHint{Ref: f.other, ASIN: "B0OTHER001"})
	if _, err := f.c.UndoImport(f.ctx, second); err != nil {
		t.Fatal(err)
	}
	if asin, _ := f.hintOf(t, f.book); asin != "B0THIRD001" {
		t.Fatalf("an earlier import's undo took the later one's hint: %q", asin)
	}
	if _, err := f.c.UndoImport(f.ctx, third); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM book_hints`); n != 0 {
		t.Fatalf("%d hints after every import was undone", n)
	}
}

// TestImportHintsAreDurable: a hint is path-keyed state: a rebuild keeps it, a
// move carries it (a book whose only durable state it is too), a join copies the
// first part's, and its library's delete takes it.
func TestImportHintsAreDurable(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t)
	parts := []Ref{{LibraryID: f.lib, Path: "Joined/CD1"}, {LibraryID: f.lib, Path: "Joined/CD2"}}
	for _, p := range parts {
		if _, err := f.c.UpsertBook(f.ctx, scannedBook(f.lib, p.Path)); err != nil {
			t.Fatal(err)
		}
	}
	f.applyHints(t, f.user, BookHint{Ref: f.other, ASIN: "B0MOVED001"},
		BookHint{Ref: parts[0], ASIN: "B0DISC0001"}, BookHint{Ref: parts[1], ASIN: "B0DISC0002"})

	if _, err := f.c.DeleteBooksNotIn(f.ctx, f.lib, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{f.other.Path, parts[0].Path, parts[1].Path} {
		if _, err := f.c.UpsertBook(f.ctx, scannedBook(f.lib, p)); err != nil {
			t.Fatal(err)
		}
	}
	if asin, _ := f.hintOf(t, f.other); asin != "B0MOVED001" {
		t.Fatalf("after a rebuild: %q", asin)
	}

	moved := Ref{LibraryID: f.lib, Path: "Moved/Book"}
	if err := f.c.MoveDurableState(f.ctx, f.lib, f.other.Path, moved.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.UpsertBook(f.ctx, scannedBook(f.lib, moved.Path)); err != nil {
		t.Fatal(err)
	}
	if asin, _ := f.hintOf(t, moved); asin != "B0MOVED001" {
		t.Fatalf("after a move: %q", asin)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM book_hints WHERE path = ?`, f.other.Path); n != 0 {
		t.Fatalf("%d hints left at the old path", n)
	}

	if err := f.c.JoinDurableState(f.ctx, f.lib, "Joined", []JoinPart{{Path: parts[0].Path},
		{Path: parts[1].Path, Offset: 1800, Last: true}}, 3600); err != nil {
		t.Fatal(err)
	}
	joined := Ref{LibraryID: f.lib, Path: "Joined"}
	if _, err := f.c.UpsertBook(f.ctx, scannedBook(f.lib, joined.Path)); err != nil {
		t.Fatal(err)
	}
	if asin, _ := f.hintOf(t, joined); asin != "B0DISC0001" {
		t.Fatalf("after a join: %q, want the first part's", asin)
	}

	if err := f.c.DeleteLibrary(f.ctx, f.lib); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM book_hints`); n != 0 {
		t.Fatalf("%d hints after the library went", n)
	}
}

// TestImportBooksMatched: the importer's books say which are matched: an ASIN,
// an ISBN or a recorded community match (matchedExpr), never a hint.
func TestImportBooksMatched(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t)
	f.applyHints(t, f.user, BookHint{Ref: f.book, ASIN: "B0HINT0001"})
	if _, err := f.c.db.ExecContext(f.ctx, `INSERT INTO book_matches(library_id, path, work_id, how, set_at)
		VALUES(?, ?, 'a-work', 'auto', 't')`, f.lib, f.other.Path); err != nil {
		t.Fatal(err)
	}
	books, err := f.c.ImportBooks(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, b := range books {
		got[b.RelPath] = b.Matched
	}
	if got[f.book.Path] || !got[f.other.Path] {
		t.Fatalf("matched = %+v, want only the book with a recorded match", got)
	}
}
