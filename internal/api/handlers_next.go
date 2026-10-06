package api

import (
	"context"
	"errors"
	"net/http"
	"path"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

// The sources of a next book, most to least informed.
const (
	nextCommunity = "community"
	nextSeries    = "series"
	nextFolder    = "folder"
	nextNone      = "none"
)

// nextBook is GET /libraries/{id}/next's answer: what to play after a book.
// Next is set only for a book the caller can open; Book is its indexed metadata
// in the list shape (no files, chapters or description); Work is the community
// work that comes next (source community), with `local` when the caller owns it.
type nextBook struct {
	Source string               `json:"source"`
	Next   *catalog.Ref         `json:"next,omitempty"`
	Book   *catalog.Book        `json:"book,omitempty"`
	Work   *meta.MetaSeriesWork `json:"work,omitempty"`
}

// handleNext answers what to play after the book at ?path= (the `next_book`
// capability), so every player follows a series the same way. Scope and path
// resolution are item's (authorizedScope + bookForPath, the same 400/403/404);
// resolveNext decides.
func (a *API) handleNext(w http.ResponseWriter, r *http.Request) {
	lib, rel, scope, status, msg := a.authorizedScope(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	book, err := a.bookForPath(r.Context(), lib, scope, rel)
	switch {
	case errors.Is(err, library.ErrNotAllowed):
		writeError(w, http.StatusForbidden, msgNoPathAccess)
		return
	case errors.Is(err, library.ErrNotIndexable):
		writeError(w, http.StatusNotFound, "no book at that path")
		return
	case err != nil:
		a.writeCatalogError(w, err, "load book for next failed", "could not load book", "library", lib.ID, "path", rel)
		return
	}
	next, err := a.resolveNext(r.Context(), lib, scope, book)
	if err != nil {
		a.writeCatalogError(w, err, "resolve next book failed", "could not find the next book", "library", lib.ID, "path", rel)
		return
	}
	writeJSON(w, http.StatusOK, next)
}

// resolveNext is the next book after book for the caller (scope is theirs in
// lib), from the first source that has an answer:
//  1. community: the book's community series (communityNext);
//  2. series: its local series numbering (seriesNext);
//  3. folder: the next sibling in its folder (folderNext);
//  4. none.
//
// Whatever it names is in the caller's scope.
func (a *API) resolveNext(ctx context.Context, lib *catalog.Library, scope catalog.Scope, book *catalog.Book) (*nextBook, error) {
	for _, source := range []func() (*nextBook, error){
		func() (*nextBook, error) { return a.communityNext(ctx, lib.ID, book) },
		func() (*nextBook, error) { return a.seriesNext(ctx, lib.ID, scope, book) },
		func() (*nextBook, error) { return a.folderNext(ctx, lib, scope, book) },
	} {
		if next, err := source(); next != nil || err != nil {
			return next, err
		}
	}
	return &nextBook{Source: nextNone}, nil
}

// communityNext reads the next book off the community series: the entry after
// the current work on the first rail's main view (meta.NextOnRail), placed for
// the caller as the /meta envelope places it (localRails). An owned next entry
// is next + book + work; one the caller does not own is the work alone, which
// still ends the lookup - playing a later owned book would skip one. The current
// work last on the rail is the end of the series ({source: community}). nil
// (fall through) when metadata is off, the book is unmatched or has no rails,
// the upstream fails, or the current position is not a number.
func (a *API) communityNext(ctx context.Context, libraryID int64, book *catalog.Book) (*nextBook, error) {
	if !a.metadataOn() || (book.ASIN == "" && book.ISBN == "") {
		return nil, nil
	}
	env, err := a.meta.Enrich(ctx, book.ASIN, book.ISBN)
	if err != nil {
		if !errors.Is(err, meta.ErrNotFound) {
			a.log.Warn("meta lookup for next book failed", "err", err, "library", libraryID, "path", book.RelPath)
		}
		return nil, nil
	}
	if env.Work == nil || len(env.Series) == 0 {
		return nil, nil
	}
	rails, books, err := a.localRails(ctx, catalog.Ref{LibraryID: libraryID, Path: book.RelPath}, env)
	if err != nil {
		return nil, err
	}
	work, ok := meta.NextOnRail(rails[0], env.Work.ID)
	if !ok {
		return nil, nil
	}
	out := &nextBook{Source: nextCommunity, Work: work}
	if work != nil && work.Local != nil {
		ref := catalog.Ref{LibraryID: work.Local.LibraryID, Path: work.Local.Path}
		out.Next = &ref
		if b, ok := books[ref]; ok {
			out.Book = &b
		}
	}
	return out, nil
}

// seriesNext reads the next book off the book's local series numbering: the
// caller's book in the same library and exact series with the smallest higher
// series_index (catalog.NextInSeries). Other numbered books but none later is
// the end of the series ({source: series}). nil (fall through) when the book is
// not numbered in a series, or nothing else in the series is.
func (a *API) seriesNext(ctx context.Context, libraryID int64, scope catalog.Scope, book *catalog.Book) (*nextBook, error) {
	if book.Series == "" || book.SeriesIndex <= 0 {
		return nil, nil
	}
	next, numbered, err := a.cat.NextInSeries(ctx, libraryID, book.RelPath, book.Series, book.SeriesIndex, scope)
	switch {
	case err != nil:
		return nil, err
	case next != nil:
		return &nextBook{Source: nextSeries, Next: &catalog.Ref{LibraryID: libraryID, Path: next.RelPath}, Book: next}, nil
	case numbered:
		return &nextBook{Source: nextSeries}, nil
	}
	return nil, nil
}

// folderPage is how many entries folderNext reads per BrowseFS page (its cap).
const folderPage = 500

// folderNext reads the next book off the book's folder: its parent listed as
// the caller browses it (BrowseFS with their scope and the library's ignore
// rules, paged to the end), the entries they may open annotated with the index
// as /fs annotates them, then library.NextSibling. nil (fall through) when
// nothing follows or the folder cannot be read.
func (a *API) folderNext(ctx context.Context, lib *catalog.Library, scope catalog.Scope, book *catalog.Book) (*nextBook, error) {
	parent := path.Dir(book.RelPath)
	if parent == "." {
		parent = ""
	}
	var allow func(string) bool
	if !scope.AllowAll {
		allow = scope.VisibleInBrowse
	}
	ignore := library.ParseIgnore(lib.IgnorePatterns)
	var entries []library.Entry
	books := map[string]catalog.Book{}
	for offset := 0; ; {
		listing, err := library.BrowseFS(lib.Root, parent, offset, folderPage, allow, ignore)
		if err != nil {
			return nil, nil
		}
		// VisibleInBrowse also shows the folders above a grant, to navigate
		// through; only what the caller may open can be next.
		var page []library.Entry
		var paths []string
		for _, e := range listing.Entries {
			if scope.Allows(e.Path) {
				page = append(page, e)
				paths = append(paths, e.Path)
			}
		}
		indexed, err := a.cat.BooksByPaths(ctx, lib.ID, paths)
		if err != nil {
			return nil, err
		}
		for _, e := range page {
			if b, ok := indexed[e.Path]; ok {
				e.IsBook = true
				books[e.Path] = b
			}
			entries = append(entries, e)
		}
		if listing.NextOffset == 0 {
			break
		}
		offset = listing.NextOffset
	}
	e := library.NextSibling(entries, book.RelPath)
	if e == nil {
		return nil, nil
	}
	out := &nextBook{Source: nextFolder, Next: &catalog.Ref{LibraryID: lib.ID, Path: e.Path}}
	if b, ok := books[e.Path]; ok {
		out.Book = &b
	}
	return out, nil
}
