package metadata

import "strings"

// placeholderGenres are the genre tags that say only that the file is an
// audiobook (or spoken audio at all), which every book in a library is: the
// genre a ripping tool or a store writes when it has no real one. They are
// dropped, so a book's genres are the ones that tell it from another book.
var placeholderGenres = map[string]bool{
	"audiobook": true, "audiobooks": true, "audio book": true, "audio books": true,
	"speech": true, "spoken word": true, "other": true,
}

// SplitGenres reads a genre tag as a list: one tag often holds several, joined
// with ";" or "," (a tagger's own joiner) or NUL (ID3v2.4's separator between
// values, where a reader leaves it in). Each is trimmed with its inner spacing
// collapsed; blanks, the placeholders (placeholderGenres, any case) and a
// repeat of an earlier one (any case) are left out. Nil for none.
func SplitGenres(v string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.FieldsFunc(v, func(r rune) bool { return r == ';' || r == ',' || r == 0 }) {
		g := strings.Join(strings.Fields(part), " ")
		key := strings.ToLower(g)
		if g == "" || placeholderGenres[key] || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, g)
	}
	return out
}
