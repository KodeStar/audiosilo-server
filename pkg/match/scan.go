package match

import (
	"strconv"
	"strings"
)

// Hand-written scanners for the regexps the matcher used: the regexps were half
// of Best's time, on titles a few dozen bytes long. Each reproduces Go regexp's
// leftmost-first answer for its pattern exactly; scan_test.go checks them
// against the regexps themselves on every title of a metadata release (when one
// is given) and on random strings made of the patterns' hard parts.
//   - \b is the ASCII word boundary (\w = [0-9A-Za-z_]); a non-ASCII byte is never
//     a word byte, as a non-ASCII rune is never \w.
//   - \s is [\t\n\f\r ], \d is [0-9].
//   - (?i) folds the keywords' letters. The only non-ASCII runes that fold onto
//     those letters are U+017F (ſ, s) and U+212A (Kelvin sign, k): a string holding
//     either goes to the regexp (needsFoldFallback), so the scanners only see
//     strings where ASCII case folding is the whole story.

// needsFoldFallback reports whether s holds a rune (?i) folds onto a keyword's
// letter: s then goes to the regexp.
func needsFoldFallback(s string) bool {
	// ſ = C5 BF, K = E2 84 AA.
	return strings.Contains(s, "\u017f") || strings.Contains(s, "\u212a")
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

func isAlnumByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || isDigitByte(c)
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r'
}

// boundaryAt is regexp's \b at byte offset i.
func boundaryAt(s string, i int) bool {
	before := i > 0 && isWordByte(s[i-1])
	after := i < len(s) && isWordByte(s[i])
	return before != after
}

// hasPrefixFoldASCII reports whether s[i:] starts with the lowercase ASCII kw,
// ignoring ASCII case.
func hasPrefixFoldASCII(s string, i int, kw string) bool {
	if len(s)-i < len(kw) {
		return false
	}
	for k := 0; k < len(kw); k++ {
		c := s[i+k]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != kw[k] {
			return false
		}
	}
	return true
}

// numberAt matches `\d+(?:\.\d+)?\b` at j (s[j] a digit), returning its end.
// Backtracking can only ever end the match at the end of the digit run or of the
// fraction's digit run: any shorter end sits between two digits, never a boundary.
func numberAt(s string, j int) (int, bool) {
	d1 := j
	for d1 < len(s) && isDigitByte(s[d1]) {
		d1++
	}
	if d1 == j {
		return 0, false
	}
	if d1 < len(s) && s[d1] == '.' {
		d2 := d1 + 1
		for d2 < len(s) && isDigitByte(s[d2]) {
			d2++
		}
		if d2 > d1+1 && boundaryAt(s, d2) {
			return d2, true
		}
	}
	if boundaryAt(s, d1) {
		return d1, true
	}
	return 0, false
}

var markerKeywords = [...]string{"book", "bk", "vol", "volume", "part", "pt", "episode", "ep", "#"}

// markerAt matches markerSeq at i: (end, number start).
func markerAt(s string, i int) (end, num int, ok bool) {
	if !boundaryAt(s, i) {
		return 0, 0, false
	}
	for _, kw := range markerKeywords {
		if !hasPrefixFoldASCII(s, i, kw) {
			continue
		}
		j := i + len(kw)
		for j < len(s) && isSpaceByte(s[j]) {
			j++
		}
		if j < len(s) && s[j] == '.' {
			j++
		}
		for j < len(s) && isSpaceByte(s[j]) {
			j++
		}
		if j < len(s) && isDigitByte(s[j]) {
			if e, ok := numberAt(s, j); ok {
				return e, j, true
			}
		}
	}
	return 0, 0, false
}

func markerStartByte(c byte) bool {
	switch c | 0x20 {
	case 'b', 'v', 'p', 'e':
		return true
	}
	return c == '#'
}

// replaceMarkers is markerSeq.ReplaceAllString(s, " ").
func replaceMarkers(s string) string {
	if needsFoldFallback(s) {
		return markerSeq.ReplaceAllString(s, " ")
	}
	var b strings.Builder
	last := 0
	for i := 0; i < len(s); {
		if markerStartByte(s[i]) {
			if e, _, ok := markerAt(s, i); ok {
				if last == 0 {
					b.Grow(len(s))
				}
				b.WriteString(s[last:i])
				b.WriteByte(' ')
				last, i = e, e
				continue
			}
		}
		i++
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// findMarker is markerSeq.FindStringSubmatch(s)[1] parsed as a float.
func findMarker(s string) (float64, bool) {
	if needsFoldFallback(s) {
		if m := markerSeq.FindStringSubmatch(s); m != nil {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				return v, true
			}
		}
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if markerStartByte(s[i]) {
			if e, n, ok := markerAt(s, i); ok {
				v, err := strconv.ParseFloat(s[n:e], 64)
				return v, err == nil
			}
		}
	}
	return 0, false
}

// findFirstNumber is the first `\b(\d+(?:\.\d+)?)\b` in s, parsed as a float.
func findFirstNumber(s string) (float64, bool) {
	for i := 0; i < len(s); i++ {
		if isDigitByte(s[i]) && boundaryAt(s, i) {
			if e, ok := numberAt(s, i); ok {
				v, err := strconv.ParseFloat(s[i:e], 64)
				return v, err == nil
			}
		}
	}
	return 0, false
}

var fluffList = [...]string{"series", "unabridged", "abridged", "audiobook", "dramatized", "adaptation"}

// replaceFluff is fluffWords.ReplaceAllString(s, " ").
func replaceFluff(s string) string {
	if needsFoldFallback(s) {
		return fluffWords.ReplaceAllString(s, " ")
	}
	var b strings.Builder
	last := 0
	for i := 0; i < len(s); {
		switch s[i] | 0x20 {
		case 's', 'u', 'a', 'd':
			if boundaryAt(s, i) {
				matched := -1
				for _, w := range fluffList {
					if hasPrefixFoldASCII(s, i, w) && boundaryAt(s, i+len(w)) {
						matched = i + len(w)
						break
					}
				}
				if matched >= 0 {
					if last == 0 {
						b.Grow(len(s))
					}
					b.WriteString(s[last:i])
					b.WriteByte(' ')
					last, i = matched, matched
					continue
				}
			}
		}
		i++
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// replaceParenGroupsOnce replaces each innermost bracketed group in s, an opener
// ( or [, no bracket, a closer ) or ], with a space (`[\(\[][^\(\)\[\]]*[\)\]]`),
// and reports whether it replaced any.
func replaceParenGroupsOnce(s string) (string, bool) {
	var b strings.Builder
	last, any := 0, false
	for i := 0; i < len(s); {
		if c := s[i]; c == '(' || c == '[' {
			j := i + 1
			for j < len(s) && s[j] != '(' && s[j] != ')' && s[j] != '[' && s[j] != ']' {
				j++
			}
			if j < len(s) && (s[j] == ')' || s[j] == ']') {
				b.WriteString(s[last:i])
				b.WriteByte(' ')
				last, i, any = j+1, j+1, true
				continue
			}
			i = j // no group starts before the next bracket
			continue
		}
		i++
	}
	if !any {
		return s, false
	}
	b.WriteString(s[last:])
	return b.String(), true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
