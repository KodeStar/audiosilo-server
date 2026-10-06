package library

import (
	"context"
	"path"
	"strings"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// NextInFolder is what to play after the book at rel, read from its folder for a
// caller with scope in lib: the folder listed as they may open it (ListDir with
// scope and the library's ignore rules), its entries marked from the index
// (MarkBooks), then NextSibling. book is the index's book at the entry, nil for a
// bare folder. next is nil when nothing follows or the folder cannot be read; an
// error is the index's.
func NextInFolder(ctx context.Context, cat *catalog.Catalog, lib *catalog.Library, scope catalog.Scope, rel string) (next *Entry, book *catalog.Book, err error) {
	var allow func(string) bool
	if !scope.AllowAll {
		allow = scope.Allows
	}
	entries, err := ListDir(lib.Root, dirOf(rel), allow, ParseIgnore(lib.IgnorePatterns))
	if err != nil {
		return nil, nil, nil
	}
	// Every entry is marked, the current book included, as the folder listing
	// marks it for the player.
	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = e.Path
	}
	books, err := cat.BooksByPaths(ctx, lib.ID, paths)
	if err != nil {
		return nil, nil, err
	}
	MarkBooks(entries, books)
	next = NextSibling(entries, rel)
	if next == nil {
		return nil, nil, nil
	}
	if b, ok := books[next.Path]; ok {
		book = &b
	}
	return next, book, nil
}

// NextSibling is what to play after the book at current, read from the whole
// listing of its folder with IsBook set on the entries the index holds as books
// (the player's findNextSibling): the first book or folder whose name sorts after
// current's, preferring an indexed book. Names sort as the player sorts them
// (localeCompare, numeric and base sensitivity: case and accents folded, numbers
// by value, so "Book 2" comes before "Book 10" and "01 - 1984" before "02 -
// Animal Farm"), ties by path. A bare folder is offered only when nothing in the
// listing is indexed, current included (a folder mid-scan): once anything is, a
// folder left over is not a book (Bonus, artwork, a series or author folder) and
// would strand the player. Loose files that are not books are never offered. nil
// when nothing follows. Order is by name, not series_index.
func NextSibling(entries []Entry, current string) *Entry {
	names := collate.New(language.Und, collate.Loose, collate.Numeric)
	before := func(a, b *Entry) bool {
		if c := names.CompareString(a.Name, b.Name); c != 0 {
			return c < 0
		}
		return strings.Compare(a.Path, b.Path) < 0
	}
	leaf := path.Base(current)
	var book, dir *Entry
	indexed := false
	for i := range entries {
		e := &entries[i]
		indexed = indexed || e.IsBook
		if e.Path == current || (!e.IsBook && !e.IsDir) || names.CompareString(e.Name, leaf) <= 0 {
			continue
		}
		if e.IsBook {
			if book == nil || before(e, book) {
				book = e
			}
		} else if dir == nil || before(e, dir) {
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
