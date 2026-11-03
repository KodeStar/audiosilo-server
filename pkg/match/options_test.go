package match

import "testing"

// optCase is one query judged twice: under Best's defaults (want stays what it
// was before Options existed) and under ImportOptions (wantOpt).
type optCase struct {
	name          string
	q             Query
	want, wantOpt int // -1 = no match
}

func runOptCases(t *testing.T, books []Book, cases []optCase) {
	t.Helper()
	// One Index per rule set, shared by every case: a prepared candidate list
	// answers each query exactly as the one-shot call does, whatever an earlier
	// query left prepared.
	ix, ixOpt := NewIndex(books), NewIndexWith(books, ImportOptions())
	for _, tc := range cases {
		got, _ := Best(books, tc.q)
		if got != tc.want {
			t.Errorf("%s: default got %d, want %d", tc.name, got, tc.want)
		}
		if ixGot, _ := ix.Best(tc.q); ixGot != got {
			t.Errorf("%s: default Index got %d, Best %d", tc.name, ixGot, got)
		}
		got, score, ok := BestWith(books, tc.q, ImportOptions())
		if ixGot, ixScore, ixOK := ixOpt.BestScore(tc.q); ixGot != got || ixScore != score || ixOK != ok {
			t.Errorf("%s: options Index got %d, %v, %v; BestWith %d, %v, %v", tc.name, ixGot, ixScore, ixOK, got, score, ok)
		}
		if !ok {
			if score != 0 {
				t.Errorf("%s: no match scored %v", tc.name, score)
			}
		} else if score < threshold {
			t.Errorf("%s: match scored %v, under the threshold", tc.name, score)
		}
		if got != tc.wantOpt {
			t.Errorf("%s: with options got %d, want %d", tc.name, got, tc.wantOpt)
		}
	}
}

// aud is an Audible record: its title, author, series and position.
func aud(title, author, series string, seq float64) Query {
	return Query{Title: title, TitleShort: title, Author: author, Series: series, Sequence: seq, HasSequence: seq > 0}
}

// Options.SeriesMarker: Audible lists every volume of a series under the bare
// series name; the library's titles are wordy with no position column, but name
// the volume right after the series. (Synthetic, shaped like a real library.)
func TestOptionsSeriesMarker(t *testing.T) {
	const who = "Kirill Klevanski"
	books := []Book{
		0: {Author: who, Series: "Dragon Heart", Title: "Dragon Heart - Book 1: Stone Will"},
		1: {Author: who, Series: "Dragon Heart", Title: "Iron Will: Dragon Heart Series, Book 2 (Unabridged)"},
		2: {Author: who, Series: "Dragon Heart", Title: "Dragon Heart: Blood Will: Dragon Heart Series, Book 3 (Unabridged)"},
		3: {Author: who, Series: "Dragon Heart", Title: "Sea of Sand: Dragon Heart: Book 4 (Unabridged)"},
		4: {Author: "Selkie Myth", Series: "Beneath the Dragoneye Moons", Title: "Oathbound Healer - Beneath the Dragoneye Moons, Book 1"},
		5: {Author: "Shirtaloon", Series: "He Who Fights With Monsters", Title: "He Who Fights with Monsters: A LitRPG Adventure: He Who Fights with Monsters, Book 1"},
		// A "Book N" that does not follow the series name is no marker.
		6: {Author: "Ann Other", Series: "Moons", Title: "The Moons of Book 2 Island"},
		// A range is an omnibus, and a number running into a word is none.
		7: {Author: "Ann Other", Series: "Saga", Title: "Omnibus: Saga, Book 1-3"},
		8: {Author: "Ann Other", Series: "Saga", Title: "Side Story: Saga, Book 2.5x"},
	}
	runOptCases(t, books, []optCase{
		{"bare series title, marker after series", aud("Dragon Heart", who, "Dragon Heart Series", 2), -1, 1},
		{"marker after series, title first", aud("Dragon Heart", who, "Dragon Heart Series", 4), -1, 3},
		{"marker with a dash", aud("Dragon Heart", who, "Dragon Heart Series", 1), -1, 0},
		{"series name twice in the title", aud("Dragon Heart", who, "Dragon Heart Series", 3), -1, 2},
		{"no volume holds the number", aud("Dragon Heart", who, "Dragon Heart Series", 9), -1, -1},
		{"bare numbered query", aud("Beneath the Dragoneye Moons 2", "Selkie Myth", "Beneath the Dragoneye Moons", 2), -1, -1},
		{"volume 1 of a wordy series", aud("Beneath the Dragoneye Moons", "Selkie Myth", "Beneath the Dragoneye Moons", 1), -1, 4},
		{"genre tail is not the last segment", aud("He Who Fights with Monsters: A LitRPG Adventure", "Shirtaloon", "He Who Fights with Monsters", 1), -1, 5},
		{"Book N not after the series", aud("Moons", "Ann Other", "Moons", 2), -1, -1},
		{"a range marker", aud("Saga", "Ann Other", "Saga", 1), -1, -1},
		{"a number running into a word", aud("Saga", "Ann Other", "Saga", 2), -1, -1},
	})
	// Confirm-grade only: a marker never vetoes a candidate the column agrees on.
	withCol := []Book{{Author: who, Series: "Dragon Heart", SeriesIndex: 6, Title: "Land of Magic: Dragon Heart, Book 5"}}
	if i, _, ok := BestWith(withCol, aud("Dragon Heart", who, "Dragon Heart Series", 6), ImportOptions()); !ok || i != 0 {
		t.Errorf("the column's agreement lost to the title's marker: %d %v", i, ok)
	}
}

