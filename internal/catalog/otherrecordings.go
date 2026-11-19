package catalog

import (
	"context"
	"database/sql"
	"slices"
	"strings"

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
// The candidates are the work's books (workBooksFrom, by the work id indexes).
// The scope filter keeps every book outside the caller's grant out before any
// copy is dropped, so a copy the caller cannot open never stands in for one
// they can. Copies of book itself that would teach the rule nothing
// (notCopyOfSQL) are left out in SQL, so a work of hundreds of copies of one
// recording is not read row by row (server#413); the rest are dropped as the
// rows are read, before the limit, and the read stops at the limit's book.
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
// in order, the copies notCopyOfSQL keeps included (OtherRecordings drops them).
func otherRecordingsQuery(scopes []Scope, book *Book) (string, []any) {
	from, args := workBooksFrom(scopes, false, []string{book.WorkID})
	notCopy, cargs := notCopyOfSQL("b", book.ASIN, book.ISBN)
	args = slices.Concat(args, []any{book.LibraryID, book.RelPath}, cargs, []any{book.LibraryID})
	return `SELECT ` + prefixCols("b.") + from + `
		   AND NOT (b.library_id = ? AND b.rel_path = ?) AND ` + notCopy + `
		 ORDER BY b.library_id <> ?, b.title COLLATE NOCASE, b.narrator COLLATE NOCASE, b.library_id, b.rel_path`, args
}

// notCopyOfSQL is the SQL of "book t is not a copy of a book with asin and isbn
// that match.Recordings would learn nothing from": t sharing its normalized
// ASIN while carrying no ISBN or the same one, or, with no ASIN of its own,
// sharing its normalized ISBN. A copy carrying another ISBN is still read (that
// ISBN marks the recording's ISBN-only copies), so the rule answers the same
// with these rows read or not. "1" for a book with neither.
func notCopyOfSQL(t, asin, isbn string) (string, []any) {
	asin, isbn = match.NormalizeASIN(asin), match.NormalizeISBN(isbn)
	var ors []string
	var args []any
	if asin != "" {
		ors = append(ors, `(`+normASINSQL(t+`.asin`)+` = ? AND `+normISBNSQL(t+`.isbn`)+` IN ('', ?))`)
		args = append(args, asin, isbn)
	}
	if isbn != "" {
		ors = append(ors, `(`+normASINSQL(t+`.asin`)+` = '' AND `+normISBNSQL(t+`.isbn`)+` = ?)`)
		args = append(args, isbn)
	}
	if len(ors) == 0 {
		return "1", nil
	}
	return `NOT (` + strings.Join(ors, ` OR `) + `)`, args
}

// workBooksFrom is the FROM and WHERE of the books of books libraries whose
// work key (workIDExpr) is one of works, within scopes unless all, aliased b
// (and l, its library), and its args. The candidates are found by the work id
// indexes on both sides (idx_book_matches_work, idx_upcoming_owned_work) and
// each kept only when its own work key is one of works, so a book whose
// recorded match names another work is not one of them whatever its
// identifiers resolved to. INDEXED BY on the recorded matches: a caller
// reaching one library has its library_id pushed down into both candidate
// reads, and SQLite (no ANALYZE statistics here) then reads every recorded
// match of that library by the primary key's prefix rather than the work's few
// by idx_book_matches_work. upcoming_owned needs no hint: its work index covers
// both columns.
func workBooksFrom(scopes []Scope, all bool, works []string) (string, []any) {
	in := `(` + placeholders(len(works)) + `)`
	ids := anyOf(works)
	q := ` FROM (
		    SELECT library_id, path FROM book_matches INDEXED BY idx_book_matches_work WHERE work_id IN ` + in + `
		    UNION
		    SELECT library_id, path FROM upcoming_owned WHERE work_id IN ` + in + `
		  ) w
		  CROSS JOIN books b ON b.library_id = w.library_id AND b.rel_path = w.path
		  CROSS JOIN libraries l ON l.id = b.library_id
		 WHERE l.kind = 'books' AND ` + workIDExpr("b") + ` IN ` + in
	args := slices.Concat(ids, ids, ids)
	if !all {
		frag, fargs := scopesFilterSQL("b.library_id", "b.rel_path", scopes)
		q += ` AND ` + frag
		args = append(args, fargs...)
	}
	return q, args
}

// maxWorkBooks bounds WorkBooks, as maxSeriesBooks bounds SeriesBooks.
const maxWorkBooks = maxSeriesBooks

// WorkBooks returns the books within scopes whose work key (Book.WorkID) is one
// of works, books libraries only, in placement preference order (library sort
// order, then path, as SeriesBooks), list shape, at most maxWorkBooks: what
// places a /meta envelope's rails on every recording the caller holds of their
// works, one filed in no series or under another series name too, as /item's
// other_recordings finds them (server#412).
func (c *Catalog) WorkBooks(ctx context.Context, scopes []Scope, works []string) ([]Book, error) {
	if len(scopes) == 0 || len(works) == 0 {
		return nil, nil
	}
	from, args := workBooksFrom(scopes, false, works)
	return queryRows(ctx, c.db, scanBookRow, `SELECT `+prefixCols("b.")+from+`
		 ORDER BY l.sort_order, l.name, l.id, b.rel_path LIMIT ?`, append(args, maxWorkBooks)...)
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
