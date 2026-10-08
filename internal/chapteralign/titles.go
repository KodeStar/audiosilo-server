package chapteralign

import (
	"regexp"
	"strings"
	"unicode"
)

// leadingNumber is a title's own numbering: "1. ", "Chapter 3: ", "Part 2 - ", or
// the whole of a bare "Chapter 12".
var leadingNumber = regexp.MustCompile(`^\s*(?:(chapter|part|book|track|disc|cd)\s*)?(\d+)\s*[.:)\]\-–—]*(?:\s+|$)`)

// stopWords carry no title of their own, so they don't pair two titles.
var stopWords = map[string]bool{"the": true, "a": true, "an": true, "and": true, "of": true, "or": true, "to": true, "in": true}

// numbering is a title's leading number and the word naming it ("chapter",
// "part"; "" for a bare number).
type numbering struct{ kind, n string }

// same reports whether two numberings name the same place: the same number, and
// the same kind unless one is bare ("Part 2" is not "Chapter 2", but "2." may be
// either).
func (a numbering) same(b numbering) bool {
	return a.n != "" && a.n == b.n && (a.kind == b.kind || a.kind == "" || b.kind == "")
}

// titleWords splits a title into its leading numbering (if any) and its words,
// lowercased, any script's letters and digits kept.
func titleWords(s string) (num numbering, words []string) {
	s = strings.ToLower(s)
	if m := leadingNumber.FindStringSubmatch(s); m != nil {
		num, s = numbering{kind: m[1], n: m[2]}, s[len(m[0]):]
	}
	words = strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	return num, words
}

// titleSim is how much two titles share: the overlap of their word sets over the
// smaller one (so "The Punishments" pairs "The Punishments - The Gift" and "Out of
// Chaos" pairs "1. Out of Chaos"), stop words left out unless a title is nothing
// else, and a long word one letter off still the same word (a typo:
// "Persophone"). A title that is only numbering ("Chapter 12") pairs on the
// number, with a bare one or one that adds a name ("Chapter 12: The Storm").
func titleSim(a, b string) float64 {
	na, wa := titleWords(a)
	nb, wb := titleWords(b)
	sa, sb := wordSet(wa), wordSet(wb)
	if len(sa) == 0 || len(sb) == 0 {
		if (len(wa) == 0 || len(wb) == 0) && na.same(nb) {
			return 1
		}
		return 0
	}
	shared := 0
	for w := range sa {
		if sb[w] {
			shared++
			continue
		}
		for v := range sb {
			if oneEdit(w, v) {
				shared++
				break
			}
		}
	}
	return float64(shared) / float64(min(len(sa), len(sb)))
}

// wordSet is a title's words without the stop words, or with them when that would
// leave none.
func wordSet(words []string) map[string]bool {
	set := map[string]bool{}
	for _, w := range words {
		if !stopWords[w] {
			set[w] = true
		}
	}
	if len(set) == 0 {
		for _, w := range words {
			set[w] = true
		}
	}
	return set
}

// oneEdit reports whether two words of typoLen letters or more are one
// insertion, deletion or substitution apart.
func oneEdit(a, b string) bool {
	ra, rb := []rune(a), []rune(b)
	if len(ra) < typoLen || len(rb) < typoLen || len(ra)-len(rb) > 1 || len(rb)-len(ra) > 1 {
		return false
	}
	i := 0
	for i < len(ra) && i < len(rb) && ra[i] == rb[i] {
		i++
	}
	switch {
	case i == len(ra) && i == len(rb):
		return true // the same word
	case len(ra) == len(rb):
		return string(ra[i+1:]) == string(rb[i+1:])
	case len(ra) > len(rb):
		return string(ra[i+1:]) == string(rb[i:])
	default:
		return string(ra[i:]) == string(rb[i+1:])
	}
}

// typoLen is the shortest word a typo is forgiven in.
const typoLen = 5
