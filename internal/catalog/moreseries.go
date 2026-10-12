package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"

	"github.com/kodestar/audiosilo-server/pkg/match"
)

// A book's main series is the series / series_index pair every client knows. A
// book can belong to more (Guards! Guards! is Discworld #8 and Discworld: City
// Watch #1): those are the more_series field, a list of the other series with the
// book's position in each, which only an edit or a community match supplies (tags
// name one series). It is an override field like any other - provenance, revert,
// undo and clearing community values work on it unchanged - whose value is the
// list as canonical JSON ("" for none). An admin edit that makes one of the other
// series the main one swaps the two (seriesSwap, overrides.go): the old main
// series takes its place in the list.

// FieldMoreSeries is the overridable list of the other series a book belongs to.
const FieldMoreSeries = "more_series"

// SeriesRef is one series a book belongs to, with its position in it (0 = none,
// as for series_index).
type SeriesRef struct {
	Name     string  `json:"name"`
	Position float64 `json:"position"`
}

// maxMoreSeries bounds the list: real books sit in a handful of series.
const maxMoreSeries = 20

// normalizeMoreSeries validates a more_series value (a JSON list of {name,
// position}) and returns its canonical form: names trimmed and checked like the
// series field, blank names dropped, each series once (the first spelling,
// seriesKey), positions from 0
// to 100000. "" for an empty list.
func normalizeMoreSeries(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	var in []SeriesRef
	if err := json.Unmarshal([]byte(v), &in); err != nil {
		return "", invalid(FieldMoreSeries, "must be a list of {name, position} with a numeric position")
	}
	out := make([]SeriesRef, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		s.Name = strings.TrimSpace(s.Name)
		if err := checkText(FieldMoreSeries, s.Name, maxShortField); err != nil {
			return "", err
		}
		if s.Name == "" || seen[seriesKey(s.Name)] {
			continue
		}
		seen[seriesKey(s.Name)] = true
		if s.Position < 0 || s.Position > maxSeriesIndex {
			return "", invalid(FieldMoreSeries, "a position must be a number from 0 to 100000")
		}
		out = append(out, s)
	}
	if len(out) > maxMoreSeries {
		return "", invalid(FieldMoreSeries, "too many series")
	}
	return EncodeMoreSeries(out), nil
}

