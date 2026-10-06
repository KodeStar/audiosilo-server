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
		"the current book does not count as indexed": {
			[]Entry{dirEntry("Book 1", true), dirEntry("Book 2", false)},
			"S/Book 1", "S/Book 2",
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
