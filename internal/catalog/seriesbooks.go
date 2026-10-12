package catalog

import (
	"cmp"
	"context"
	"database/sql"
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
// by its folded key (seriesKey: case, accents, punctuation and spacing
// ignored), so a local "The Expanse" is found for a community rail named "the
// Expanse!". They come in placement preference order: library sort order, then
// path. Files and chapters are not loaded (the list shape).
//
// Index-friendly queries: the books whose main series has one of the names'
// keys within scopes (idx_books_series_key), then the books in a matching series
// only through their list (seriesMembers, book_more_series), both narrowed by the
// share's paths, so a book outside the grant is never returned.
func (c *Catalog) SeriesBooks(ctx context.Context, scopes []Scope, names []string) ([]Book, error) {
	var keys []string
	for _, n := range names {
		if match.SeriesKey(n) != "" && !slices.Contains(keys, seriesKey(n)) { // a rail named by letters or numbers
			keys = append(keys, seriesKey(n))
		}
	}
	if len(keys) == 0 || len(scopes) == 0 {
		return nil, nil
	}
	frag, fargs := scopesFilterSQL("b.library_id", "b.rel_path", scopes)
	args := slices.Concat(anyOf(keys), fargs, []any{maxSeriesBooks})
	// CROSS JOIN keeps books the outer loop, so the series' books are found by
	// idx_books_series_key (library_id, series_key) and sorted, a handful of rows.
	// As a plain JOIN, a single-library scope had SQLite walk that whole library
	// by (library_id, rel_path) to get rel_path's order for free: tens of
	// milliseconds on every /meta of a large library, for a few books.
	rows, err := c.db.QueryContext(ctx, `SELECT `+prefixCols("b.")+` FROM books b
		  CROSS JOIN libraries l ON l.id = b.library_id
		 WHERE b.series_key IN (`+placeholders(len(keys))+`) AND `+frag+`
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
	return c.seriesMembers(ctx, scopes, keys, out)
}

// seriesMembers adds to found (in placement order) the books within scopes in a
// series keyed by one of keys only through their list (book_more_series, by its
// key index), up to maxSeriesBooks in all, keeping the order: library sort order,
// then path.
func (c *Catalog) seriesMembers(ctx context.Context, scopes []Scope, keys []string, found []Book) ([]Book, error) {
	if len(found) >= maxSeriesBooks {
		return found, nil
	}
	have := make(map[int64]bool, len(found))
	for _, b := range found {
		have[b.ID] = true
	}
	inLibs := make([]any, 0, len(scopes))
	for _, s := range scopes {
		inLibs = append(inLibs, s.LibraryID)
	}
	frag, fargs := scopesFilterSQL("b.library_id", "b.rel_path", scopes)
	listed, err := queryRows(ctx, c.db, func(r *sql.Rows, id *int64) error { return r.Scan(id) },
		`SELECT DISTINCT m.book_id FROM book_more_series m CROSS JOIN books b ON b.id = m.book_id
		 WHERE m.series_key IN (`+placeholders(len(keys))+`) AND m.library_id IN (`+placeholders(len(inLibs))+`) AND `+frag,
		slices.Concat(anyOf(keys), inLibs, fargs)...)
	if err != nil {
		return nil, err
	}
	var ids []any
	for _, id := range listed {
		if !have[id] {
			ids = append(ids, id)
		}
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

// NextInSeries returns the book after book in the series it is in within one
// library and scope, list shape: every series of book.AllSeries() it has a
// position in, its main series first, then its others in list order; the first
// holding a later book that doesn't step back answers with the first such book
// (nextInOneSeries, stepsBack). A later book in one series that sits at or before
// book in a series ranked above it is no step forward - in a series numbered two
// ways (Narnia's chronological main series, its publication order listed)
// following the other numbering would loop back - so that book is passed over
// and the series' next later book is judged, in position order: after The Silver
// Chair (chronological #6, publication #4) the publication order passes over The
// Horse and His Boy (chronological #3) and answers with The Last Battle. When
// none answers, numbered reports whether any of them holds another numbered book
// in scope - the end of a numbered series - and false means its series give no
// order to follow at all (the book unnumbered, or alone in its series). numbered
// is true whenever next is found.
func (c *Catalog) NextInSeries(ctx context.Context, libraryID int64, book *Book, scope Scope) (next *Book, numbered bool, err error) {
	all := book.AllSeries()
	for k, s := range all { // unnumbered everywhere: no order to follow, nothing asked
		if s.Position <= 0 {
			continue
		}
		next, num, err := c.nextInOneSeries(ctx, libraryID, book.RelPath, s.Name, s.Position, scope,
			func(b *Book) bool { return stepsBack(all[:k], b) })
		if err != nil {
			return nil, false, err
		}
		numbered = numbered || num
		if next != nil {
			return next, true, nil
		}
	}
	return nil, numbered, nil
}

// stepsBack reports whether candidate, a later book in one of the current book's
// series, sits at or before the current book in one of earlier (the current
// book's series ranked above that one, with its positions): a series both are
// numbered in where candidate's position is not above the current book's.
func stepsBack(earlier []SeriesRef, candidate *Book) bool {
	theirs := candidate.AllSeries()
	for _, s := range earlier {
		if p := positionIn(theirs, s.Name); s.Position > 0 && p > 0 && p <= s.Position {
			return true
		}
	}
	return false
}

// nextInOneSeries returns the book after the one at relPath in the series named
// series (any spelling) within one library and scope: of the books in that series, the one with the
// smallest position in it above index (ties by path) that skip doesn't report
// (nil skips none), list shape. A book is in the series through its main series
// (at its series_index) or through an entry of its more_series (at that entry's
// position), so the book after Guards! Guards! in City Watch is found whichever
// way each book names City Watch. When there is none, numbered reports whether
// the series holds any other numbered book in scope. One query (laterSeriesMembers):
// the numbered members, those above index first, read in that order until one
// passes skip or the first at or below index, which proves the series numbered
// without a later book.
func (c *Catalog) nextInOneSeries(ctx context.Context, libraryID int64, relPath string, series string, index float64, scope Scope, skip func(*Book) bool) (next *Book, numbered bool, err error) {
	q, args := laterSeriesMembers(libraryID, relPath, series, index, scope)
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var b Book
		var pos float64
		dest, finish := bookDest(&b)
		if err := rows.Scan(append(dest, &pos)...); err != nil {
			return nil, false, err
		}
		numbered = true
		if pos <= index {
			break // every member above index has been read
		}
		finish()
		if skip == nil || !skip(&b) {
			return &b, true, nil
		}
	}
	return nil, numbered, rows.Err()
}

// laterSeriesMembers is nextInOneSeries' query, and its args: the numbered
// members of series (numberedSeriesMembers), those above index first, each group
// by position then path. Unbounded: SQLite sorts every member before the first
// row anyway, and nextInOneSeries stops reading at the first book it takes.
func laterSeriesMembers(libraryID int64, relPath string, series string, index float64, scope Scope) (string, []any) {
	q, args := numberedSeriesMembers(libraryID, relPath, series, scope)
	return `SELECT ` + bookCols + `, pos FROM (` + q + `) ORDER BY pos <= ?, pos, rel_path`, append(args, index)
}

// numberedSeriesMembers is the query, and its args, of the books in the series
// named series (any spelling: its seriesKey) within one library and scope, other
// than relPath, with a position in it (above 0): bookCols, then pos, their
// position. Its two branches are each found by an index: the books whose main
// series it is by idx_books_series_key (library_id, series_key, ...),
// and the books in it only through their list by book_more_series' key index
// (which never holds a book's main series, so a list repeating it counts once).
// Both are narrowed by the scope's paths, so a book outside the grant is never a
// member.
func numberedSeriesMembers(libraryID int64, relPath string, series string, scope Scope) (string, []any) {
	frag, fargs := pathFilterSQL("b.rel_path", scope)
	key := seriesKey(series)
	q := `SELECT ` + prefixCols("b.") + `, b.series_index AS pos FROM books b
		 WHERE b.library_id = ? AND b.series_key = ? AND b.series_index > 0 AND b.rel_path <> ? AND ` + frag + `
		UNION ALL
		SELECT ` + prefixCols("b.") + `, m.position AS pos FROM book_more_series m CROSS JOIN books b ON b.id = m.book_id
		 WHERE m.series_key = ? AND m.library_id = ? AND m.position > 0 AND b.rel_path <> ? AND ` + frag
	return q, slices.Concat(
		[]any{libraryID, key, relPath}, fargs,
		[]any{key, libraryID, relPath}, fargs)
}
