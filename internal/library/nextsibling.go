package library

import (
	"context"
	"path"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// NextInFolder is what to play after the book at rel, read from its folder for a
// caller with scope in lib: the folder listed as they may open it (ListDir with
// scope and the library's ignore rules), its entries marked from the index
// (MarkBooks), then NextSibling. book is the index's book at the entry, nil for a
// bare folder. next is nil when nothing follows or the folder cannot be read; an
// error is the index's.
func NextInFolder(ctx context.Context, cat *catalog.Catalog, lib *catalog.Library, scope catalog.Scope, rel string) (next *Entry, book *catalog.Book, err error) {
	parent := path.Dir(rel)
	if parent == "." {
		parent = ""
	}
	var allow func(string) bool
	if !scope.AllowAll {
		allow = scope.Allows
	}
	entries, err := ListDir(lib.Root, parent, allow, ParseIgnore(lib.IgnorePatterns))
	if err != nil {
		return nil, nil, nil
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Path != rel {
			paths = append(paths, e.Path)
		}
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
// listing of its folder with IsBook set on the entries the index holds as books:
// the first book or folder whose name sorts after current's (naturalCompare: case
// folded, numbers by value, so "Book 2" comes before "Book 10"), preferring an
// indexed book. A bare folder is offered only when nothing else in the listing is
// indexed (a folder mid-scan): once anything is, a folder left over is not a book
// (Bonus, artwork) and would strand the player. Loose files that are not books are
// never offered. current itself never counts as indexed. nil when nothing
// follows. Order is by name, not series_index.
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
