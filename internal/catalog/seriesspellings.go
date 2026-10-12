package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"slices"
	"strings"
)

// One series, several spellings (Health > Series spelled several ways): a series
// is one series however its books spell it (seriesKey), so the Series page and
// every filter already treat "He Who Fights With Monsters" and "...WIth..." as
// one. The spellings still differ book by book, in the players' book details, the
// Books list and the export. The issue lists such a series once per library, and
// the console's fix sets one spelling on every book of it (a bulk edit of series,
// as an author merge is; the files are not changed).

// seriesSpellingsExpr is "another book of b's library spells b's series another
// way": one spelling below b's or one above, each a range seek on
// idx_books_series_key (library_id, series_key, series, ...), so a long series
// of one spelling costs two seeks per book, not a walk of the series.
const seriesSpellingsExpr = `(b.series_key <> '' AND (EXISTS(SELECT 1 FROM books s
		WHERE s.library_id = b.library_id AND s.series_key = b.series_key AND s.series < b.series)
	OR EXISTS(SELECT 1 FROM books s
		WHERE s.library_id = b.library_id AND s.series_key = b.series_key AND s.series > b.series)))`

// SeriesSpelling is one way a series' books spell it, and how many do.
type SeriesSpelling struct {
	Name  string `json:"name"`
	Books int    `json:"books"`
}

// SeriesSpellingGroup is a series of one library spelled several ways: the
// spelling to offer (seriesName, the Series page's rule, over this library's main
// series only, so it can differ from the Series page's name when other libraries
// or more_series lists spell it too), each spelling with its books (most first,
// ties alphabetical), and how many books it has.
type SeriesSpellingGroup struct {
	LibraryID int64            `json:"library_id"`
	Suggested string           `json:"suggested"`
	Spellings []SeriesSpelling `json:"spellings"`
	Books     int              `json:"books"`
}

// maxSpellingGroups bounds one answer: a library rarely holds more, and the
// rest come back once those are settled.
const maxSpellingGroups = 200

// SeriesSpellingGroups returns the series spelled several ways within a library
// (libraryID 0: in each library), by name: the books seriesSpellingsExpr lists
// (the Health count's predicate), grouped by library and key. Open groups are
// those with a book an admin hasn't ignored for the issue; with ignored, those
// with a book an admin has. Every spelling of a group is listed, ignored books'
// too, so a rename leaves none behind.
func (c *Catalog) SeriesSpellingGroups(ctx context.Context, libraryID int64, ignored bool) ([]SeriesSpellingGroup, error) {
	type row struct {
		lib                int64
		key, series        string
		matched, isIgnored bool
	}
	lib, args := "", []any{IssueSeriesSpellings}
	if libraryID != 0 {
		lib, args = " AND b.library_id = ?", append(args, libraryID)
	}
	rows, err := queryRows(ctx, c.db, func(r *sql.Rows, x *row) error {
		return r.Scan(&x.lib, &x.key, &x.series, &x.matched, &x.isIgnored)
	}, `SELECT b.library_id, b.series_key, b.series, `+matchedExpr+`, `+ignoredExpr+` FROM books b
		 WHERE `+seriesSpellingsExpr+lib, args...)
	if err != nil {
		return nil, err
	}
	type acc struct {
		spellings map[string]spelling
		books     int
		show      bool
	}
	type groupKey struct {
		lib int64
		key string
	}
	groups := map[groupKey]*acc{}
	for _, x := range rows {
		k := groupKey{x.lib, x.key}
		a := groups[k]
		if a == nil {
			a = &acc{spellings: map[string]spelling{}}
			groups[k] = a
		}
		sp := a.spellings[x.series]
		sp.books++
		if x.matched {
			sp.matched++
		}
		a.spellings[x.series] = sp
		a.books++
		a.show = a.show || x.isIgnored == ignored
	}
	out := []SeriesSpellingGroup{}
	for k, a := range groups {
		if !a.show {
			continue
		}
		g := SeriesSpellingGroup{LibraryID: k.lib, Suggested: seriesName(a.spellings), Books: a.books}
		for name, sp := range a.spellings {
			g.Spellings = append(g.Spellings, SeriesSpelling{Name: name, Books: sp.books})
		}
		slices.SortFunc(g.Spellings, func(x, y SeriesSpelling) int {
			return cmp.Or(cmp.Compare(y.Books, x.Books), strings.Compare(x.Name, y.Name))
		})
		out = append(out, g)
	}
	slices.SortFunc(out, func(x, y SeriesSpellingGroup) int {
		return cmp.Or(compareFold(x.Suggested, y.Suggested), cmp.Compare(x.LibraryID, y.LibraryID))
	})
	return out[:min(len(out), maxSpellingGroups)], nil
}
