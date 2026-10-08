// Package names reads the people in the catalogue's single Author and Narrator
// strings: which names a credit holds (Split), whether a name is written
// "Surname, Given" (Reversed), and the key a credit sorts by surname first
// (SortKey). Tag data is messy, so every rule here is deliberately shy: a wrong
// split invents a person, a wrong surname misfiles one.
package names

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/kodestar/audiosilo-server/pkg/match"
)

// separatorRE matches the joiners that unambiguously separate two contributors
// in one credit. A comma is NOT here: it is ambiguous ("Alexandre Dumas, pere")
// and is handled separately by splitOnCommas.
var separatorRE = regexp.MustCompile(`;|\s+&\s+|\s+and\s+`)

// Split turns a single Author (or Narrator) credit into a list of names,
// splitting ONLY where the string clearly holds several: unambiguous joiners
// always split; a comma splits only when every resulting part still looks like a
// full name (at least two words), which keeps suffixed names such as "Alexandre
// Dumas, pere" whole. Returns nil for a blank string.
func Split(s string) []string {
	var out []string
	for _, chunk := range cleanParts(separatorRE.Split(s, -1)) {
		out = append(out, splitOnCommas(chunk)...)
	}
	return out // nil when the string named nobody
}

// splitOnCommas splits one already-cleaned chunk on commas, but only when every
// piece has at least two words and the chunk isn't one "Surname, Given" name
// (Reversed: "Le Guin, Ursula K.") - otherwise the comma is part of a single name
// ("Dumas, pere"; "Doe, John") and the chunk is returned whole.
func splitOnCommas(p string) []string {
	pieces := cleanParts(strings.Split(p, ","))
	if _, _, reversed := Reversed(p); len(pieces) < 2 || reversed {
		return []string{p}
	}
	for _, piece := range pieces {
		if len(strings.Fields(piece)) < 2 {
			return []string{p}
		}
	}
	return pieces
}

// cleanParts trims whitespace and dangling separator punctuation from BOTH ends
// of each part and drops the empties. Trailing: an "A, B, and C" split leaves a
// trailing comma. Leading: a half-empty "Last, First" tag arrives as ", Jane
// Doe", and splitOnCommas keeps such a chunk whole, so an untrimmed leading comma
// would otherwise stay part of the name.
func cleanParts(parts []string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimFunc(p, func(r rune) bool {
			return unicode.IsSpace(r) || r == ';' || r == '&' || r == ','
		})
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Reversed reports whether name is written "Surname, Given" (one word before a
// single comma, or one word after particles such as "Le Guin, Ursula K.", so
// "Alexandre Dumas, pere" is not), with its two parts.
func Reversed(name string) (given, surname string, ok bool) {
	before, after, found := strings.Cut(name, ",")
	if !found || strings.Contains(after, ",") {
		return "", "", false
	}
	b, a := strings.TrimSpace(before), strings.TrimSpace(after)
	if a == "" {
		return "", "", false
	}
	if words := strings.Fields(b); len(words) > 1 {
		for _, w := range words[:len(words)-1] {
			if !particles[match.Fold(w)] {
				return "", "", false
			}
		}
	}
	return a, b, true
}

// suffixes trail a name without being its surname ("Martin Luther King Jr").
var suffixes = map[string]bool{
	"jr": true, "sr": true, "ii": true, "iii": true, "iv": true, "phd": true, "md": true,
}

// particles join the word after them in a surname ("Ursula K. Le Guin" files
// under Le Guin, "Ludwig van Beethoven" under van Beethoven).
var particles = map[string]bool{
	"van": true, "von": true, "de": true, "da": true, "di": true, "du": true, "del": true,
	"della": true, "der": true, "den": true, "le": true, "la": true, "st": true, "ter": true,
}

// SortKey is the key a credit sorts by when people are ordered surname first:
// its first name (Split), as surname then given names, folded (match.Fold, so
// case and punctuation don't count), with a space between the two so every
// "Smith" comes before "Smithers". "Surname, Given" is read as written; otherwise
// the surname is the last word, after any suffix ("Jr") and with any particle
// before it ("Le Guin"). A one-word name is its own key; "" for a blank credit.
// The catalog registers it as the SQL function name_sort, so a query orders by
// exactly what Go computes for a keyset cursor.
func SortKey(credit string) string {
	people := Split(credit)
	if len(people) == 0 {
		return ""
	}
	name := people[0]
	// A suffix after a second comma ("King, Martin Luther, Jr.") is no part of
	// the surname or the given names, so it doesn't stop a "Surname, Given" read.
	if i := strings.LastIndex(name, ","); i >= 0 && suffixes[match.Fold(name[i+1:])] {
		name = strings.TrimSpace(name[:i])
	}
	if given, surname, ok := Reversed(name); ok {
		return match.Fold(surname) + " " + match.Fold(given)
	}
	// A comma left in one name ends it with a suffix ("Alexandre Dumas, pere").
	name, _, _ = strings.Cut(name, ",")
	words := strings.Fields(name)
	for len(words) > 1 && suffixes[match.Fold(words[len(words)-1])] {
		words = words[:len(words)-1]
	}
	start := len(words) - 1
	for start > 1 && particles[match.Fold(words[start-1])] {
		start--
	}
	return match.Fold(strings.Join(words[start:], "")) + " " + match.Fold(strings.Join(words[:start], ""))
}
