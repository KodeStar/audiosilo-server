package api

import (
	"errors"
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Per-user listening state is addressed by (library, path) - the book path -
// matching the path-based identity. Each handler authorizes the path against
// the caller's share scope via authorizedPath.

// handleListProgress returns the caller's progress rows (for paths they can
// still access), used to seed an offline client's local copy on first sync. The
// access scope is resolved here and the filtering happens in the catalog query,
// so durable state for a since-revoked share isn't read back.
func (a *API) handleListProgress(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	scopes, err := a.cat.UserScopes(r.Context(), u.ID, u.Role == auth.RoleAdmin)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load progress")
		return
	}
	items, err := a.cat.ListProgress(r.Context(), u.ID, scopes)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load progress")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"progress": items})
}

func (a *API) handleGetProgress(w http.ResponseWriter, r *http.Request) {
	lib, path, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	u := userFrom(r.Context())
	p, err := a.cat.GetProgress(r.Context(), u.ID, catalog.Ref{LibraryID: lib.ID, Path: path})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load progress")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"progress": p})
}

// handlePutProgress upserts progress using last-write-wins reconciliation.
func (a *API) handlePutProgress(w http.ResponseWriter, r *http.Request) {
	lib, path, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	var in catalog.Progress
	if err := decodeJSON(r, &in, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	in.Ref = catalog.Ref{LibraryID: lib.ID, Path: path}
	u := userFrom(r.Context())
	saved, err := a.cat.SaveProgress(r.Context(), u.ID, in)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save progress")
		return
	}
	// The save is also the heartbeat of a listening session: what this device
	// played just now, whether or not it won last-write-wins against the stored row.
	a.recordHeartbeat(r, u.ID, in)
	writeJSON(w, http.StatusOK, map[string]any{"progress": saved})
}

// handleEditMyProgress is the caller's own edit of their progress on a book
// (PATCH ?path=): mark it finished or not (unfinished keeps the position), move
// the position, set or clear the start and finish dates. The admin's edit
// (catalog.EditProgress) with the caller's scope: a path outside it is 403, and a
// new row is created only for an indexed book. It is not playback, so it records
// no listening session.
func (a *API) handleEditMyProgress(w http.ResponseWriter, r *http.Request) {
	lib, path, scope, status, msg := a.authorizedScope(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	body := decodeProgressEdit(w, r)
	if body == nil {
		return
	}
	u := userFrom(r.Context())
	saved, err := a.cat.EditProgress(r.Context(), u.ID, catalog.Ref{LibraryID: lib.ID, Path: path}, body.edit(), scope)
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		writeErrorCode(w, http.StatusNotFound, codeBookNotFound, "no progress or book at this path")
	case errors.Is(err, catalog.ErrNoAccess):
		writeError(w, http.StatusForbidden, msgNoPathAccess)
	case err != nil:
		a.writeCatalogError(w, err, "edit own progress failed", "could not save progress", "library", lib.ID, "path", path)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"progress": saved.AsProgress()})
	}
}

func (a *API) handleListBookmarks(w http.ResponseWriter, r *http.Request) {
	lib, path, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	u := userFrom(r.Context())
	items, err := a.cat.ListBookmarks(r.Context(), u.ID, catalog.Ref{LibraryID: lib.ID, Path: path})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load bookmarks")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bookmarks": items})
}

func (a *API) handleAddBookmark(w http.ResponseWriter, r *http.Request) {
	lib, path, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	var bm catalog.Bookmark
	if err := decodeJSON(r, &bm, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	bm.Ref = catalog.Ref{LibraryID: lib.ID, Path: path}
	u := userFrom(r.Context())
	saved, err := a.cat.AddBookmark(r.Context(), u.ID, bm)
	if err != nil {
		a.writeCatalogError(w, err, "add bookmark failed", "could not save bookmark", "library", lib.ID, "path", path)
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

func (a *API) handleDeleteBookmark(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid bookmark id")
		return
	}
	u := userFrom(r.Context())
	if err := a.cat.DeleteBookmark(r.Context(), u.ID, id); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete bookmark")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleListNotes(w http.ResponseWriter, r *http.Request) {
	lib, path, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	u := userFrom(r.Context())
	items, err := a.cat.ListNotes(r.Context(), u.ID, catalog.Ref{LibraryID: lib.ID, Path: path})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load notes")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notes": items})
}

func (a *API) handleAddNote(w http.ResponseWriter, r *http.Request) {
	lib, path, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	var n catalog.Note
	if err := decodeJSON(r, &n, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	n.Ref = catalog.Ref{LibraryID: lib.ID, Path: path}
	u := userFrom(r.Context())
	saved, err := a.cat.AddNote(r.Context(), u.ID, n)
	if err != nil {
		a.writeCatalogError(w, err, "add note failed", "could not save note", "library", lib.ID, "path", path)
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

func (a *API) handleDeleteNote(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid note id")
		return
	}
	u := userFrom(r.Context())
	if err := a.cat.DeleteNote(r.Context(), u.ID, id); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete note")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleListHistory(w http.ResponseWriter, r *http.Request) {
	lib, path, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	u := userFrom(r.Context())
	items, err := a.cat.ListHistory(r.Context(), u.ID,
		catalog.Ref{LibraryID: lib.ID, Path: path}, queryInt(r, "limit", 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load history")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"history": items})
}

// handleAddHistory records a listening span (from/to positions over a time range).
func (a *API) handleAddHistory(w http.ResponseWriter, r *http.Request) {
	lib, path, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	var in struct {
		From      float64 `json:"from_pos"`
		To        float64 `json:"to_pos"`
		StartedAt string  `json:"started_at"`
		EndedAt   string  `json:"ended_at"`
	}
	if err := decodeJSON(r, &in, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	u := userFrom(r.Context())
	if err := a.cat.AddHistory(r.Context(), u.ID,
		catalog.Ref{LibraryID: lib.ID, Path: path}, in.From, in.To, in.StartedAt, in.EndedAt); err != nil {
		writeError(w, http.StatusInternalServerError, "could not save history")
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// handleListFavourites returns the caller's favourites across every library they
// can still reach (for the Favourites shelf + home section), enriched with book
// metadata. The access scope is resolved here and the filtering happens in the
// catalog query, so a since-revoked share's favourites aren't read back.
func (a *API) handleListFavourites(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	scopes, err := a.cat.UserScopes(r.Context(), u.ID, u.Role == auth.RoleAdmin)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load favourites")
		return
	}
	items, err := a.cat.ListAllFavourites(r.Context(), u.ID, scopes)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load favourites")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"favourites": items})
}

// handleAddFavourite hearts a path for the caller (idempotent).
func (a *API) handleAddFavourite(w http.ResponseWriter, r *http.Request) {
	lib, path, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	u := userFrom(r.Context())
	if err := a.cat.AddFavourite(r.Context(), u.ID, catalog.Ref{LibraryID: lib.ID, Path: path}); err != nil {
		writeError(w, http.StatusInternalServerError, "could not save favourite")
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// handleRemoveFavourite clears the caller's favourite for a path (idempotent).
// Keyed by ?path= (one favourite per user+library+path), like progress.
func (a *API) handleRemoveFavourite(w http.ResponseWriter, r *http.Request) {
	lib, path, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	u := userFrom(r.Context())
	if err := a.cat.RemoveFavourite(r.Context(), u.ID, catalog.Ref{LibraryID: lib.ID, Path: path}); err != nil {
		writeError(w, http.StatusInternalServerError, "could not remove favourite")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
