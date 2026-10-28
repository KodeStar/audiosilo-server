package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
)

// The reader (Bridge Phase 3b, capability "ebooks"): a book's ebook, served to
// the player that hands it to its reader document, and the reader's place in it.
// Transport only: the scan records the ebook (library.BookEbook's rule,
// books.ebook_path) and the place's conflict rule is catalog.ResolvePlace.

// ebookCSP is sent with the ebook's bytes, so navigating to the URL can run
// nothing and load nothing even if a browser rendered it.
const ebookCSP = "sandbox; default-src 'none'"

// handleEbook serves the ebook of the book at ?path= (exactly the book's own
// path), with Range and validators. It is header-auth only (requireAuth: a
// session or an API key, never ?token=, and there is no URL token for it): the
// web player fetches it with its bearer header and hands the bytes to the
// reader. Only the file the scan recorded for the book is served, opened fresh
// by library.OpenEbook (a regular file inside the library, the very one its path
// leads to; a folder book's never a link): a book with none, a path that names
// anything but a book (a track, the epub itself, a folder above), a podcast, or
// a file gone or changed into anything else since the scan is 404; a book
// outside the caller's scope is 403; a file that can't be read right now is 500,
// logged.
func (a *API) handleEbook(w http.ResponseWriter, r *http.Request) {
	// The bytes outlive the request timeout (isLibraryMedia), but the lookup
	// before them is bounded like any request: a book indexed on demand waits for
	// the one writer, which a stalled write could hold indefinitely.
	lookup, cancel := context.WithTimeout(r.Context(), a.timeoutDur)
	defer cancel()
	lr := r.WithContext(lookup)
	lib, rel, scope, status, msg := a.authorizedScope(lr)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	book, ok := a.ebookBook(w, lr, lib, scope, rel, "could not load the ebook")
	if !ok {
		return
	}
	cancel()
	if book.RelPath != rel {
		// The ebook is addressed by its book alone (a part's path is no handle on it).
		writeError(w, http.StatusNotFound, msgNoEbook)
		return
	}
	f, info, err := library.OpenEbook(lib.Root, book.Ebook.Path, book.IsFolder)
	switch {
	case errors.Is(err, library.ErrNoEbook), errors.Is(err, fs.ErrNotExist):
		writeError(w, http.StatusNotFound, msgNoEbook)
		return
	case err != nil:
		a.log.Warn("open ebook failed", "library", lib.ID, "path", book.Ebook.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "could not load the ebook")
		return
	}
	defer func() { _ = f.Close() }()
	h := w.Header()
	h.Set("Content-Type", "application/epub+zip")
	h.Set("Content-Security-Policy", ebookCSP)
	h.Set("Cache-Control", "private, no-cache")
	h.Set("ETag", fmt.Sprintf(`"%x-%x"`, info.Size(), info.ModTime().UnixNano()))
	http.ServeContent(w, r, path.Base(book.Ebook.Path), info.ModTime(), f)
}

// msgNoEbook is the 404 for a book with no ebook, or one gone since the scan.
const msgNoEbook = "this book has no ebook"

// ebookBook is the book at rel for the reader's routes (bookAt: a part path
// reads its book), refusing a podcast library's and a book with no ebook (404);
// on a failure it writes the answer and returns false.
func (a *API) ebookBook(w http.ResponseWriter, r *http.Request, lib *catalog.Library, scope catalog.Scope, rel, generic string) (*catalog.Book, bool) {
	if lib.Kind == catalog.KindPodcasts {
		writeError(w, http.StatusNotFound, msgNoEbook)
		return nil, false
	}
	book, ok := a.bookAt(w, r, lib, scope, rel, "no book at this path", generic)
	if !ok {
		return nil, false
	}
	if book.Ebook == nil {
		writeError(w, http.StatusNotFound, msgNoEbook)
		return nil, false
	}
	return book, true
}

