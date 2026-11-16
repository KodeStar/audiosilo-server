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
// only, not book itself, and not a copy sharing its ASIN or ISBN, compared in
// their normalized forms (match.NormalizeASIN, match.NormalizeISBN: a tag's
// case, spaces and hyphens aside), the same recording again. The book's own
// library comes first, then by title, then narrator, then path; at most
// MaxOtherRecordings. None when the book has no work.
//
// The candidates are found by the work id indexes on both sides
// (idx_book_matches_work, idx_upcoming_owned_work) and each kept only when its
// own work key is the work, so a book whose recorded match names another work
// is not one of them whatever its identifiers resolved to. The scope filter
// keeps every book outside the caller's grant out.
func (c *Catalog) OtherRecordings(ctx context.Context, scopes []Scope, book *Book) ([]Book, error) {
	if len(scopes) == 0 || book.WorkID == "" {
		return nil, nil
	}
	q, args := otherRecordingsQuery(scopes, book)
	return queryRows(ctx, c.db, scanBookRow, q, args...)
}

// otherRecordingsQuery is OtherRecordings' query, and its args. INDEXED BY on
// the recorded matches: a caller reaching one library has its library_id pushed
// down into both candidate reads, and SQLite (no ANALYZE statistics here) then
// reads every recorded match of that library by the primary key's prefix
// rather than the work's few by idx_book_matches_work. upcoming_owned needs no
// hint: its work index covers both columns.
func otherRecordingsQuery(scopes []Scope, book *Book) (string, []any) {
	work := book.WorkID
	frag, fargs := scopesFilterSQL("b.library_id", "b.rel_path", scopes)
	asin, isbn := match.NormalizeASIN(book.ASIN), match.NormalizeISBN(book.ISBN)
	args := slices.Concat([]any{work, work}, fargs,
		[]any{book.LibraryID, book.RelPath, work, asin, asin, isbn, isbn, book.LibraryID, MaxOtherRecordings})
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
		   AND NOT (? <> '' AND UPPER(TRIM(b.asin)) = ?)
		   AND NOT (? <> '' AND UPPER(REPLACE(REPLACE(b.isbn, '-', ''), ' ', '')) = ?)
		 ORDER BY b.library_id <> ?, b.title COLLATE NOCASE, b.narrator COLLATE NOCASE, b.library_id, b.rel_path
		 LIMIT ?`, args
}

// InProgress reports which of refs the user has started and not finished
// (progress with a position, not finished): the recording of a work the pick
// prefers after the narrator (meta's pickRank). Only refs in it are true.
func (c *Catalog) InProgress(ctx context.Context, userID int64, refs []Ref) (map[Ref]bool, error) {
	out := map[Ref]bool{}
	_, byLib := groupRefs(refs)
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
