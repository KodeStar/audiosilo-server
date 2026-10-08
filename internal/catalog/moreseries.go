package catalog

import (
	"encoding/json"
	"strings"
)

// A book's main series is the series / series_index pair every client knows. A
// book can belong to more (Guards! Guards! is Discworld #8 and Discworld: City
// Watch #1): those are the more_series field, a list of the other series with the
// book's position in each, which only an edit or a community match supplies (tags
// name one series). It is an override field like any other - provenance, revert,
// undo and clearing community values work on it unchanged - whose value is the
// list as canonical JSON ("" for none).

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
// series field, blank names dropped, each name once (the first), positions from 0
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
		if s.Name == "" || seen[s.Name] {
			continue
		}
		seen[s.Name] = true
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

// seriesList is every series a book belongs to: the main one first (when it has
// one), then the others, each name once.
func seriesList(series string, index float64, more string) []SeriesRef {
	out := []SeriesRef{}
	if series != "" {
		out = append(out, SeriesRef{Name: series, Position: index})
	}
	for _, s := range ParseMoreSeries(more) {
		if s.Name != series {
			out = append(out, s)
		}
	}
	return out
}

// seriesFilter is the WHERE condition, and its args, for a series= filter on the
// books row whose columns carry prefix: the books whose main series is value or
// whose more_series names it. The full-text index (whose series column holds
// every series name) finds the candidates; the exact test keeps the rest out. A
// value with no letter or digit has no phrase to match and is tested alone.
func seriesFilter(prefix, value string) (string, []any) {
	exact := "(" + prefix + "series = ? OR EXISTS (SELECT 1 FROM json_each(" + prefix +
		"more_series) j WHERE json_extract(j.value, '$.name') = ?))"
	phrase, ok := ftsPhrase(value)
	if !ok {
		return exact, []any{value, value}
	}
	return prefix + "id IN (SELECT rowid FROM books_fts WHERE series MATCH ?) AND " + exact,
		[]any{phrase, value, value}
}

// positionIn is a book's position in the named series (0 when none, or when it
// isn't in it).
func positionIn(list []SeriesRef, name string) float64 {
	for _, s := range list {
		if s.Name == name {
			return s.Position
		}
	}
	return 0
}

// seriesOrderKeys is the series sort for a list filtered to one series: by the
// book's position in that series (unnumbered last), then title. The series name
// is a quoted literal in the expression, since a sort key binds no arguments
// (the only key built from a request value).
func seriesOrderKeys(name string) []sortKey {
	lit := sqlQuote(name)
	pos := "(CASE WHEN b.series = " + lit + " THEN b.series_index ELSE COALESCE((SELECT json_extract(j.value, '$.position') " +
		"FROM json_each(b.more_series) j WHERE json_extract(j.value, '$.name') = " + lit + "), 0) END)"
	val := func(b AdminBook) float64 { return positionIn(b.SeriesList, name) }
	return []sortKey{
		lastWhen(pos+" = 0", func(b AdminBook) bool { return val(b) == 0 }),
		{expr: pos, num: true, val: func(b AdminBook) any { return val(b) }},
		keyTitle,
	}
}

// sqlQuote is s as an SQL string literal.
func sqlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// ftsSeries is the full-text index's series text: every series name, a line each.
func ftsSeries(series, more string) string {
	names := []string{}
	for _, s := range seriesList(series, 0, more) {
		names = append(names, s.Name)
	}
	return strings.Join(names, "\n")
}