// readingBook is the path of the book whose reading place rel addresses (a part
// path reads its book), for the reading-progress routes: from the index alone
// (catalog.BookEbookAt), never loading the whole book, and through ebookBook
// (which may index it on demand) only when no indexed book is there. A podcast
// library's, a book with no ebook (404) and one outside scope (403) are refused;
// on a failure it writes the answer and returns false.
func (a *API) readingBook(w http.ResponseWriter, r *http.Request, lib *catalog.Library, scope catalog.Scope, rel, generic string) (string, bool) {
	if lib.Kind == catalog.KindPodcasts {
		writeError(w, http.StatusNotFound, msgNoEbook)
		return "", false
	}
	bookPath, has, err := a.cat.BookEbookAt(r.Context(), lib.ID, rel)
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		book, ok := a.ebookBook(w, r, lib, scope, rel, generic)
		if !ok {
			return "", false
		}
		return book.RelPath, true
	case err != nil:
		a.writeCatalogError(w, err, "load book for path failed", generic, "library", lib.ID, "path", rel)
	case !scope.Allows(bookPath):
		writeError(w, http.StatusForbidden, msgNoPathAccess)
	case !has:
		writeError(w, http.StatusNotFound, msgNoEbook)
	default:
		return bookPath, true
	}
	return "", false
}

// handleGetReadingProgress answers the caller's place in the ebook of the book
// at ?path= (a part path reads its book's): {"reading_progress": ReadingProgress
// | null}. 404 when there is no book there, or it has no ebook.
func (a *API) handleGetReadingProgress(w http.ResponseWriter, r *http.Request) {
	lib, rel, scope, status, msg := a.authorizedScope(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	bookPath, ok := a.readingBook(w, r, lib, scope, rel, "could not load reading progress")
	if !ok {
		return
	}
	p, err := a.cat.GetReadingProgress(r.Context(), userFrom(r.Context()).ID, catalog.Ref{LibraryID: lib.ID, Path: bookPath})
	if err != nil {
		a.writeCatalogError(w, err, "get reading progress failed", "could not load reading progress", "library", lib.ID, "path", bookPath)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reading_progress": p})
}

// readingBody is PUT /reading-progress's body. base_version is a pointer: absent
// is a save that never diverges (catalog.ResolvePlace), 0 is "saw none".
// progression is one too, so an absent or null one is refused rather than read
// as 0 (the place the conflict rule compares). device_name is accepted and
// never read (the name is the caller's token's), so a body carrying one, such as
// an echoed other_place, isn't refused; a whole stored row still is (its
// library_id, path and version are unknown fields).
type readingBody struct {
	CFI         string   `json:"cfi"`
	Progression *float64 `json:"progression"`
	DeviceID    string   `json:"device_id"`
	DeviceName  any      `json:"device_name"`
	UpdatedAt   string   `json:"updated_at"`
	BaseVersion *int64   `json:"base_version"`
}

// handlePutReadingProgress saves the caller's place in the ebook of the book at
// ?path=, on the book's own path (a part path resolves to its book), by Sync
// v2's conflict rule (catalog.SaveReadingProgress): {"reading_progress": the row
// stored after the save, "other_place"?: the place a diverged save did not
// keep}. The device's name is the caller's token's (never the body's). 400 for a
// body CheckReadingSave refuses, 404 for no book or no ebook. No listening
// heartbeat: reading isn't listening.
func (a *API) handlePutReadingProgress(w http.ResponseWriter, r *http.Request) {
	lib, rel, scope, status, msg := a.authorizedScope(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	var body readingBody
	if err := decodeJSON(r, &body, 16<<10); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if body.Progression == nil {
		writeError(w, http.StatusBadRequest, "progression must be from 0 to 1")
		return
	}
	save := catalog.ReadingSave{CFI: body.CFI, Progression: *body.Progression, DeviceID: body.DeviceID,
		UpdatedAt: body.UpdatedAt, BaseVersion: body.BaseVersion,
		DeviceName: credentialFrom(r.Context()).DeviceName}
	// Checked before readingBook, which may read an unindexed book from disk.
	if err := catalog.CheckReadingSave(save); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	bookPath, ok := a.readingBook(w, r, lib, scope, rel, "could not save reading progress")
	if !ok {
		return
	}
	save.Ref = catalog.Ref{LibraryID: lib.ID, Path: bookPath}
	kept, other, err := a.cat.SaveReadingProgress(r.Context(), userFrom(r.Context()).ID, save)
	if err != nil {
		a.writeCatalogError(w, err, "save reading progress failed", "could not save reading progress", "library", lib.ID, "path", bookPath)
		return
	}
	out := map[string]any{"reading_progress": kept}
	if other != nil {
		out["other_place"] = other
	}
	writeJSON(w, http.StatusOK, out)
}