// Options.ExactTitle: the same title by the same person is the book, across
// differently named series, unless a side is just its series name.
func TestOptionsExactTitle(t *testing.T) {
	books := []Book{
		0: {Author: "Aleron Kong", Series: "Labyrinth World", Title: "God's Eye: Awakening: A Labyrinth World LitRPG Novel"},
		1: {Author: "George Orwell", Title: "1984 (Unabridged)"},
		2: {Author: "Sarah Turner", Title: "The Unmumsy Mum"},
		3: {Author: "Sarah Turner", Title: "The Unmumsy Mum Diary"},
		4: {Author: "Kirill Klevanski", Series: "Dragon Heart", SeriesIndex: 3, Title: "Dragon Heart"},
		5: {Author: "Ann Other", Series: "Saga", Title: "Mono"},
	}
	runOptCases(t, books, []optCase{
		{"identical title, series differs", aud("God's Eye: Awakening: A Labyrinth World LitRPG Novel", "Aleron Kong", "God's Eye", 1), -1, 0},
		{"numeric-only title", aud("1984", "George Orwell", "", 0), -1, 1},
		{"exact beats a covered title", aud("The Unmumsy Mum Diary", "Sarah Turner", "", 0), 2, 3},
		{"bare series-name title is not exact evidence", aud("Dragon Heart", "Kirill Klevanski", "Dragon Heart Series", 5), -1, -1},
		{"another person's same title", aud("1984", "Someone Else", "", 0), -1, -1},
	})
	if _, score, _ := BestWith(books, aud("1984", "George Orwell", "", 0), ImportOptions()); score != scoreExactTitle {
		t.Errorf("exact title scored %v, want %v", score, scoreExactTitle)
	}
	// The exact title is a bonus, never the whole score: same-titled candidates
	// (the key drops a bracketed group; one author's title in two series) are
	// still told apart by their series and number.
	twins := []Book{
		0: {Author: "Lindsay Buroker", Series: "The Emperor's Edge", SeriesIndex: 6, Title: "Forged in Blood (Part 1)"},
		1: {Author: "Lindsay Buroker", Series: "The Emperor's Edge", SeriesIndex: 7, Title: "Forged in Blood (Part 2)"},
		2: {Author: "Ann Other", Series: "Saga B", SeriesIndex: 1, Title: "Awakening"},
		3: {Author: "Ann Other", Series: "Saga A", SeriesIndex: 2, Title: "Awakening"},
	}
	runOptCases(t, twins, []optCase{
		{"bracketed part, its number", aud("Forged in Blood (Part 2)", "Lindsay Buroker", "The Emperor's Edge", 7), 1, 1},
		{"one title in two series", aud("Awakening", "Ann Other", "Saga A", 2), 3, 3},
	})
}

