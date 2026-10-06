package catalog

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/kodestar/audiosilo-server/pkg/match"
)

// The player's series lookups: the caller's books in a community series (to
// place them on its rails) and the next book of a local series (GET
// /libraries/{id}/next).

// maxSeriesBooks bounds SeriesBooks. A series rail holds tens of works, and a
// /meta envelope at most a few rails with their orderings, so this leaves room
// for every copy of every book of a long series in several libraries while
// keeping one envelope's lookup bounded whatever the library holds.
const maxSeriesBooks = 1000

// SeriesBooks returns the books within scopes whose series matches one of names
// by its folded key (match.SeriesKey: case, accents, punctuation and spacing
// ignored), so a local "The Expanse" is found for a community rail named "the
// Expanse!". They come in placement preference order: library sort order, then
// path. Files and chapters are not loaded (the list shape).
//
// Two index-friendly queries: the distinct series spellings of the caller's
// libraries (idx_books_series, never a table row), folded here to find the
// spellings that match, then the books of exactly those spellings within each
// scope. The first is not narrowed by the share's paths, since a spelling is
// only compared, never returned; the second is, so a book outside the grant is
// never returned.
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
	if err != nil || len(spellings) == 0 {
		return nil, err
	}
	var conds []string
	var args []any
	for _, s := range scopes {
		names := spellings[s.LibraryID]
		if len(names) == 0 {
			continue
		}
		frag, fargs := pathFilterSQL("b.rel_path", s)
		conds = append(conds, "(b.library_id = ? AND b.series IN ("+placeholders(len(names))+") AND "+frag+")")
		args = append(args, s.LibraryID)
		for _, n := range names {
			args = append(args, n)
		}
		args = append(args, fargs...)
	}
	if len(conds) == 0 {
		return nil, nil
	}
	args = append(args, maxSeriesBooks)
	rows, err := c.db.QueryContext(ctx, `SELECT `+prefixCols("b.")+` FROM books b
		  JOIN libraries l ON l.id = b.library_id
		 WHERE (`+strings.Join(conds, " OR ")+`)
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
	return out, rows.Err()
}

// seriesSpellings returns, per library of scopes, the series values whose
// folded key is in want.
func (c *Catalog) seriesSpellings(ctx context.Context, scopes []Scope, want map[string]bool) (map[int64][]string, error) {
	args := make([]any, len(scopes))
	for i, s := range scopes {
		args[i] = s.LibraryID
	}
	rows, err := c.db.QueryContext(ctx, `SELECT DISTINCT library_id, series FROM books
		 WHERE library_id IN (`+placeholders(len(scopes))+`) AND series <> ''`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var libID int64
		var series string
		if err := rows.Scan(&libID, &series); err != nil {
			return nil, err
		}
		if want[match.SeriesKey(series)] {
			out[libID] = append(out[libID], series)
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
