package catalog

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// A series position may be a range (an omnibus of books 1-3): the series_index
// field holds "1-3", the row its start (series_index) and end (series_index_end).

func TestNormalizeSeriesRange(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"1-3": "1-3", " 1 - 3 ": "1-3", "1–3": "1-3", "2.50-4.0": "2.5-4", "1-101": "1-101",
	} {
		if got, err := normalizeOverride(FieldSeriesIndex, in); err != nil || got != want {
			t.Errorf("normalizeOverride(series_index, %q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"3-1", "3-3", "0-2", "1-102", "99999-100001", "1-3-5", "one-three"} {
		_, err := normalizeOverride(FieldSeriesIndex, in)
		if oe, ok := err.(*OverrideError); !ok || !strings.Contains(oe.Reason, "a range like 1-3") {
			t.Errorf("normalizeOverride(series_index, %q) = %v, want refused", in, err)
		}
	}
	if got, err := normalizeMoreSeries(`[{"name":"Box","position":1,"position_end":3},{"name":"Other","position":2,"position_end":0}]`); err != nil ||
		got != `[{"name":"Box","position":1,"position_end":3},{"name":"Other","position":2}]` {
		t.Errorf("more_series with a range = %s, %v", got, err)
	}
	for _, bad := range []string{
		`[{"name":"Box","position":3,"position_end":1}]`, `[{"name":"Box","position":0,"position_end":3}]`,
		`[{"name":"Box","position":1,"position_end":500}]`, `[{"name":"Box","position":1,"position_end":"3"}]`,
	} {
		if _, err := normalizeMoreSeries(bad); err == nil {
			t.Errorf("more_series %s accepted, want refused", bad)
		}
	}
	for _, tc := range []struct {
		in         string
		start, end float64
	}{{"1-3", 1, 3}, {"2", 2, 0}, {"", 0, 0}, {"x", 0, 0}} {
		if start, end := ParseSeriesPosition(tc.in); start != tc.start || end != tc.end {
			t.Errorf("ParseSeriesPosition(%q) = %v, %v; want %v, %v", tc.in, start, end, tc.start, tc.end)
		}
	}
	if got := FormatSeriesRange(1, 3); got != "1-3" {
		t.Errorf("FormatSeriesRange(1, 3) = %q", got)
	}
	if got := FormatSeriesRange(2, 0); got != "2" {
		t.Errorf("FormatSeriesRange(2, 0) = %q", got)
	}
}

// TestSeriesRangeEditAndRevert: an edit to a range is the row's start and end, on
// the player's JSON as series_index (the start) and series_index_end; reverting
// restores the scanned single position, and series_index_end leaves the JSON. A
// tag's range (the scan's) reads as the tag's.
func TestSeriesRangeEditAndRevert(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Shelf", Root: "/tmp/s"})
	for _, b := range []*Book{
		{RelPath: "Saga/Omnibus", Title: "Omnibus", Series: "Saga", SeriesIndex: 1},
		{RelPath: "Saga/Tagged", Title: "Tagged", Series: "Saga", SeriesIndex: 4, SeriesIndexEnd: 6},
	} {
		b.LibraryID, b.Format = lib.ID, "m4b"
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	playerJSON := func(path string) string {
		t.Helper()
		raw, err := json.Marshal(mustBook(t, c, ctx, lib.ID, path))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	if err := c.EditBook(ctx, lib.ID, "Saga/Omnibus", BookEdit{Set: map[string]string{FieldSeriesIndex: "1 – 3"}}); err != nil {
		t.Fatal(err)
	}
	b := mustBook(t, c, ctx, lib.ID, "Saga/Omnibus")
	if b.SeriesIndex != 1 || b.SeriesIndexEnd != 3 {
		t.Fatalf("edited = %v-%v, want 1-3", b.SeriesIndex, b.SeriesIndexEnd)
	}
	if got := playerJSON("Saga/Omnibus"); !strings.Contains(got, `"series_index":1,"series_index_end":3,`) {
		t.Errorf("player JSON = %s, want series_index 1 and series_index_end 3", got)
	}
	d, err := c.AdminBookDetail(ctx, lib.ID, "Saga/Omnibus")
	if err != nil {
		t.Fatal(err)
	}
	if f := d.Fields[FieldSeriesIndex]; f.Value != "1-3" || f.Source != SourceEdited || f.Scanned != "1" {
		t.Errorf("series_index field = %+v, want the edited 1-3 over the scanned 1", f)
	}
	if d.Book.SeriesIndexEnd != 3 || !reflect.DeepEqual(d.Book.SeriesList, []SeriesRef{{Name: "Saga", Position: 1, PositionEnd: 3}}) {
		t.Errorf("admin book = %v-%v %+v", d.Book.SeriesIndex, d.Book.SeriesIndexEnd, d.Book.SeriesList)
	}
	if err := c.EditBook(ctx, lib.ID, "Saga/Omnibus", BookEdit{Revert: []string{FieldSeriesIndex}}); err != nil {
		t.Fatal(err)
	}
	if b := mustBook(t, c, ctx, lib.ID, "Saga/Omnibus"); b.SeriesIndex != 1 || b.SeriesIndexEnd != 0 {
		t.Errorf("reverted = %v-%v, want the scanned 1", b.SeriesIndex, b.SeriesIndexEnd)
	}
	if got := playerJSON("Saga/Omnibus"); strings.Contains(got, "series_index_end") || strings.Contains(got, "position_end") {
		t.Errorf("reverted player JSON = %s, want no range field", got)
	}
	d, _ = c.AdminBookDetail(ctx, lib.ID, "Saga/Tagged")
	if f := d.Fields[FieldSeriesIndex]; f.Value != "4-6" || f.Source != SourceTag || f.Locked {
		t.Errorf("tagged range = %+v, want the tag's 4-6", f)
	}
	// A rescan keeps the edit's range (an edit is a lock).
	if err := c.EditBook(ctx, lib.ID, "Saga/Omnibus", BookEdit{Set: map[string]string{FieldSeriesIndex: "1-3"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "Saga/Omnibus", Title: "Omnibus", Series: "Saga", SeriesIndex: 1, Format: "m4b"}); err != nil {
		t.Fatal(err)
	}
	if b := mustBook(t, c, ctx, lib.ID, "Saga/Omnibus"); b.SeriesIndex != 1 || b.SeriesIndexEnd != 3 {
		t.Errorf("after a rescan = %v-%v, want 1-3", b.SeriesIndex, b.SeriesIndexEnd)
	}
	// The aggregate holds every position inside a range; the export carries it.
	series, err := c.Series(ctx, lib.ID, nil, true)
	if err != nil || len(series) != 1 || !reflect.DeepEqual(series[0].Positions, []float64{1, 2, 3, 4, 5, 6}) {
		t.Errorf("series = %+v, %v; want positions 1 to 6", series, err)
	}
	exp, err := c.ExportLibraryBooks(ctx, lib.ID, "dev")
	if err != nil {
		t.Fatal(err)
	}
	var positions []string
	for _, b := range exp.Books {
		positions = append(positions, b.SeriesPosition)
	}
	if !reflect.DeepEqual(positions, []string{"1-3", "4-6"}) {
		t.Errorf("export positions = %q, want 1-3 and 4-6", positions)
	}
}

func TestHeldPositions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		s    SeriesRef
		want []float64
	}{
		{SeriesRef{Position: 0}, nil},
		{SeriesRef{Position: 2}, []float64{2}},
		{SeriesRef{Position: 1, PositionEnd: 3}, []float64{1, 2, 3}},
		{SeriesRef{Position: 2.5, PositionEnd: 4}, []float64{2.5, 3, 4}},
		{SeriesRef{Position: 1, PositionEnd: 2.5}, []float64{1, 2, 2.5}},
	} {
		if got := heldPositions(tc.s); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("heldPositions(%+v) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

// TestSeriesSwapCarriesRange: making a listed series the main one carries a
// range across the swap both ways, the main series' range into the list and a
// listed range into series_index.
func TestSeriesSwapCarriesRange(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Shelf", Root: "/tmp/s"})
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "box", Title: "Box", Series: "Saga", SeriesIndex: 1, Format: "m4b"}); err != nil {
		t.Fatal(err)
	}
	edit := func(set map[string]string) {
		t.Helper()
		if err := c.EditBook(ctx, lib.ID, "box", BookEdit{Set: set}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(series, idx, more string) {
		t.Helper()
		d, err := c.AdminBookDetail(ctx, lib.ID, "box")
		if err != nil {
			t.Fatal(err)
		}
		f := d.Fields
		if f[FieldSeries].Value != series || f[FieldSeriesIndex].Value != idx || f[FieldMoreSeries].Value != more {
			t.Errorf("= %q #%q more %s, want %q #%q more %s",
				f[FieldSeries].Value, f[FieldSeriesIndex].Value, f[FieldMoreSeries].Value, series, idx, more)
		}
	}
	edit(map[string]string{FieldSeriesIndex: "1-3", FieldMoreSeries: `[{"name":"Collected","position":5}]`})
	edit(map[string]string{FieldSeries: "Collected"})
	check("Collected", "5", `[{"name":"Saga","position":1,"position_end":3}]`)
	edit(map[string]string{FieldSeries: "Saga"})
	check("Saga", "1-3", `[{"name":"Collected","position":5}]`)
	b := mustBook(t, c, ctx, lib.ID, "box")
	if want := []SeriesRef{{Name: "Saga", Position: 1, PositionEnd: 3}, {Name: "Collected", Position: 5}}; b.SeriesIndexEnd != 3 || !reflect.DeepEqual(b.SeriesList, want) {
		t.Errorf("player book = %v-%v %+v, want %+v", b.SeriesIndex, b.SeriesIndexEnd, b.SeriesList, want)
	}
}

// TestNextInSeriesRanges: a book at a range is followed by the first book after
// its end; a book inside it, or at its start, is followed by the next single book
// (the omnibus never takes another's place); a series a book lists at a range
// counts from its end too; and a book inside an omnibus in a series ranked above
// steps back.
func TestNextInSeriesRanges(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Shelf", Root: "/tmp/s"})
	add := func(path, series string, idx float64, edit map[string]string) {
		t.Helper()
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: path, Title: path, Series: series, SeriesIndex: idx, Format: "m4b"}); err != nil {
			t.Fatal(err)
		}
		if edit != nil {
			if err := c.EditBook(ctx, lib.ID, path, BookEdit{Set: edit}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// "S/0 omnibus" sorts before "S/1" by path: the order puts the single first.
	add("S/0 omnibus", "S", 1, map[string]string{FieldSeriesIndex: "1-3"})
	add("S/0.5", "S", 0.5, nil)
	add("S/1", "S", 1, nil)
	add("S/2", "S", 2, nil)
	add("S/3", "S", 3, nil)
	add("S/4", "S", 4, nil)
	add("L/listed", "Other", 1, map[string]string{FieldMoreSeries: `[{"name":"S","position":1,"position_end":3}]`})
	// Narnia: the omnibus (chronological 1-3, publication 5) is followed by
	// nothing in Narnia; in its publication order #6 is chronological 2, inside
	// the omnibus, so it steps back, and #7 answers.
	add("N/omnibus", "Narnia", 1, map[string]string{FieldSeriesIndex: "1-3", FieldMoreSeries: `[{"name":"Pub","position":5}]`})
	add("N/inside", "Narnia", 2, map[string]string{FieldMoreSeries: `[{"name":"Pub","position":6}]`})
	add("N/after", "Pub", 7, nil)
	all := Scope{LibraryID: lib.ID, AllowAll: true}
	next := func(path string) string {
		t.Helper()
		b := mustBook(t, c, ctx, lib.ID, path)
		n, _, err := c.NextInSeries(ctx, lib.ID, b, all)
		if err != nil {
			t.Fatal(err)
		}
		if n == nil {
			return ""
		}
		return n.RelPath
	}
	for path, want := range map[string]string{
		"S/0 omnibus": "S/4", // after the range's end
		"S/0.5":       "S/1", // the single at 1 before the omnibus starting there
		"S/1":         "S/2",
		"S/2":         "S/3", // the omnibus (from 1) is no step forward from 2
		"L/listed":    "S/4", // Other has nothing else; S from the listed range's end
		"N/omnibus":   "N/after",
	} {
		if got := next(path); got != want {
			t.Errorf("next(%s) = %q, want %q", path, got, want)
		}
	}
}
