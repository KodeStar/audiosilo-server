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
// lib), from the first source that has an answer: the book's community series
// (communityNext), its local series numbering (seriesNext), the next sibling in
// its folder (folderNext), else none. Whatever it names is in the caller's scope.
func (a *API) resolveNext(ctx context.Context, lib *catalog.Library, scope catalog.Scope, book *catalog.Book) (*nextBook, error) {
	if next, err := a.communityNext(ctx, lib.ID, book); next != nil || err != nil {
		return next, err
	}
	if next, err := a.seriesNext(ctx, lib.ID, scope, book); next != nil || err != nil {
		return next, err
	}
	if next, err := a.folderNext(ctx, lib, scope, book); next != nil || err != nil {
		return next, err
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
		ref := catalog.Ref(*work.Local)
		out.Next = &ref
		for i := range books {
			if books[i].LibraryID == ref.LibraryID && books[i].RelPath == ref.Path {
				out.Book = &books[i]
				break
			}
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

// folderNext reads the next book off the book's folder (library.NextInFolder).
// nil (fall through) when nothing follows.
func (a *API) folderNext(ctx context.Context, lib *catalog.Library, scope catalog.Scope, book *catalog.Book) (*nextBook, error) {
	e, b, err := library.NextInFolder(ctx, a.cat, lib, scope, book.RelPath)
	if e == nil || err != nil {
		return nil, err
	}
	return &nextBook{Source: nextFolder, Next: &catalog.Ref{LibraryID: lib.ID, Path: e.Path}, Book: b}, nil
}
