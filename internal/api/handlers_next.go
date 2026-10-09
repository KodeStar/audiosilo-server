package api

import (
	"context"
	"errors"
	"net/http"

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
// Source names the step that produced Next (or decided there is none). Next is
// set only for a book the caller can open; Book is its indexed metadata in the
// list shape (no files, chapters or description). Work is the community work
// that comes next (the next entry of the rail that decides, meta.NextRail): with
// `local` beside a community Next, or without it beside a series/folder/none
// answer when the caller's copy could not be placed.
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
	book, ok := a.bookAt(w, r, lib, scope, rel, "no book at that path", "could not load book")
	if !ok {
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
// lib). A community series answers when it places the next entry on one of the
// caller's books (communityNext). Failing to place proves nothing - the books
// may be untagged, or tagged under a series named unlike the rail, and the rail
// can lag the library - so otherwise the local series numbering of every series
// the book is in (seriesNext), then the book's folder (folderNext), then none
// answer, carrying the community's unplaced next work, if it named one, as Work.
// Whatever Next names is in the caller's scope.
func (a *API) resolveNext(ctx context.Context, lib *catalog.Library, scope catalog.Scope, book *catalog.Book) (*nextBook, error) {
	work, placed := a.communityNext(ctx, lib.ID, book)
	if work != nil && work.Local != nil {
		ref := catalog.Ref(*work.Local)
		return &nextBook{Source: nextCommunity, Next: &ref, Book: placed, Work: work}, nil
	}
	next, err := a.seriesNext(ctx, lib.ID, scope, book)
	if next == nil && err == nil {
		next, err = a.folderNext(ctx, lib, scope, book)
	}
	if err != nil {
		return nil, err
	}
	if next == nil {
		next = &nextBook{Source: nextNone}
	}
	next.Work = work
	return next, nil
}

// communityNext is the entry after the current work on the rail that decides
// (meta.NextRail: in the book's own series order, the first rail with a next
// entry that doesn't step back), placed for the caller as the /meta envelope
// places it (localRails). The deciding rail is read off the shared envelope, so
// localRails runs only when there is one. work.Local is set when its entry is
// one of the caller's books, and placed is then that book's indexed metadata
// (when found); a later rail's placed entry never answers instead. work is nil
// when there is no community answer: metadata off, the book unmatched or without
// rails, the upstream failing, or no rail deciding. When the entry can't be
// placed, work is it without `local`, as /meta degrades.
func (a *API) communityNext(ctx context.Context, libraryID int64, book *catalog.Book) (work *meta.MetaSeriesWork, placed *catalog.Book) {
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
	var names []string
	for _, s := range book.AllSeries() {
		names = append(names, s.Name)
	}
	at := meta.NextRail(env.Series, env.Work.ID, names)
	if at < 0 {
		return nil, nil
	}
	rails, books, err := a.localRails(ctx, catalog.Ref{LibraryID: libraryID, Path: book.RelPath}, env)
	if err != nil {
		if ctx.Err() == nil {
			a.log.Warn("place owned books for next book failed", "err", err, "library", libraryID, "path", book.RelPath)
		}
		work, _ = meta.NextOnRail(env.Series[at], env.Work.ID)
		return work, nil
	}
	work, _ = meta.NextOnRail(rails[at], env.Work.ID) // the same entry, with the caller's local
	if work.Local != nil {
		for i := range books {
			if books[i].LibraryID == work.Local.LibraryID && books[i].RelPath == work.Local.Path {
				return work, &books[i]
			}
		}
	}
	return work, nil
}

// seriesNext reads the next book off the local numbering of every series the
// book is in (catalog.NextInSeries). Numbered books but none later is the end of
// the series ({source: series}); nil (fall through) when the book is not
// numbered in a series, or nothing else in its series is.
func (a *API) seriesNext(ctx context.Context, libraryID int64, scope catalog.Scope, book *catalog.Book) (*nextBook, error) {
	next, numbered, err := a.cat.NextInSeries(ctx, libraryID, book, scope)
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

// folderNext reads the next book off the book's folder (library.NextInFolder).
// nil (fall through) when nothing follows.
func (a *API) folderNext(ctx context.Context, lib *catalog.Library, scope catalog.Scope, book *catalog.Book) (*nextBook, error) {
	e, b, err := library.NextInFolder(ctx, a.cat, lib, scope, book.RelPath)
	if e == nil || err != nil {
		return nil, err
	}
	return &nextBook{Source: nextFolder, Next: &catalog.Ref{LibraryID: lib.ID, Path: e.Path}, Book: b}, nil
}
