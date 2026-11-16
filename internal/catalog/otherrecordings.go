package catalog

import (
	"context"
	"database/sql"
	"errors"
	"slices"
)

// Other recordings together (server#241): the books of a books library that are
// recordings of one community work, another narrator, an abridgement, a
// dramatisation, so a book page can offer them.

// MaxOtherRecordings bounds OtherRecordings: a work has a handful of
// recordings, and the list rides on a single book's /item.
const MaxOtherRecordings = 20

// OtherRecordings returns the other books within scopes that are recordings of
// the community work the book at ref is (its work key, workIDExpr: a recorded
// match beats the work its identifiers resolved to, on both sides), list shape:
// books libraries only, not ref itself, and not a copy sharing its ASIN or ISBN
// (the same recording again, Health's duplicate). The book's own library comes
// first, then by title, then narrator, then path; at most MaxOtherRecordings.
// None when the book is unknown or has no work.
//
// The candidates are found by the work id indexes on both sides
// (idx_book_matches_work, idx_upcoming_owned_work) and each kept only when its
// own work key is the work, so a book whose recorded match names another work
// is not one of them whatever its identifiers resolved to. The scope filter
// keeps every book outside the caller's grant out.
func (c *Catalog) OtherRecordings(ctx context.Context, scopes []Scope, ref Ref) ([]Book, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	var asin, isbn, work string
	err := c.db.QueryRowContext(ctx, `SELECT asin, isbn, `+workIDExpr("books")+` FROM books
		 WHERE library_id = ? AND rel_path = ?`, ref.LibraryID, ref.Path).Scan(&asin, &isbn, &work)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && work == "") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	frag, fargs := scopesFilterSQL("b.library_id", "b.rel_path", scopes)
	args := slices.Concat([]any{work, work}, fargs, []any{ref.LibraryID, ref.Path, work, asin, isbn, ref.LibraryID, MaxOtherRecordings})
	return queryRows(ctx, c.db, func(r *sql.Rows, b *Book) error {
		dest, finish := bookDest(b)
		if err := r.Scan(dest...); err != nil {
			return err
		}
		finish()
		return nil
	}, `SELECT `+prefixCols("b.")+` FROM (
		    SELECT library_id, path FROM book_matches WHERE work_id = ?
		    UNION
		    SELECT library_id, path FROM upcoming_owned WHERE work_id = ?
		  ) w
		  CROSS JOIN books b ON b.library_id = w.library_id AND b.rel_path = w.path
		  CROSS JOIN libraries l ON l.id = b.library_id
		 WHERE l.kind = 'books' AND `+frag+`
		   AND NOT (b.library_id = ? AND b.rel_path = ?)
		   AND `+workIDExpr("b")+` = ?
		   AND NOT (b.asin <> '' AND b.asin = ?) AND NOT (b.isbn <> '' AND b.isbn = ?)
		 ORDER BY b.library_id <> ?, b.title COLLATE NOCASE, b.narrator COLLATE NOCASE, b.library_id, b.rel_path
		 LIMIT ?`, args...)
}

// InProgress reports which of refs the user has started and not finished
// (progress with a position, not finished): the recording of a work the pick
// prefers after the narrator (meta's pickRank). Only refs in it are true.
func (c *Catalog) InProgress(ctx context.Context, userID int64, refs []Ref) (map[Ref]bool, error) {
	out := map[Ref]bool{}
	byLib := map[int64][]string{}
	seen := map[Ref]bool{}
	for _, r := range refs {
		if !seen[r] {
			seen[r] = true
			byLib[r.LibraryID] = append(byLib[r.LibraryID], r.Path)
		}
	}
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
