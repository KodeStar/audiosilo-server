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
	ignore := ParseIgnore(lib.IgnorePatterns)
	entries, err := ListDir(lib.Root, dirOf(rel), allow, ignore)
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
	if err := markPendingNext(ctx, cat, lib, scope, ignore, entries, rel); err != nil {
		return nil, nil, err
	}
	next = NextSibling(entries, rel)
	if next == nil {
		return nil, nil, nil
	}
	if b, ok := books[next.Path]; ok {
		book = &b
	}
	return next, book, nil
}

// markPendingNext marks the folders after rel among entries that a scan would
// make books of but the index doesn't hold yet (MarkPendingBooks, as the folder
// listing marks them for the player), so the book that follows is offered
// before a scan reaches it (server#211). Only the folders sorting after rel are
// read. Without the library's overrides none are marked (a `collection` folder
// would read as one).
func markPendingNext(ctx context.Context, cat *catalog.Catalog, lib *catalog.Library, scope catalog.Scope, ignore *Ignore, entries []Entry, rel string) error {
	names := collate.New(language.Und, collate.Loose, collate.Numeric)
	leaf := path.Base(rel)
	var dirs []string
	for _, e := range entries {
		if e.IsDir && !e.IsBook && e.Path != rel && names.CompareString(e.Name, leaf) > 0 {
			dirs = append(dirs, e.Path)
		}
	}
	if len(dirs) == 0 {
		return nil
	}
	overrides, err := cat.FolderOverrides(ctx, lib.ID)
	if err != nil {
		return err
	}
	holding, err := cat.FoldersHoldingBooks(ctx, lib.ID, dirs)
	if err != nil {
		return err
	}
	after := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		after[d] = true
	}
	MarkPendingBooks(ctx, *lib, entries, overrides, holding, func(p string) bool {
		return after[p] && scope.Allows(p)
	}, ignore)
	return nil
}

// NextSibling is what to play after the book at current, read from the whole
// listing of its folder with IsBook set on the entries the index holds as books
// and PendingBook on the folders a scan would make books of (the player's
// findNextSibling): the first book or folder whose name sorts after current's,
// preferring a book, indexed or pending (each is what the player opens as one). Names sort as the player sorts them
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
		isBook := e.IsBook || e.PendingBook
		indexed = indexed || isBook
		if e.Path == current || (!isBook && !e.IsDir) || names.CompareString(e.Name, leaf) <= 0 {
			continue
		}
		if isBook {
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
