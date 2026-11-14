package importer

import (
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Identifiers (server#366): an import carries the ASIN and ISBN Audiobookshelf
// has for its books onto the books here as match hints (catalog.BookHint, never
// the book's own identifiers), so a match run looks them up first and the books
// end up matched. Every ABS book with one (Payload.Identified) is paired with a
// book here as the history is (bookIndex), but for the whole server, whoever can
// access the book, and only by the tiers that don't read our identifiers: the
// path, then title and author (whose pairing the match run's agreement check
// guards: identifierAgrees). Left out: a book two ABS books pair with
// (contested: one of them is wrong; judged among the ABS books with an
// identifier, the only ones a payload keeps for this), and a book already
// matched here (its own identifiers, or its recorded match, stand).

// allAccess is the matcher's access check for identifiers: every book.
func allAccess(catalog.Ref) bool { return true }

// identifiersFor pairs the identified ABS books (Payload.Identified, none of
// them a gone item: ABS has every one) with books here: the hints an apply
// writes, and the summary's count of them. It reads nothing of a user's, so a
// fetch runs it once for all of them.
func identifiersFor(identified []Item, books *bookIndex) ([]catalog.BookHint, *catalog.ImportIdentifiers) {
	sum := &catalog.ImportIdentifiers{}
	if len(identified) == 0 {
		return nil, sum
	}
	m := books.matcher(allAccess)
	type paired struct {
		hint    catalog.BookHint
		idx     int
		byTitle bool
	}
	var pairs []paired
	per := map[int]int{} // ABS books paired with each book here
	for _, it := range identified {
		h, ok := (catalog.BookHint{ASIN: it.ASIN, ISBN: it.ISBN}).Normalized()
		if !ok {
			continue
		}
		idx, _, split := m.matchPath(it)
		byTitle := false
		if split {
			continue // its discs are books here: none of them is the ABS book
		}
		if idx < 0 {
			idx, _ = m.matchTitle(it)
			byTitle = true
		}
		if idx < 0 {
			continue
		}
		b := m.books[idx]
		h.Ref = catalog.Ref{LibraryID: b.LibraryID, Path: b.RelPath}
		pairs = append(pairs, paired{h, idx, byTitle})
		per[idx]++
	}
	var hints []catalog.BookHint
	for _, x := range pairs {
		switch {
		case per[x.idx] > 1:
			continue
		case m.books[x.idx].Matched:
			sum.AlreadyMatched++
			continue
		}
		hints = append(hints, x.hint)
		sum.Books++
		if x.hint.ASIN != "" {
			sum.ASIN++
		}
		if x.hint.ISBN != "" {
			sum.ISBN++
		}
		if x.byTitle {
			sum.ByTitle++
		}
	}
	return hints, sum
}
