package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
		if !slices.ContainsFunc(out, func(o SeriesRef) bool { return match.SameSeries(o.Name, s.Name) }) {
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

// otherSeries is the series a book is in besides its main one (SeriesList less
// the main series): what book_more_series holds for it.
func otherSeries(series, more string) []SeriesRef {
	all := SeriesList(series, 0, more)
	if series != "" {
		all = all[1:]
	}
	return all
}

// keyOtherSeries writes book_more_series for a book: its other series by key,
// with its position in each. A series filter, ordering or next-book query finds
// a book in a series through its list by that table's index, whatever the
// spelling (refreshEffective writes books.series_key beside it).
func keyOtherSeries(ctx context.Context, tx *sql.Tx, bookID, libID int64, series, more string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM book_more_series WHERE book_id = ?`, bookID); err != nil {
		return err
	}
	return insertOtherSeries(ctx, tx, bookID, libID, series, more)
}

// insertOtherSeries adds a book's other series to book_more_series.
func insertOtherSeries(ctx context.Context, tx *sql.Tx, bookID, libID int64, series, more string) error {
	for _, s := range otherSeries(series, more) {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO book_more_series(book_id, library_id, series_key, position) VALUES(?, ?, ?, ?)`,
			bookID, libID, seriesKey(s.Name), s.Position); err != nil {
			return err
		}
	}
	return nil
}

// seriesFilter is the WHERE condition, and its args, for a filter on the series
// named name (any spelling: its seriesKey) on the books row whose columns carry
// prefix: the books whose main series it is (idx_books_series_key), and with
// memberships also those whose list names it (book_more_series), each found by
// its index in every library (the caller's library term narrows them).
func seriesFilter(prefix, name string, memberships bool) (string, []any) {
	key := seriesKey(name)
	main := `SELECT s.id FROM books s WHERE s.library_id IN (SELECT id FROM libraries) AND s.series_key = ?`
	if !memberships {
		return prefix + "id IN (" + main + ")", []any{key}
	}
	return prefix + "id IN (" + main + ` UNION ALL SELECT m.book_id FROM book_more_series m WHERE m.series_key = ?)`,
		[]any{key, key}
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
		if match.SameSeries(s.Name, name) {
			return s.Position
		}
	}
	return 0
}

// seriesOrderKeys is the series sort for a list filtered to the series name:
// by the book's position in that series (unnumbered last), then title. The key is
// a quoted literal in the expression, since a sort key binds no arguments (the
// only key built from a request value). The SQL takes a book's main series when
// it is that series, else its book_more_series row for it, as positionIn does for
// the cursor.
func seriesOrderKeys(name string) []sortKey {
	key := sqlQuote(seriesKey(name))
	pos := "(CASE WHEN b.series_key = " + key + " THEN b.series_index ELSE COALESCE((SELECT m.position " +
		"FROM book_more_series m WHERE m.book_id = b.id AND m.series_key = " + key + "), 0) END)"
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
	for _, s := range SeriesList(series, 0, more) {
		names = append(names, s.Name)
	}
	return strings.Join(names, "\n")
}

// seriesKeysKey is the server_state row saying KeySeries has run.
const seriesKeysKey = "series_keys"

// KeySeries keys, once per database, the books indexed before series keys were
// stored (migration 0041 added books.series_key and book_more_series empty):
// every book's main series key and other series, written as refreshEffective
// writes them. It first puts every stored more_series list through
// normalizeMoreSeries, since a list written before a series became one by its key
// can name one series twice, in two spellings, and rebuilds those books whole.
// One transaction, before the server answers (the series filters read only the
// keys), about a second per 50,000 books. It returns how many books it keyed;
// 0 once it has run (server_state seriesKeysKey).
func (c *Catalog) KeySeries(ctx context.Context) (int, error) {
	if done, err := getServerState[bool](ctx, c.db, seriesKeysKey); err != nil || done {
		return 0, err
	}
	keyed := 0
	err := c.db.WithTx(ctx, "KeySeries", func(tx *sql.Tx) error {
		type list struct {
			lib         int64
			path, value string
		}
		lists, err := queryRows(ctx, tx, func(r *sql.Rows, l *list) error { return r.Scan(&l.lib, &l.path, &l.value) },
			`SELECT library_id, path, value FROM book_overrides WHERE field = ?`, FieldMoreSeries)
		if err != nil {
			return err
		}
		var relisted []int64
		for _, l := range lists {
			v, err := normalizeMoreSeries(l.value)
			if err != nil || v == l.value {
				continue // a list it can't read stays as stored (it names no series)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE book_overrides SET value = ? WHERE library_id = ? AND path = ? AND field = ?`,
				v, l.lib, l.path, FieldMoreSeries); err != nil {
				return err
			}
			switch id, err := bookIDByPath(ctx, tx, l.lib, l.path); {
			case err == nil:
				relisted = append(relisted, id)
			case !errors.Is(err, ErrNotFound): // an edit kept for a book not indexed now
				return err
			}
		}
		type book struct {
			id, lib      int64
			series, more string
		}
		books, err := queryRows(ctx, tx, func(r *sql.Rows, b *book) error { return r.Scan(&b.id, &b.lib, &b.series, &b.more) },
			`SELECT id, library_id, series, more_series FROM books`)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM book_more_series`); err != nil {
			return err
		}
		setKey, err := tx.PrepareContext(ctx, `UPDATE books SET series_key = ? WHERE id = ?`)
		if err != nil {
			return err
		}
		defer setKey.Close()
		for _, b := range books {
			if b.series != "" {
				if _, err := setKey.ExecContext(ctx, seriesKey(b.series), b.id); err != nil {
					return err
				}
			}
			if err := insertOtherSeries(ctx, tx, b.id, b.lib, b.series, b.more); err != nil {
				return err
			}
		}
		for _, id := range relisted {
			if err := refreshEffective(ctx, tx, id); err != nil {
				return err
			}
		}
		keyed = len(books)
		return putServerState(ctx, tx, seriesKeysKey, true, c.stamp())
	})
	return keyed, err
}
