package library

import "testing"

// dirEntry and fileEntry build a listing entry of folder "S"; book marks it indexed.
func dirEntry(name string, book bool) Entry {
	return Entry{Name: name, Path: "S/" + name, IsDir: true, IsBook: book}
}

func fileEntry(name string, book bool) Entry {
	return Entry{Name: name, Path: "S/" + name, IsAudio: true, IsBook: book}
}

func TestNextSibling(t *testing.T) {
	for name, tc := range map[string]struct {
		entries []Entry
		current string
		want    string // "" = nil
	}{
		"natural numeric order": {
			[]Entry{dirEntry("Book 10", true), dirEntry("Book 2", true), dirEntry("Book 1", true)},
			"S/Book 1", "S/Book 2",
		},
		"case folded": {
			[]Entry{dirEntry("book 3", true), dirEntry("Book 2", true)},
			"S/Book 2", "S/book 3",
		},
		"prefers the next indexed book over a bare folder": {
			[]Entry{dirEntry("Book 1", true), dirEntry("Book 2 Extras", false), dirEntry("Book 3", true)},
			"S/Book 1", "S/Book 3",
		},
		"a bare folder left over is not a book": {
			[]Entry{dirEntry("Book 1", true), dirEntry("Book 2", true), dirEntry("Extras", false)},
			"S/Book 2", "",
		},
		"wholly unindexed folder falls back to the next folder": {
			[]Entry{dirEntry("Book 1", false), dirEntry("Book 2", false), fileEntry("Book 3.mp3", false)},
			"S/Book 1", "S/Book 2",
		},
		"the current book counts as indexed, as in the player": {
			[]Entry{dirEntry("Book 1", true), dirEntry("Book 2", false)},
			"S/Book 1", "",
		},
		"a series folder beside the only book is not a book": {
			[]Entry{dirEntry("Elantris", true), dirEntry("Mistborn", false)},
			"S/Elantris", "",
		},
		"an earlier indexed book does": {
			[]Entry{dirEntry("Book 0", true), dirEntry("Book 1", true), dirEntry("Book 2", false)},
			"S/Book 1", "",
		},
		"indexed files count": {
			[]Entry{fileEntry("01.m4b", true), fileEntry("02.m4b", true)},
			"S/01.m4b", "S/02.m4b",
		},
		"loose non-book files are never offered": {
			[]Entry{fileEntry("01.mp3", false), fileEntry("02.mp3", false)},
			"S/01.mp3", "",
		},
		"the last": {
			[]Entry{dirEntry("Book 1", true), dirEntry("Book 2", true)},
			"S/Book 2", "",
		},
		"numbers apart from the title's own": {
			[]Entry{dirEntry("02 - Animal Farm", true), dirEntry("01 - 1984", true)},
			"S/01 - 1984", "S/02 - Animal Farm",
		},
		"a title that starts with a number": {
			[]Entry{dirEntry("Reacher 13 - Gone Tomorrow", true), dirEntry("Reacher 15 - Worth Dying For", true), dirEntry("Reacher 14 - 61 Hours", true)},
			"S/Reacher 13 - Gone Tomorrow", "S/Reacher 14 - 61 Hours",
		},
		"never back to an earlier title": {
			[]Entry{dirEntry("Reacher 14 - 61 Hours", true), dirEntry("Reacher 15 - Worth Dying For", true)},
			"S/Reacher 15 - Worth Dying For", "",
		},
		"a novella numbered between two books": {
			[]Entry{dirEntry("Kingkiller 2 - The Wise Man's Fear", true), dirEntry("Kingkiller 2.5 - The Slow Regard", true), dirEntry("Kingkiller 1 - The Name of the Wind", true)},
			"S/Kingkiller 1 - The Name of the Wind", "S/Kingkiller 2 - The Wise Man's Fear",
		},
		"accents folded": {
			[]Entry{dirEntry("Eve", true), dirEntry("Émile", true), dirEntry("Anna", true)},
			"S/Anna", "S/Émile",
		},
		"names read alike keep a stable order": {
			[]Entry{dirEntry("Book 02", true), dirEntry("Book 2", true), dirEntry("Book 1", true)},
			"S/Book 1", "S/Book 02",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := ""
			if e := NextSibling(tc.entries, tc.current); e != nil {
				got = e.Path
			}
			if got != tc.want {
				t.Fatalf("NextSibling = %q, want %q", got, tc.want)
			}
		})
	}
}
