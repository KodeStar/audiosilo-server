package matchrun

// The benchmark harness's title-only judge (benchharness_test.go, build tag
// benchharness): kept out of the tag, with its tests, so they run with every
// go test since they need no benchmark data.

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/meta"
	"github.com/kodestar/audiosilo-server/pkg/match"
)

// sameBook is the fuzzy check for a reference without an ASIN this copy
// knows: the work reads like the reference title (sameTitle, or sameNumbered
// for a numbered one), and one of its authors' surnames is in the reference
// author.
func sameBook(c *meta.MatchCandidate, title, author string) bool {
	if !sameTitle(c, title) && !sameNumbered(c, title) {
		return false
	}
	a := match.Fold(author)
	for _, p := range c.Authors {
		if f := strings.Fields(p.Name); len(f) > 0 && strings.Contains(a, match.Fold(f[len(f)-1])) {
			return true
		}
	}
	return false
}

// sameTitle: the work's title (alone, with its subtitle, or the part before a
// colon on either side) reads like the reference title ("&" as "and", a
// leading article dropped).
func sameTitle(c *meta.MatchCandidate, title string) bool {
	ref := []string{normTitle(title), normTitle(beforeColon(title))}
	work := []string{normTitle(c.Title), normTitle(c.Title + " " + c.Subtitle), normTitle(beforeColon(c.Title))}
	for _, r := range ref {
		if r != "" && slices.Contains(work, r) {
			return true
		}
	}
	return false
}

// sameNumbered: the reference title, its bracketed notes dropped, states a
// number (its last) that is the work's position in one of its series, and the
// two titles agree once that number is taken out, with a "Book" or "Vol."
// before it, singular and plural alike; the work's title may stop at its
// colon, as in sameTitle. So "Rise, My Minions 6! (Rise, My Minions!)" is
// "Rise, My Minions! Book 6" at position 6 and "Werepanther 2" is
// "Werepanther: Paranormal Hunter 2" at position 2, never the same series'
// book at another position.
func sameNumbered(c *meta.MatchCandidate, title string) bool {
	ref := titleWords(withAnd(dropBracketed(title)))
	n, ok := lastNumber(ref)
	if !ok || !atPosition(c, n) {
		return false
	}
	stem := numberStem(ref, n)
	if stem == "" {
		return false
	}
	for _, w := range []string{c.Title, beforeColon(c.Title)} {
		if numberStem(titleWords(withAnd(w)), n) == stem {
			return true
		}
	}
	return false
}

// dropBracketed is s without its bracketed notes, "(...)" and "[...]", nested
// ones included; an unclosed bracket drops the rest.
func dropBracketed(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '(' || r == '[':
			depth++
		case (r == ')' || r == ']') && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// lastNumber is the last of ws that is a whole number.
func lastNumber(ws []string) (int, bool) {
	for i := len(ws) - 1; i >= 0; i-- {
		if n, err := strconv.Atoi(ws[i]); err == nil {
			return n, true
		}
	}
	return 0, false
}

// atPosition reports whether n is c's position in one of its series.
func atPosition(c *meta.MatchCandidate, n int) bool {
	return slices.ContainsFunc(c.Series, func(s meta.MatchSeries) bool {
		v, err := strconv.ParseFloat(strings.TrimSpace(s.Position), 64)
		return err == nil && v == float64(n)
	})
}

// numberStem is ws without the number n ("06" is 6) and a "book", "vol" or
// "volume" just before it, a leading article dropped and each word's plural
// "s" taken off, joined.
func numberStem(ws []string, n int) string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		if v, err := strconv.Atoi(w); err == nil && v == n {
			if k := len(out) - 1; k >= 0 && (out[k] == "book" || out[k] == "vol" || out[k] == "volume") {
				out = out[:k]
			}
			continue
		}
		out = append(out, w)
	}
	out = dropArticle(out)
	for i, w := range out {
		if len(w) > 3 {
			out[i] = strings.TrimSuffix(w, "s")
		}
	}
	return strings.Join(out, " ")
}

func withAnd(s string) string { return strings.ReplaceAll(s, "&", " and ") }

func beforeColon(s string) string { before, _, _ := strings.Cut(s, ":"); return before }

func normTitle(s string) string { return strings.Join(dropArticle(titleWords(withAnd(s))), " ") }

// dropArticle is ws without a leading "the", "a" or "an", unless that is all
// there is.
func dropArticle(ws []string) []string {
	if len(ws) > 1 && (ws[0] == "the" || ws[0] == "a" || ws[0] == "an") {
		return ws[1:]
	}
	return ws
}

// work is a candidate as the judge reads it: its title, author and series at
// a position ("" for none).
func work(title, author, series, position string) *meta.MatchCandidate {
	c := &meta.MatchCandidate{Title: title, Authors: []meta.MetaPersonRef{{Name: author}}}
	if series != "" {
		c.Series = []meta.MatchSeries{{Name: series, Position: position}}
	}
	return c
}

