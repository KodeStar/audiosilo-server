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
// that comes next: with `local` beside a community Next (the entry of the rail
// that placed it), or without it beside a series/folder/none answer when no next
// entry could be placed on the caller's books (the next entry of the first rail,
// in the book's series order, that has one).
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

// communityNext is the entry after the current work on the envelope's rails'
// main views (meta.NextOnRail), placed for the caller as the /meta envelope
// places it (localRails). A book in several series has a rail for each; they are
// followed in the book's own order (meta.RailOrder: the rail of its main series
// first, then of its others in list order, then the rest as listed), and the
// first whose next entry is one of the caller's books answers: work.Local is
// set, and placed is then that book's indexed metadata (when found). work is nil
// when there is no community answer: metadata off, the book unmatched or without
// rails, the upstream failing, or no rail with a next entry (the current
// position not a number, or the current work last). When no next entry can be
// placed, work is the first rail's next entry (in that order) without `local`,
// as /meta degrades.
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
	// The next entries are read off the shared rails first: placing the caller's
	// books moves no entry, so the end of every series and unreadable positions
	// are answered without looking up what the caller owns.
	var names []string
	for _, s := range book.AllSeries() {
		names = append(names, s.Name)
	}
	var withNext []int // the rails with a next entry, in the book's order
	for _, i := range meta.RailOrder(env.Series, names) {
		if w, ok := meta.NextOnRail(env.Series[i], env.Work.ID); ok && w != nil {
			if work == nil {
				work = w
			}
			withNext = append(withNext, i)
		}
	}
	if work == nil {
		return nil, nil
	}
	rails, books, err := a.localRails(ctx, catalog.Ref{LibraryID: libraryID, Path: book.RelPath}, env)
	if err != nil {
		if ctx.Err() == nil {
			a.log.Warn("place owned books for next book failed", "err", err, "library", libraryID, "path", book.RelPath)
		}
		return work, nil
	}
	for _, i := range withNext {
		w, _ := meta.NextOnRail(rails[i], env.Work.ID) // the same entry, with the caller's local
		if w == nil || w.Local == nil {
			continue
		}
		for j := range books {
			if books[j].LibraryID == w.Local.LibraryID && books[j].RelPath == w.Local.Path {
				return w, &books[j]
			}
		}
		return w, nil
	}
	return work, nil
}

// seriesNext reads the next book off the local series numbering of every series
// the book is in (Book.AllSeries: its main series first, then its others in list
// order), skipping those it has no position in: the first series holding a later
// book in scope answers with it (catalog.NextInSeries: the caller's book in the
// same library and exact series with the smallest higher position in it). Other
// numbered books in a series but none later in any is the end of the series
// ({source: series}). nil (fall through) when the book is not numbered in a
// series, or nothing else in its series is.
func (a *API) seriesNext(ctx context.Context, libraryID int64, scope catalog.Scope, book *catalog.Book) (*nextBook, error) {
	ended := false
	for _, s := range book.AllSeries() {
		if s.Position <= 0 {
			continue
		}
		next, numbered, err := a.cat.NextInSeries(ctx, libraryID, book.RelPath, s.Name, s.Position, scope)
		if err != nil {
			return nil, err
		}
		if next != nil {
			return &nextBook{Source: nextSeries, Next: &catalog.Ref{LibraryID: libraryID, Path: next.RelPath}, Book: next}, nil
		}
		ended = ended || numbered
	}
	if ended {
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
