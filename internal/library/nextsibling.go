package library

import "path"

// NextSibling is what to play after the book at current, read from its folder:
// entries is the whole listing of current's parent (BrowseFS paged to the end),
// with IsBook set on the entries the index holds as books. It is the first book
// or folder whose name sorts after current's (naturalCompare: case folded,
// numbers by value, so "Book 2" comes before "Book 10"), preferring an indexed
// book. A bare folder is offered only when nothing else in the listing is
// indexed yet (a folder mid-scan): once anything is, a folder left over is not a
// book (Bonus, artwork) and would strand the player. Loose files that are not
// books are never offered. nil when nothing follows.
//
// It is the player's findNextSibling (next-book.ts), moved behind GET
// /libraries/{id}/next. Order is by name, deliberately not by series_index,
// which many libraries number differently or not at all. One difference: the
// current book itself does not count as "indexed" here. It always is by the
// time anyone asks (opening it indexes it on demand), so counting it would
// leave the mid-scan fallback unreachable.
func NextSibling(entries []Entry, current string) *Entry {
	leaf := path.Base(current)
	var book, dir *Entry
	indexed := false
	for i := range entries {
		e := &entries[i]
		if e.Path == current {
			continue
		}
		indexed = indexed || e.IsBook
		if (!e.IsBook && !e.IsDir) || naturalCompare(e.Name, leaf) <= 0 {
			continue
		}
		if e.IsBook {
			if book == nil || discOrder(e.Path, book.Path) < 0 {
				book = e
			}
		} else if dir == nil || discOrder(e.Path, dir.Path) < 0 {
			dir = e
		}
	}
	switch {
	case book != nil:
		return book
	case indexed:
		return nil
	default:
		return dir
	}
}