func TestSameBook(t *testing.T) {
	const lj, ev = "Logan Jacobs", "Eric Vall"
	for _, tc := range []struct {
		ref, author string
		c           *meta.MatchCandidate
		want        bool
	}{
		// The title alike, as before.
		{"The Hobbit", "J.R.R. Tolkien", work("Hobbit", "J. R. R. Tolkien", "", ""), true},
		{"Pride & Prejudice", "Jane Austen", work("Pride and Prejudice", "Jane Austen", "", ""), true},
		{"Werepanther", lj, work("Werepanther: Paranormal Hunter", lj, "Werepanther", "1"), true},
		{"The Hobbit", "Someone Else", work("The Hobbit", "J. R. R. Tolkien", "", ""), false},

		// The Audiobook Guild shapes: the reference states the position, the
		// work's title says "Book N", a subtitle, or carries a bracketed note.
		{"Backyard Dungeon 3", lj, work("Backyard Dungeon: UK Edition 3", lj, "Backyard Dungeon: UK Edition", "3"), true},
		{"Rise, My Minions! 3", lj, work("Rise, My Minions! Book 3", lj, "Rise, My Minions!", "3"), true},
		{"Rise, My Minions! 2", lj, work("Rise, My Minions! Book 2", lj, "Rise, My Minions!", "2"), true},
		{"Rise, My Minions 6! (Rise, My Minions!)", lj, work("Rise, My Minions! Book 6", lj, "Rise, My Minions!", "6"), true},
		{"Rise, My Minions 7! (Rise, My Minions!)", lj, work("Rise, My Minions! Book 7", lj, "Rise, My Minions!", "7"), true},
		{"Rise, My Minions 8! (Rise, My Minions!)", lj, work("Rise, My Minions! Book 8", lj, "Rise, My Minions!", "8"), true},
		{"Arena 7", lj, work("Arena Book 7", lj, "Arena", "7"), true},
		{"Survive the Monster and Breed 2", lj, work("Survive the Monsters and Breed - Book 2", lj, "Survive the Monsters and Breed", "2"), true},
		{"Survive the Monster and Breed 3", lj, work("Survive the Monsters and Breed - Book 3", lj, "Survive the Monsters and Breed", "3"), true},
		{"Survive the Monsters and Breed 4", lj, work("Survive the Monsters and Breed - Book 4", lj, "Survive the Monsters and Breed", "4"), true},
		{"Werepanther 2", lj, work("Werepanther: Paranormal Hunter 2", lj, "Werepanther", "2"), true},
		{"Werepanther 3", lj, work("Werepanther: Paranormal Hunter 3", lj, "Werepanther", "3"), true},
		{"Werepanther 4", lj, work("Werepanther: Paranormal Hunter 4", lj, "Werepanther", "4"), true},
		{"Werepanther 5", lj, work("Werepanther: Paranormal Hunter 5", lj, "Werepanther", "5"), true},
		{"Werepanther 6", lj, work("Werepanther: Paranormal Hunter 6", lj, "Werepanther", "6"), true},
		{"Werepanther 7", lj, work("Werepanther: Paranormal Hunter 7", lj, "Werepanther", "7"), true},
		{"Werepanther 8", lj, work("Werepanther: Paranormal Hunter 8", lj, "Werepanther", "8"), true},
		{"Werepanther 9", lj, work("Werepanther: Paranormal Hunter 9", lj, "Werepanther", "9"), true},
		{"Dragon Emperor 22 [Dragon Emperor, 22]", ev, work("Dragon Emperor 22", ev, "Dragon Emperor", "22"), true},
		{"King of Superheroes 2", ev, work("King of Superheroes - Book 2", ev, "King of Superheroes", "2"), true},
		{"The Duelist 12 (The Duelist (Completed Series))", ev, work("The Duelist 12", ev, "The Duelist", "12"), true},
		{"Arena 7", lj, work("Arena Vol. 07", lj, "Arena", "7"), true},

		// A sibling at another position stays wrong, however alike the title.
		{"Werepanther 2", lj, work("Werepanther: Paranormal Hunter 3", lj, "Werepanther", "3"), false},
		{"Rise, My Minions! 3", lj, work("Rise, My Minions! Book 2", lj, "Rise, My Minions!", "2"), false},
		// The position alone, or the stem alone, is not enough.
		{"Arena 7", lj, work("Dragon Emperor 7", lj, "Dragon Emperor", "7"), false},
		{"Arena 7", lj, work("Arena Book 7", lj, "", ""), false},
		{"Arena 7", ev, work("Arena Book 7", lj, "Arena", "7"), false},
		// No number stated, or the stem only after the work's colon.
		{"Dragon Conjurer", ev, work("Dragon Conjurer 8", ev, "Dragon Conjurer", "8"), false},
		{"King of Dinosaurs 2", "Michael-Scott Earle", work("Tamer: King of Dinosaurs 2", "Michael-Scott Earle", "Tamer", "2"), false},
		// A number with nothing else is no title.
		{"3", lj, work("Arena Book 3", lj, "Arena", "3"), false},
	} {
		if got := sameBook(tc.c, tc.ref, tc.author); got != tc.want {
			t.Errorf("sameBook(%q, %q by %q) = %v, want %v", tc.c.Title, tc.ref, tc.author, got, tc.want)
		}
	}
}

func TestDropBracketed(t *testing.T) {
	for in, want := range map[string]string{
		"The Duelist 12 (The Duelist (Completed Series))": "The Duelist 12 ",
		"Dragon Emperor 22 [Dragon Emperor, 22]":          "Dragon Emperor 22 ",
		"Laws By Plato (":                                 "Laws By Plato ",
		"Stray ) bracket":                                 "Stray ) bracket",
	} {
		if got := dropBracketed(in); got != want {
			t.Errorf("dropBracketed(%q) = %q, want %q", in, got, want)
		}
	}
}
