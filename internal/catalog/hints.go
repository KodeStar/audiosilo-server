package catalog

import (
	"context"
	"database/sql"

	"github.com/kodestar/audiosilo-server/pkg/match"
)

// Match hints from another app (book_hints, migration 0074, server#366): the
// ASIN and ISBN Audiobookshelf has for a book, carried by an applied listening
// import (ImportWrite.Hints). Like a tag's (Book.TagASIN), a hint is only what a
// community match sends (matchrun.QueryFor: the book's own identifiers, else
// this hint, else its tags'), never the book's identifiers and never on the
// wire: the book is matched only by what a match run's apply or the match
// dialog writes. Path-keyed, durable state: a rebuild keeps it, a move carries
// it (moveBookState), its library's delete takes it (the foreign key).

// HintSourceABS is a hint an Audiobookshelf import carried (an import writes its
// own source's: ImportWrite.Hints).
const HintSourceABS = ImportSourceABS

// bookHintWhere is the WHERE clause (on book_hints h) of the Audiobookshelf hint
// of the books row a query reads (matchSubjectJoin's condition, for a query
// that can't alias books).
const bookHintWhere = `WHERE h.library_id = books.library_id AND h.path = books.rel_path AND h.source = '` + HintSourceABS + `'`

// BookHint is the identifiers a hint gives a book.
type BookHint struct {
	Ref
	ASIN string
	ISBN string
}

// Normalized is h with its identifiers normalized and an invalid one dropped,
// as a hint is stored: ok is false when neither is left.
func (h BookHint) Normalized() (BookHint, bool) {
	asin, isbn := match.NormalizeASIN(h.ASIN), match.NormalizeISBN(h.ISBN)
	if !match.ValidASIN(asin) {
		asin = ""
	}
	if !match.ValidISBN(isbn) {
		isbn = ""
	}
	return BookHint{Ref: h.Ref, ASIN: asin, ISBN: isbn}, asin != "" || isbn != ""
}

// setHintsTx writes hints from source inside tx, owned by import importID: each
// replaces the book's hint from that source whole (the last import applied owns
// it). A hint with no valid identifier is skipped. An undo deletes an import's
// hints with its other rows (undoImport).
func setHintsTx(ctx context.Context, tx *sql.Tx, source string, importID int64, hints []BookHint, now string) error {
	valid := make([]BookHint, 0, len(hints))
	for _, h := range hints {
		if n, ok := h.Normalized(); ok {
			valid = append(valid, n)
		}
	}
	return execEach(ctx, tx,
		`INSERT INTO book_hints(library_id, path, source, asin, isbn, import_id, set_at) VALUES(?,?,?,?,?,?,?)
		 ON CONFLICT(library_id, path, source) DO UPDATE SET
		     asin = excluded.asin, isbn = excluded.isbn, import_id = excluded.import_id, set_at = excluded.set_at`,
		len(valid), func(i int) ([]any, error) {
			h := valid[i]
			return []any{h.LibraryID, h.Path, source, h.ASIN, h.ISBN, importID, now}, nil
		})
}
