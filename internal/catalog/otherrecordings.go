package catalog

import (
	"context"
	"database/sql"
	"slices"

	"github.com/kodestar/audiosilo-server/pkg/match"
)

// Other recordings together (server#241): the books of a books library that are
// recordings of one community work, another narrator, an abridgement, a
// dramatisation, so a book page can offer them.

// MaxOtherRecordings bounds OtherRecordings: a work has a handful of
// recordings, and the list rides on a single book's /item.
const MaxOtherRecordings = 20

// OtherRecordings returns the other books within scopes that are recordings of
// book's community work (its WorkID, workIDExpr), list shape: books libraries
// only, not book itself, and one book per recording (match.Recordings: a book
// sharing a normalized ASIN with book, or with one before it, or, with no ASIN
// of its own, an ISBN, is the same recording again and left out; books with
// different ASINs never are). The book's own library comes first, then
// by title, then narrator, then path, and of a recording's copies the first in
// that order is the one kept; at most MaxOtherRecordings. None when the book has
// no work.
//
// The candidates are found by the work id indexes on both sides
// (idx_book_matches_work, idx_upcoming_owned_work) and each kept only when its
// own work key is the work, so a book whose recorded match names another work
// is not one of them whatever its identifiers resolved to. The scope filter
// keeps every book outside the caller's grant out before any copy is dropped,
// so a copy the caller cannot open never stands in for one they can. Copies are
// dropped as the rows are read, before the limit: the query has none, and the
// read stops at the limit's book.
func (c *Catalog) OtherRecordings(ctx context.Context, scopes []Scope, book *Book) ([]Book, error) {
	if len(scopes) == 0 || book.WorkID == "" {
		return nil, nil
	}
	q, args := otherRecordingsQuery(scopes, book)
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var seen match.Recordings
	seen.Seen(book.ASIN, book.ISBN)
	out := []Book{}
	for len(out) < MaxOtherRecordings && rows.Next() {
		var b Book
		if err := scanBookRow(rows, &b); err != nil {
			return nil, err
		}
		if !seen.Seen(b.ASIN, b.ISBN) {
			out = append(out, b)
		}
	}
	return out, rows.Err()
}

// otherRecordingsQuery is OtherRecordings' query, and its args: every candidate
// in order, copies included (OtherRecordings drops them). INDEXED BY on the
// recorded matches: a caller reaching one library has its library_id pushed
// down into both candidate reads, and SQLite (no ANALYZE statistics here) then
// reads every recorded match of that library by the primary key's prefix
// rather than the work's few by idx_book_matches_work. upcoming_owned needs no
// hint: its work index covers both columns.
func otherRecordingsQuery(scopes []Scope, book *Book) (string, []any) {
	work := book.WorkID
	frag, fargs := scopesFilterSQL("b.library_id", "b.rel_path", scopes)
	args := slices.Concat([]any{work, work}, fargs, []any{book.LibraryID, book.RelPath, work, book.LibraryID})
	return `SELECT ` + prefixCols("b.") + ` FROM (
		    SELECT library_id, path FROM book_matches INDEXED BY idx_book_matches_work WHERE work_id = ?
		    UNION
		    SELECT library_id, path FROM upcoming_owned WHERE work_id = ?
		  ) w
		  CROSS JOIN books b ON b.library_id = w.library_id AND b.rel_path = w.path
		  CROSS JOIN libraries l ON l.id = b.library_id
		 WHERE l.kind = 'books' AND ` + frag + `
		   AND NOT (b.library_id = ? AND b.rel_path = ?)
		   AND ` + workIDExpr("b") + ` = ?
		 ORDER BY b.library_id <> ?, b.title COLLATE NOCASE, b.narrator COLLATE NOCASE, b.library_id, b.rel_path`, args
}

// InProgress reports which of refs the user has started and not finished
// (progress with a position, not finished): the recording of a work the pick
// prefers after the narrator (meta's pickRank). Only refs in it are true.
func (c *Catalog) InProgress(ctx context.Context, userID int64, refs []Ref) (map[Ref]bool, error) {
	out := map[Ref]bool{}
	_, byLib := GroupRefs(refs)
	for lib, paths := range byLib {
		for chunk := range slices.Chunk(paths, 500) {
			started, err := queryRows(ctx, c.db, func(r *sql.Rows, p *string) error { return r.Scan(p) },
				`SELECT rel_path FROM progress
				  WHERE user_id = ? AND library_id = ? AND rel_path IN (`+placeholders(len(chunk))+`)
				    AND finished = 0 AND position > 0`,
				slices.Concat([]any{userID, lib}, anyOf(chunk))...)
			if err != nil {
				return nil, err
			}
			for _, p := range started {
				out[Ref{LibraryID: lib, Path: p}] = true
			}
		}
	}
	return out, nil
}
