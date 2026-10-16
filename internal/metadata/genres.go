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
// with ";" (a tagger's own joiner) or NUL (ID3v2.4's separator between values,
// where a reader leaves it in). Never on ",": a store's category names hold one
// ("Mystery, Thriller & Suspense"), and a metadata.json names it whole. Blanks and the placeholders
// (placeholderGenres, any case and spacing) are left out; the rest are as the
// tag spells them, since every consumer cleans the list as a label field stores
// it (catalog.CleanLabels: trimmed, spacing collapsed, each label once). Nil for
// none.
func SplitGenres(v string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(v, func(r rune) bool { return r == ';' || r == 0 }) {
		if key := strings.ToLower(strings.Join(strings.Fields(part), " ")); key != "" && !placeholderGenres[key] {
			out = append(out, part)
		}
	}
	return out
}
