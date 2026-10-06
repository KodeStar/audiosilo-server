package meta

import (
	"encoding/json"
	"reflect"
	"testing"
)

// named is rail(...) with a series name.
func named(name, at string, entries ...string) MetaSeries {
	r := rail(at, entries...)
	r.Name = name
	return r
}

// mine is one of the caller's books.
func mine(lib int64, path, series string, index float64, workID string) LocalBook {
	return LocalBook{MetaLocal: MetaLocal{LibraryID: lib, Path: path}, Series: series, SeriesIndex: index, WorkID: workID}
}

// locals reads each entry's local path ("" = none) off works.
func locals(works []MetaSeriesWork) []string {
	out := make([]string, len(works))
	for i, w := range works {
		if w.Local != nil {
			out[i] = w.Local.Path
		}
	}
	return out
}

var me = MetaLocal{LibraryID: 1, Path: "S/2"}

func TestPlaceLocalCurrentWork(t *testing.T) {
	rails := []MetaSeries{named("S", "2", "one@1", "two@2", "three@3")}
	books := []LocalBook{
		mine(1, "S/2", "S", 2, ""),
		mine(1, "S/2 copy", "S", 2, "two"), // another book of the current work
		mine(1, "S/2 index", "S", 2, ""),   // numbered like it
	}
	got := locals(PlaceLocal(rails, "two", me, books)[0].Works)
	if want := []string{"", "S/2", ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("locals = %q, want %q", got, want)
	}
}

func TestPlaceLocalRequestedNotByIndex(t *testing.T) {
	// The requested book is numbered 3 locally but is the work at 2: it holds
	// the current work's entry and nothing else.
	rails := []MetaSeries{named("S", "2", "two@2", "three@3")}
	got := locals(PlaceLocal(rails, "two", me, []LocalBook{mine(1, "S/2", "S", 3, "")})[0].Works)
	if want := []string{"S/2", ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("locals = %q, want %q", got, want)
	}
}

func TestPlaceLocalWorkIDBeatsIndex(t *testing.T) {
	rails := []MetaSeries{named("S", "1", "one@1", "two@2", "five@5")}
	books := []LocalBook{
		mine(1, "S/a", "S", 2, ""),     // numbered 2, work unknown
		mine(1, "S/b", "S", 5, "two"),  // numbered 5 locally, but known to be work "two"
		mine(1, "S/c", "S", 3, "nope"), // known to be a work not on the rail
	}
	got := locals(PlaceLocal(rails, "one", MetaLocal{LibraryID: 1, Path: "S/1"}, books)[0].Works)
	// S/b holds "two" by its work id and is NOT also placed at 5 by its index;
	// S/a loses entry 2 to it; S/c is placed nowhere.
	if want := []string{"S/1", "S/b", ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("locals = %q, want %q", got, want)
	}
}

func TestPlaceLocalIndexFallback(t *testing.T) {
	rails := []MetaSeries{named("S", "1", "one@1", "half@1.5", "two@2", "omni@1-3", "blank@")}
	books := []LocalBook{mine(1, "S/1.5", "S", 1.5, ""), mine(1, "S/2", "S", 2, ""), mine(1, "S/0", "S", 0, "")}
	got := locals(PlaceLocal(rails, "", MetaLocal{}, books)[0].Works)
	if want := []string{"", "S/1.5", "S/2", "", ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("locals = %q, want %q", got, want)
	}
}