// Options.WordlessFallback: a residual that is only a stopword falls back to
// the whole title; one that is a number stays a number.
func TestOptionsWordlessFallback(t *testing.T) {
	books := []Book{
		0: {Author: "Ryan Rimmel", Series: "Mayor of Noobtown", Title: "The Mayor of Noobtown"},
	}
	runOptCases(t, books, []optCase{
		{"stopword residual", aud("The Mayor of Noobtown", "Ryan Rimmel", "Noobtown", 1), -1, 0},
		// The fallback's words are the series': a later volume must not take the
		// numberless first one on token overlap.
		{"later volume", aud("The Mayor of Noobtown 2", "Ryan Rimmel", "Noobtown", 2), -1, -1},
	})
	o := Options{WordlessFallback: true}
	if got := cleanTitle("The Mayor of Noobtown", "Mayor of Noobtown", o); got != "The Mayor of Noobtown" {
		t.Errorf("stopword residual cleaned to %q", got)
	}
	if got := cleanTitle("Primal Hunter 3", "Primal Hunter", o); got != "3" {
		t.Errorf("numbered residual cleaned to %q, want the number kept", got)
	}
	if got := CleanTitle("The Mayor of Noobtown", "Mayor of Noobtown"); got != "The" {
		t.Errorf("default CleanTitle changed: %q", got)
	}
}

// Options.DropUnabridged: "(Unabridged)" no longer shields a genre tail, so a
// "Series N: genre (Unabridged)" title is bare volume N.
func TestOptionsDropUnabridged(t *testing.T) {
	books := []Book{
		0: {Author: "Zogarth", Series: "Primal Hunter", Title: "The Primal Hunter 14:  A LitRPG Adventure (Unabridged)"},
		1: {Author: "Zogarth", Series: "Primal Hunter", Title: "The Primal Hunter 15:  A LitRPG Adventure (Unabridged)"},
	}
	runOptCases(t, books, []optCase{
		{"bare number against a genre tail", aud("The Primal Hunter 15", "Zogarth", "The Primal Hunter", 15), -1, 1},
		{"its sibling", aud("The Primal Hunter 14", "Zogarth", "The Primal Hunter", 14), -1, 0},
	})
	if got := residualTitle("The Primal Hunter 15: A LitRPG Adventure (Unabridged)", "Primal Hunter", Options{DropUnabridged: true}); got != "The 15" {
		t.Errorf("residual = %q", got)
	}
}

// BestWith reports ScoreASIN for an ASIN match, and the default options are
// Best exactly.
func TestBestWithScore(t *testing.T) {
	books := []Book{{Author: "A", Title: "Alpha", ASIN: "B0X"}, {Author: "A", Title: "Beta"}}
	if i, s, ok := BestWith(books, Query{Title: "zz", ASIN: "b0x"}, Options{}); !ok || i != 0 || s != ScoreASIN {
		t.Errorf("ASIN match = %d %v %v", i, s, ok)
	}
	if i, s, ok := BestWith(books, Query{Title: "Beta", Author: "A"}, Options{}); !ok || i != 1 || s != 1.0 {
		t.Errorf("title match = %d %v %v", i, s, ok)
	}
	if i, s, ok := BestWith(books, Query{Title: "Gamma", Author: "A"}, Options{}); ok || i != -1 || s != 0 {
		t.Errorf("no match = %d %v %v", i, s, ok)
	}
}
