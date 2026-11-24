// Package textclean is the server's one rule for free text it did not write
// and keeps or shows anyway (what a Bridge reports, a podcast directory's
// titles, an exported OPML title): cleaned and bounded rather than refused, so
// one odd value never fails the report, search or export it came in. A field
// that must be exact (a path, an id) is checked and refused instead, never
// cleaned (bridge.checkText, metadata's attachment checks).
package textclean

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Clean is v as printable bounded text: valid UTF-8 (a broken sequence becomes
// U+FFFD), each control character (C0, DEL and C1) a space, except a line break
// when lines keeps them, and at most maxBytes, cut at a character's boundary.
// The cut text is a copy, so a kept value never holds the whole of v.
func Clean(v string, maxBytes int, lines bool) string {
	return cut(printable(v, lines), maxBytes)
}

// Line is v cleaned onto one line (Clean) and trimmed: the bound counts the
// text, not the spaces around it.
func Line(v string, maxBytes int) string {
	return strings.TrimSpace(cut(strings.TrimSpace(printable(v, false)), maxBytes))
}

func printable(v string, lines bool) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && (!lines || r != '\n') {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(v, "\uFFFD"))
}

// cut is v at most maxBytes long, cut at a character's boundary, as a copy.
func cut(v string, maxBytes int) string {
	if len(v) <= maxBytes {
		return v
	}
	n := max(maxBytes, 0)
	for n > 0 && !utf8.RuneStart(v[n]) {
		n--
	}
	return strings.Clone(v[:n])
}

// FirstNonEmpty is the first of vals that isn't blank, trimmed; "" when every
// one is.
func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