func TestPlaceLocalOnePerEntryTieOrder(t *testing.T) {
	rails := []MetaSeries{named("S", "1", "one@1", "two@2", "three@3")}
	// In preference order as SeriesBooks returns them: library 2 sorts first.
	books := []LocalBook{
		mine(2, "A/3", "S", 3, ""),
		mine(2, "B/3", "S", 3, ""),
		mine(2, "A/2", "S", 2, ""),
		mine(1, "Z/2", "S", 2, ""),
		mine(2, "W/x", "S", 9, "three"),
		mine(1, "Y/x", "S", 9, "three"),
	}
	out := PlaceLocal(rails, "", MetaLocal{LibraryID: 1, Path: "elsewhere"}, books)[0].Works
	// Entry 2: the requested book's library (1) beats library sort order.
	// Entry 3: by work id, library 1 again. One book per entry throughout.
	if got, want := locals(out), []string{"", "Z/2", "Y/x"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("locals = %q, want %q", got, want)
	}
	if out[1].Local.LibraryID != 1 || out[2].Local.LibraryID != 1 {
		t.Fatalf("libraries = %+v %+v", out[1].Local, out[2].Local)
	}
	// Without a preferred library, input order (library sort order, then path)
	// decides.
	out = PlaceLocal(rails, "", MetaLocal{LibraryID: 7}, books)[0].Works
	if got, want := locals(out), []string{"", "A/2", "W/x"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("locals = %q, want %q", got, want)
	}
	out = PlaceLocal(rails, "", MetaLocal{LibraryID: 7}, books[:2])[0].Works
	if got, want := locals(out), []string{"", "", "A/3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("locals = %q, want %q", got, want)
	}
}

func TestPlaceLocalFoldedSeriesNames(t *testing.T) {
	rails := []MetaSeries{named("The Expanse", "1", "one@1", "two@2", "three@3")}
	books := []LocalBook{
		mine(1, "E/2", "the  expanse!", 2, ""),
		mine(1, "E/3", "Thé Éxpanse", 3, ""),
		mine(1, "O/1", "Expanse Origins", 1, ""), // another series numbered 1
	}
	got := locals(PlaceLocal(rails, "", MetaLocal{}, books)[0].Works)
	if want := []string{"", "E/2", "E/3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("locals = %q, want %q", got, want)
	}
}

func TestPlaceLocalRailsByName(t *testing.T) {
	// Two rails: a book is placed by index only on the rail named like its series.
	rails := []MetaSeries{named("Discworld", "1", "a@1", "b@2"), named("Witches", "1", "c@1", "d@2")}
	out := PlaceLocal(rails, "", MetaLocal{}, []LocalBook{mine(1, "D/2", "Discworld", 2, ""), mine(1, "W/1", "Witches", 1, "")})
	if got, want := locals(out[0].Works), []string{"", "D/2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("discworld = %q, want %q", got, want)
	}
	if got, want := locals(out[1].Works), []string{"W/1", ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("witches = %q, want %q", got, want)
	}
}

func TestPlaceLocalOrderings(t *testing.T) {
	r := named("Narnia", "1", "lww@1", "pc@2", "mn@6")
	r.Orderings = []MetaSeriesOrdering{
		{ID: "chrono", Name: "Narnia (Chronological)", Position: "2", Works: []MetaSeriesWork{
			{ID: "mn", Position: "1"}, {ID: "lww", Position: "2"}, {ID: "hb", Position: "3"}, {ID: "pc", Position: "4"},
		}},
	}
	books := []LocalBook{
		mine(1, "N/6", "Narnia", 6, ""),                    // by index on the main view (mn)
		mine(1, "N/2", "Narnia", 2, ""),                    // by index on the main view (pc)
		mine(1, "C/3", "Narnia (Chronological)", 3, ""),    // by index on the chronological view only
		mine(1, "N/4", "Narnia", 4, ""),                    // numbered 4: no main entry, and not chrono-named
		mine(1, "C/x", "Narnia (Chronological)", 0, "lww"), // a copy of the current work
	}
	out := PlaceLocal([]MetaSeries{r}, "lww", MetaLocal{LibraryID: 1, Path: "N/1"}, books)
	if got, want := locals(out[0].Works), []string{"N/1", "N/2", "N/6"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("main = %q, want %q", got, want)
	}
	// The ordering follows the main view by work (mn = N/6, pc = N/2, never by
	// their chronological positions), and places by index only a book of a
	// series named like the ordering.
	if got, want := locals(out[0].Orderings[0].Works), []string{"N/6", "N/1", "C/3", "N/2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("chronological = %q, want %q", got, want)
	}
}

