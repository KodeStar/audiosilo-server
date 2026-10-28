package library

import (
	"context"
	"log/slog"
	"path"
	"slices"
	"strings"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// NextInFolder is what to play after the book at rel, read from its folder for a
// caller with scope in lib: the folder listed as they may open it (ListDir with
// scope and the library's ignore rules), its entries marked from the index
// (MarkBooks) and the pending folders after rel (markPendingNext, best-effort:
// logged to log), then NextSibling. book is the index's book at the entry, nil
// for a bare or pending folder. next is nil when nothing follows or the folder
// cannot be read; an error is the index's.
func NextInFolder(ctx context.Context, cat *catalog.Catalog, lib *catalog.Library, scope catalog.Scope, rel string, log *slog.Logger) (next *Entry, book *catalog.Book, err error) {
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
	markPendingNext(ctx, cat, lib, scope, ignore, entries, rel, log)
	next = NextSibling(entries, rel)
	if next == nil {
		return nil, nil, nil
	}
	if b, ok := books[next.Path]; ok {
		book = &b
	}
	return next, book, nil
}

// pendingNextBatch is how many of the folders after rel markPendingNext reads
// at a time, in name order.
const pendingNextBatch = 16

// markPendingNext marks the folders after rel among entries that a scan would
// make books of but the index doesn't hold yet (MarkPendingBooks, as the folder
// listing marks them for the player), so the book that follows is offered
// before a scan reaches it (server#211). Only the folders sorting after rel and
// before the first indexed book after it are read (one after that book could
// never be the answer), in name order a batch at a time, stopping at the batch
// that holds a pending book (the answer is the first): a long series folder no
// scan has reached costs a few directory reads, not one per folder, and the
// index lookup's bound variables stay few. Best-effort, like the listing's
// marks, within pendingTime in all: a failed index read is logged and leaves the
// rest unmarked, so the folder step answers from the index alone; without the
// library's overrides none are marked (a `collection` folder would read as one).
func markPendingNext(ctx context.Context, cat *catalog.Catalog, lib *catalog.Library, scope catalog.Scope, ignore *Ignore, entries []Entry, rel string, log *slog.Logger) {
	names := newNameCollator()
	leaf := path.Base(rel)
	var first *Entry // the first indexed book after rel
	for i := range entries {
		e := &entries[i]
		if e.IsBook && e.Path != rel && names.CompareString(e.Name, leaf) > 0 && (first == nil || entryBefore(names, e, first)) {
			first = e
		}
	}
	var later []*Entry
	for i := range entries {
		e := &entries[i]
		if e.IsDir && !e.IsBook && e.Path != rel && names.CompareString(e.Name, leaf) > 0 && (first == nil || entryBefore(names, e, first)) {
			later = append(later, e)
		}
	}
	if len(later) == 0 {
		return
	}
	failed := func(err error) {
		if ctx.Err() == nil {
			log.Warn("next book: pending folders not read", "library", lib.ID, "path", rel, "err", err)
		}
	}
	overrides, err := cat.FolderOverrides(ctx, lib.ID)
	if err != nil {
		failed(err)
		return
	}
	slices.SortFunc(later, func(a, b *Entry) int {
		if entryBefore(names, a, b) {
			return -1
		}
		if entryBefore(names, b, a) {
			return 1
		}
		return 0
	})
	ctx, cancel := context.WithTimeout(ctx, pendingTime)
	defer cancel()
	for len(later) > 0 && ctx.Err() == nil {
		batch := later[:min(pendingNextBatch, len(later))]
		later = later[len(batch):]
		dirs := make([]string, len(batch))
		in := make(map[string]bool, len(batch))
		for i, e := range batch {
			dirs[i], in[e.Path] = e.Path, true
		}
		holding, err := cat.FoldersHoldingBooks(ctx, lib.ID, dirs)
		if err != nil {
			failed(err)
			return
		}
		MarkPendingBooks(ctx, *lib, entries, overrides, holding, func(p string) bool {
			return in[p] && scope.Allows(p)
		}, ignore)
		if slices.ContainsFunc(batch, func(e *Entry) bool { return e.PendingBook }) {
			return
		}
	}
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
	names := newNameCollator()
	before := func(a, b *Entry) bool { return entryBefore(names, a, b) }
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

// newNameCollator sorts names as the player does (NextSibling's order).
func newNameCollator() *collate.Collator {
	return collate.New(language.Und, collate.Loose, collate.Numeric)
}

// entryBefore is NextSibling's order: by name (names), ties by path.
func entryBefore(names *collate.Collator, a, b *Entry) bool {
	if c := names.CompareString(a.Name, b.Name); c != 0 {
		return c < 0
	}
	return strings.Compare(a.Path, b.Path) < 0
}
