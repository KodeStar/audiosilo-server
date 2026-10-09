package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"github.com/kodestar/audiosilo-server/pkg/match"
)

// The player's series lookups: the caller's books in a community series (to
// place them on its rails) and the next book of a local series (GET
// /libraries/{id}/next).

// maxSeriesBooks bounds SeriesBooks. A series rail holds tens of works, and a
// /meta envelope at most a few rails with their orderings, so this leaves room
// for every copy of every book of a long series in several libraries while
// keeping the rows one envelope's lookup returns bounded whatever the library
// holds.
const maxSeriesBooks = 1000

// SeriesBooks returns the books within scopes whose series matches one of names
// by its folded key (match.SeriesKey: case, accents, punctuation and spacing
// ignored), so a local "The Expanse" is found for a community rail named "the
// Expanse!". They come in placement preference order: library sort order, then
// path. Files and chapters are not loaded (the list shape).
//
// Index-friendly queries: the distinct series spellings of the caller's
// libraries (idx_books_series, never a table row), folded here to find the
// spellings that match, then the books of exactly those spellings within scopes,
// then the books in a matching series only through their more_series
// (seriesMembers). The first is not narrowed by the share's paths, since a
// spelling is only compared, never returned; the others are, so a book outside
// the grant is never returned.
func (c *Catalog) SeriesBooks(ctx context.Context, scopes []Scope, names []string) ([]Book, error) {
	want := map[string]bool{}
	for _, n := range names {
		if k := match.SeriesKey(n); k != "" {
			want[k] = true
		}
	}
	if len(want) == 0 || len(scopes) == 0 {
		return nil, nil
	}
	spellings, err := c.seriesSpellings(ctx, scopes, want)
	if err != nil {
		return nil, err
	}
	if len(spellings) == 0 {
		return c.seriesMembers(ctx, scopes, want, nil)
	}
	frag, fargs := scopesFilterSQL("b.library_id", "b.rel_path", scopes)
	args := make([]any, 0, len(spellings)+len(fargs)+1)
	for _, n := range spellings {
		args = append(args, n)
	}
	args = append(append(args, fargs...), maxSeriesBooks)
	// CROSS JOIN keeps books the outer loop, so the series' books are found by
	// idx_books_series (library_id, series) and sorted, a handful of rows. As a
	// plain JOIN, a single-library scope had SQLite walk that whole library by
	// (library_id, rel_path) to get rel_path's order for free: tens of
	// milliseconds on every /meta of a large library, for a few books.
	rows, err := c.db.QueryContext(ctx, `SELECT `+prefixCols("b.")+` FROM books b
		  CROSS JOIN libraries l ON l.id = b.library_id
		 WHERE b.series IN (`+placeholders(len(spellings))+`) AND `+frag+`
		 ORDER BY l.sort_order, l.name, l.id, b.rel_path LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Book
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return c.seriesMembers(ctx, scopes, want, out)
}

// memberIDs runs q (selecting a book's id, series and more_series) and returns the
// ids of the books not in have whose series include a wanted one.
func (c *Catalog) memberIDs(ctx context.Context, q string, args []any, want map[string]bool, have map[int64]bool) ([]any, error) {
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []any
	for rows.Next() {
		var id int64
		var series, more string
		if err := rows.Scan(&id, &series, &more); err != nil {
			return nil, err
		}
		if !have[id] && slices.ContainsFunc(seriesList(series, 0, more), func(s SeriesRef) bool { return want[match.SeriesKey(s.Name)] }) {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// seriesMembers adds to found (in placement order) the books within scopes in a
// wanted series only through their more_series (few books have one, so a plain
// read of those), up to maxSeriesBooks in all, keeping the order: library sort
// order, then path.
func (c *Catalog) seriesMembers(ctx context.Context, scopes []Scope, want map[string]bool, found []Book) ([]Book, error) {
	if len(found) >= maxSeriesBooks {
		return found, nil
	}
	have := make(map[int64]bool, len(found))
	for _, b := range found {
		have[b.ID] = true
	}
	// The partial index of the books that have a list (idx_books_more_series)
	// finds them by the scopes' libraries; +library_id in the scope terms keeps
	// SQLite from walking each whole library by its library index instead.
	args := make([]any, 0, len(scopes))
	for _, s := range scopes {
		args = append(args, s.LibraryID)
	}
	frag, fargs := scopesFilterSQL("+b.library_id", "b.rel_path", scopes)
	// Only the series columns of the books with a list (a matched library can hold
	// many), the whole rows of just the ones in a wanted series after.
	ids, err := c.memberIDs(ctx, `SELECT b.id, b.series, b.more_series FROM books b
		 WHERE b.more_series <> '[]' AND b.library_id IN (`+placeholders(len(scopes))+`) AND `+frag,
		append(args, fargs...), want, have)
	if err != nil {
		return nil, err
	}
	mainCount := len(found)
	for len(ids) > 0 { // every match (ids come unordered), then the order and the cap
		part := ids[:min(len(ids), 500)]
		ids = ids[len(part):]
		more, err := queryRows(ctx, c.db, func(r *sql.Rows, b *Book) error {
			dest, finish := bookDest(b)
			if err := r.Scan(dest...); err != nil {
				return err
			}
			finish()
			return nil
		}, `SELECT `+prefixCols("b.")+` FROM books b CROSS JOIN libraries l ON l.id = b.library_id
			 WHERE b.id IN (`+placeholders(len(part))+`) ORDER BY l.sort_order, l.name, l.id, b.rel_path`, part...)
		if err != nil {
			return nil, err
		}
		found = append(found, more...)
	}
	if len(found) == mainCount || (mainCount == 0 && len(found) <= 500) {
		return found[:min(len(found), maxSeriesBooks)], nil // one ordered list
	}
	// Two ordered lists (or several chunks): one placement order, so a more_series
	// book in an earlier library (or at an earlier path) is still preferred.
	libs, err := c.ListLibraries(ctx)
	if err != nil {
		return nil, err
	}
	rank := make(map[int64]int, len(libs))
	for i, l := range libs {
		rank[l.ID] = i
	}
	slices.SortStableFunc(found, func(a, b Book) int {
		return cmp.Or(cmp.Compare(rank[a.LibraryID], rank[b.LibraryID]), strings.Compare(a.RelPath, b.RelPath))
	})
	return found[:min(len(found), maxSeriesBooks)], nil
}

// seriesSpellings returns the distinct series values of scopes' libraries whose
// folded key is in want.
func (c *Catalog) seriesSpellings(ctx context.Context, scopes []Scope, want map[string]bool) ([]string, error) {
	args := make([]any, len(scopes))
	for i, s := range scopes {
		args[i] = s.LibraryID
	}
	rows, err := c.db.QueryContext(ctx, `SELECT DISTINCT series FROM books
		 WHERE library_id IN (`+placeholders(len(scopes))+`) AND series <> ''`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var series string
		if err := rows.Scan(&series); err != nil {
			return nil, err
		}
		if want[match.SeriesKey(series)] {
			out = append(out, series)
		}
	}
	return out, rows.Err()
}

// NextInSeries returns the book after the one at relPath in its series within
// one library and scope: the book of exactly that series with the smallest
// series_index above index (ties by path), list shape. When there is none,
// numbered reports whether the series holds any other numbered book in scope -
// the end of a numbered series - and false means the series gives no order to
// follow at all. numbered is true whenever next is found.
func (c *Catalog) NextInSeries(ctx context.Context, libraryID int64, relPath, series string, index float64, scope Scope) (next *Book, numbered bool, err error) {
	frag, fargs := pathFilterSQL("rel_path", scope)
	args := append([]any{libraryID, series, index, relPath}, fargs...)
	next, err = scanBook(c.db.QueryRowContext(ctx, `SELECT `+bookCols+` FROM books
		 WHERE library_id = ? AND series = ? AND series_index > ? AND rel_path <> ? AND `+frag+`
		 ORDER BY series_index, rel_path LIMIT 1`, args...))
	switch {
	case err == nil:
		return next, true, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, false, err
	}
	args = append([]any{libraryID, series, relPath}, fargs...)
	err = c.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM books
		 WHERE library_id = ? AND series = ? AND series_index > 0 AND rel_path <> ? AND `+frag+`)`,
		args...).Scan(&numbered)
	return nil, numbered, err
}