func TestPlaceLocalDoesNotMutate(t *testing.T) {
	r := named("S", "1", "one@1", "two@2")
	r.Orderings = []MetaSeriesOrdering{{ID: "o", Name: "S", Works: []MetaSeriesWork{{ID: "two", Position: "1"}}}}
	rails := []MetaSeries{r}
	before, _ := json.Marshal(rails)
	books := []LocalBook{mine(2, "S/2", "S", 2, ""), mine(1, "S/1b", "S", 1, "")}
	booksBefore := append([]LocalBook(nil), books...)

	out := PlaceLocal(rails, "one", MetaLocal{LibraryID: 1, Path: "S/1"}, books)
	if after, _ := json.Marshal(rails); string(after) != string(before) {
		t.Fatalf("rails modified:\n%s\n%s", before, after)
	}
	if !reflect.DeepEqual(books, booksBefore) {
		t.Fatalf("books reordered: %+v", books)
	}
	if out[0].Works[1].Local == nil || out[0].Orderings[0].Works[0].Local == nil {
		t.Fatalf("copy not annotated: %+v", out)
	}
	if PlaceLocal(nil, "one", me, books) != nil {
		t.Fatal("no rails should stay nil")
	}
}

func TestNextOnRail(t *testing.T) {
	for name, tc := range map[string]struct {
		rail   MetaSeries
		want   string // "" = the last
		wantOK bool
	}{
		"next by position":       {rail("2", "c@3", "a@1", "cur@2", "d@4"), "c", true},
		"a novella counts":       {rail("1", "a@1", "b@2", "half@1.5"), "half", true},
		"unnumbered skipped":     {rail("1", "cur@1", "omni@1-3", "x@", "b@2"), "b", true},
		"last":                   {rail("3", "a@1", "b@2", "cur@3"), "", true},
		"ties keep rail order":   {rail("1", "cur@1", "b@2", "b2@2"), "b", true},
		"own entry when blank":   {rail("", "a@1", "cur@2", "c@3"), "c", true},
		"unreadable position":    {rail("1-3", "cur@1-3", "b@4"), "", false},
		"current work skipped":   {rail("1", "cur@1", "cur@2", "b@3"), "b", true},
		"equal position skipped": {rail("2", "a@2", "cur@2", "b@3"), "b", true},
	} {
		t.Run(name, func(t *testing.T) {
			next, ok := NextOnRail(tc.rail, "cur")
			got := ""
			if next != nil {
				got = next.ID
			}
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("NextOnRail = %q, %v; want %q, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestSeriesNames(t *testing.T) {
	r := named("Main", "1")
	r.Orderings = []MetaSeriesOrdering{{Name: "Chronological"}}
	got := SeriesNames([]MetaSeries{r, named("Other", "2")})
	if want := []string{"Main", "Chronological", "Other"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("SeriesNames = %q, want %q", got, want)
	}
}

// TestPlaceOwned: a book the cache knows is placed by its work (here a novella
// numbered like volume 2 holds no slot), the rest by series index; the books
// given are not modified.
func TestPlaceOwned(t *testing.T) {
	svc := NewService("http://meta.invalid", nil)
	cachePut(svc.cache, nsASIN.key("B0NOVELLA"), &Enrichment{Matched: true, Work: &MetaWork{ID: "novella"}}, positiveTTL)
	env := &Enrichment{Work: &MetaWork{ID: "one"}, Series: []MetaSeries{named("S", "1", "one@1", "two@2", "novella@2.5")}}
	books := []LocalBook{
		{MetaLocal: MetaLocal{LibraryID: 1, Path: "S/novella"}, Series: "S", SeriesIndex: 2, ASIN: "B0NOVELLA"},
		{MetaLocal: MetaLocal{LibraryID: 1, Path: "S/2"}, Series: "S", SeriesIndex: 2},
	}
	got := locals(svc.PlaceOwned(env, MetaLocal{LibraryID: 1, Path: "S/1"}, books)[0].Works)
	if want := []string{"S/1", "S/2", "S/novella"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("placed = %q, want %q", got, want)
	}
	if books[0].WorkID != "" || env.Series[0].Works[0].Local != nil {
		t.Fatal("PlaceOwned modified its input")
	}
}
