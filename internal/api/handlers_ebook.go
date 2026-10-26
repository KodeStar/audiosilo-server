package api

import (
	"fmt"
	"net/http"
	"os"
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
// reader. Only the file the scan recorded for the book is served, joined again
// under the library root and opened fresh: a book with none, a path that names
// anything but a book (a track, the epub itself, a folder above), a podcast, or
// a file gone since the scan is 404; a book outside the caller's scope is 403.
func (a *API) handleEbook(w http.ResponseWriter, r *http.Request) {
	lib, rel, scope, status, msg := a.authorizedScope(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	book, ok := a.ebookBook(w, r, lib, scope, rel, "could not load the ebook")
	if !ok {
		return
	}
	if book.RelPath != rel {
		// The ebook is addressed by its book alone (a part's path is no handle on it).
		writeError(w, http.StatusNotFound, msgNoEbook)
		return
	}
	full, err := library.ContentJoin(lib.Root, book.Ebook.Path)
	if err != nil {
		writeError(w, http.StatusNotFound, msgNoEbook)
		return
	}
	if book.IsFolder {
		// A folder book's ebook is never a link (the rule's), even one swapped in
		// since the scan.
		if info, err := os.Lstat(full); err != nil || !info.Mode().IsRegular() {
			writeError(w, http.StatusNotFound, msgNoEbook)
			return
		}
	}
	f, err := os.Open(full) //nolint:gosec // ContentJoin'd under the library root; the path the scan recorded
	if err != nil {
		writeError(w, http.StatusNotFound, msgNoEbook)
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, msgNoEbook)
		return
	}
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

// handleGetReadingProgress answers the caller's place in the ebook of the book
// at ?path= (a part path reads its book's): {"reading_progress": ReadingProgress
// | null}. 404 when there is no book there, or it has no ebook.
func (a *API) handleGetReadingProgress(w http.ResponseWriter, r *http.Request) {
	lib, rel, scope, status, msg := a.authorizedScope(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	book, ok := a.ebookBook(w, r, lib, scope, rel, "could not load reading progress")
	if !ok {
		return
	}
	p, err := a.cat.GetReadingProgress(r.Context(), userFrom(r.Context()).ID, catalog.Ref{LibraryID: lib.ID, Path: book.RelPath})
	if err != nil {
		a.writeCatalogError(w, err, "get reading progress failed", "could not load reading progress", "library", lib.ID, "path", book.RelPath)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reading_progress": p})
}

// readingBody is PUT /reading-progress's body. base_version is a pointer: absent
// is a save that never diverges (catalog.ResolvePlace), 0 is "saw none".
// device_name is accepted so a client that echoes a stored row's fields isn't
// refused, and never read: the name is the caller's token's.
type readingBody struct {
	CFI         string  `json:"cfi"`
	Progression float64 `json:"progression"`
	DeviceID    string  `json:"device_id"`
	DeviceName  any     `json:"device_name"`
	UpdatedAt   string  `json:"updated_at"`
	BaseVersion *int64  `json:"base_version"`
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
	save := catalog.ReadingSave{CFI: body.CFI, Progression: body.Progression, DeviceID: body.DeviceID,
		UpdatedAt: body.UpdatedAt, BaseVersion: body.BaseVersion,
		DeviceName: credentialFrom(r.Context()).DeviceName}
	// Checked before bookAt, which may read an unindexed book from disk.
	if err := catalog.CheckReadingSave(save); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	book, ok := a.ebookBook(w, r, lib, scope, rel, "could not save reading progress")
	if !ok {
		return
	}
	save.Ref = catalog.Ref{LibraryID: lib.ID, Path: book.RelPath}
	kept, other, err := a.cat.SaveReadingProgress(r.Context(), userFrom(r.Context()).ID, save)
	if err != nil {
		a.writeCatalogError(w, err, "save reading progress failed", "could not save reading progress", "library", lib.ID, "path", book.RelPath)
		return
	}
	out := map[string]any{"reading_progress": kept}
	if other != nil {
		out["other_place"] = other
	}
	writeJSON(w, http.StatusOK, out)
}