// EncodeMoreSeries is the canonical more_series value for refs ("" for none):
// without HTML escaping, so "Tom & Jerry" is written as the console's
// JSON.stringify writes it and the two compare equal.
func EncodeMoreSeries(refs []SeriesRef) string {
	if len(refs) == 0 {
		return ""
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(refs) // a slice of plain structs always encodes
	return strings.TrimSuffix(b.String(), "\n")
}

// ParseMoreSeries reads a stored more_series value (canonical JSON; "" or "[]"
// for none). A value it can't read names no series.
func ParseMoreSeries(v string) []SeriesRef {
	if v == "" || v == "[]" {
		return nil // most books: no parse
	}
	var out []SeriesRef
	_ = json.Unmarshal([]byte(v), &out)
	return out
}

// AllSeries is every series the book is in, its main one first: SeriesList, which
// the scan sets for a book in more than one, else its main series alone.
func (b *Book) AllSeries() []SeriesRef {
	if b.SeriesList != nil {
		return b.SeriesList
	}
	return SeriesList(b.Series, b.SeriesIndex, "")
}

// SeriesList is every series a book belongs to: the main one first (when it has
// one), then the others, each series once (seriesKey: the first spelling).
func SeriesList(series string, index float64, more string) []SeriesRef {
	out := []SeriesRef{}
	if series != "" {
		out = append(out, SeriesRef{Name: series, Position: index})
	}
	for _, s := range ParseMoreSeries(more) {
		// A series once, however the list spells it again.
		if !slices.ContainsFunc(out, func(o SeriesRef) bool { return sameSeries(o.Name, s.Name) }) {
			out = append(out, s)
		}
	}
	return out
}

// One series, however its books spell it: series group, filter and order by
// seriesKey, so books tagged "He Who Fights With Monsters", "He Who Fights with
// Monsters" and "He Who Fights WIth Monsters" are one series. Spellings that
// differ by more than case, accents, punctuation or spacing stay apart: no
// article or "Series" is dropped and nothing is fuzzy, so two series never merge
// by being alike.

// seriesKey is the key a series is one series by: match.SeriesKey (the
// community rails' key, so a series and the rail named like it agree), or, for a
// name with no letter or number, the name itself (so "?!" is a series of its own,
// not one with every other such name).
func seriesKey(name string) string { return seriesKeys.get(name) }

var seriesKeys = &memo[string]{f: func(name string) string { return cmp.Or(match.SeriesKey(name), name) }}

// sameSeries reports whether two names are one series (seriesKey).
func sameSeries(a, b string) bool { return a == b || seriesKey(a) == seriesKey(b) }

// seriesNames is one series as the index spells it: the main series values and
// the more_series names that are that series (seriesKey). A filter or an
// ordering on one series takes them all, exactly, so an index finds them.
type seriesNames struct {
	main, more []string
}

// spellings returns, by seriesKey, how the index spells each of names' series in
// libraries (none: every library): the distinct series values (idx_books_series
// in order, no table row) and, with more, the more_series names of the books
// that have a list (idx_books_more_series, reading each list; a caller that
// matches main series only leaves it off). A name the index doesn't hold is
// absent. Spellings are only compared, never returned, so the share's paths
// don't narrow them.
func (c *Catalog) spellings(ctx context.Context, libraries []int64, more bool, names ...string) (map[string]seriesNames, error) {
	want := map[string]bool{}
	for _, n := range names {
		want[seriesKey(n)] = true
	}
	lib, args := "", anyOf(libraries)
	if len(libraries) > 0 {
		lib = " AND b.library_id IN (" + placeholders(len(libraries)) + ")"
	}
	// By library and name, not name alone: the index's own order, so no sort.
	main, err := c.distinctNames(ctx, `SELECT DISTINCT b.library_id, b.series FROM books b WHERE b.series <> ''`+lib, args)
	if err != nil {
		return nil, err
	}
	var listed []string
	if more {
		if listed, err = c.distinctNames(ctx, `SELECT DISTINCT b.library_id, json_extract(j.value, '$.name')
			  FROM books b, json_each(b.more_series) j WHERE b.more_series <> '[]'`+lib, args); err != nil {
			return nil, err
		}
	}
	out := map[string]seriesNames{}
	add := func(names []string, list func(*seriesNames) *[]string) {
		for _, name := range names {
			k := seriesKey(name)
			if name == "" || !want[k] {
				continue
			}
			n := out[k]
			if l := list(&n); !slices.Contains(*l, name) { // a spelling in several libraries
				*l = append(*l, name)
			}
			out[k] = n
		}
	}
	add(main, func(n *seriesNames) *[]string { return &n.main })
	add(listed, func(n *seriesNames) *[]string { return &n.more })
	return out, nil
}

// distinctNames runs q, selecting a library id and a name, for the names.
func (c *Catalog) distinctNames(ctx context.Context, q string, args []any) ([]string, error) {
	return queryRows(ctx, c.db, func(r *sql.Rows, name *string) error {
		var lib int64
		var n sql.NullString
		err := r.Scan(&lib, &n)
		*name = n.String
		return err
	}, q, args...)
}

// spellingsOf is spellings for one series.
func (c *Catalog) spellingsOf(ctx context.Context, libraries []int64, more bool, name string) (seriesNames, error) {
	m, err := c.spellings(ctx, libraries, more, name)
	return m[seriesKey(name)], err
}

// seriesFilter is the WHERE condition, and its args, for a series filter on the
// books row whose columns carry prefix: the books whose main series is one of
// n's spellings, and with memberships also those whose more_series names one.
// With memberships the full-text index (whose series column holds every series
// name, verbatim) finds the candidates by any spelling's phrase, and the exact
// test keeps the rest out; a series with no letter or digit has no phrase to
// match and is tested alone. Without, idx_books_series finds them.
func seriesFilter(prefix string, n seriesNames, memberships bool) (string, []any) {
	cond := prefix + "series IN (" + placeholders(len(n.main)) + ")"
	args := anyOf(n.main)
	if !memberships {
		return cond, args
	}
	cond = "(" + cond + " OR EXISTS (SELECT 1 FROM json_each(" + prefix +
		"more_series) j WHERE json_extract(j.value, '$.name') IN (" + placeholders(len(n.more)) + ")))"
	args = append(args, anyOf(n.more)...)
	all := slices.Concat(n.main, n.more)
	if len(all) == 0 {
		return "0", nil // no spelling in the index
	}
	phrases, ok := ftsPhrases(all)
	if !ok {
		return cond, args
	}
	return prefix + "id IN (SELECT rowid FROM books_fts WHERE series MATCH ?) AND " + cond,
		append([]any{phrases}, args...)
}

// anyOf is ss as query arguments.
func anyOf[T any](ss []T) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// positionIn is a book's position in the named series (0 when none, or when it
// isn't in it), the series matched by seriesKey: its first entry that is it.
func positionIn(list []SeriesRef, name string) float64 {
	for _, s := range list {
		if sameSeries(s.Name, name) {
			return s.Position
		}
	}
	return 0
}

// seriesOrderKeys is the series sort for a list filtered to the series name,
// spelled n: by the book's position in that series (unnumbered last), then title.
// The spellings are quoted literals in the expression, since a sort key binds no
// arguments (the only key built from a request value). The SQL takes a book's
// main series when it is one of n's, else its first more_series entry that is,
// as positionIn does for the cursor.
func seriesOrderKeys(name string, n seriesNames) []sortKey {
	pos := "(CASE WHEN b.series IN (" + sqlList(n.main) + ") THEN b.series_index ELSE COALESCE((SELECT json_extract(j.value, '$.position') " +
		"FROM json_each(b.more_series) j WHERE json_extract(j.value, '$.name') IN (" + sqlList(n.more) + ")), 0) END)"
	val := func(b AdminBook) float64 { return positionIn(b.SeriesList, name) }
	return []sortKey{
		lastWhen(pos+" = 0", func(b AdminBook) bool { return val(b) == 0 }),
		{expr: pos, num: true, val: func(b AdminBook) any { return val(b) }},
		keyTitle,
	}
}

// sqlQuote is s as an SQL string literal.
func sqlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// sqlList is ss as a list of SQL string literals (empty for none: SQLite takes
// "IN ()" as false).
func sqlList(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = sqlQuote(s)
	}
	return strings.Join(q, ", ")
}

// ftsSeries is the full-text index's series text: every series name, a line each.
func ftsSeries(series, more string) string {
	names := []string{}
	for _, s := range SeriesList(series, 0, more) {
		names = append(names, s.Name)
	}
	return strings.Join(names, "\n")
}
