package api

import (
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Per-user listening state is addressed by (library, path) — the book path —
// matching the path-based identity. Each handler authorizes the path against
// the caller's share scope via authorizedPath.

// userScopeMap returns the caller's effective access scope keyed by library,
// used to filter the cross-library listing endpoints (progress/history) to
// content the caller can still reach — so durable state for a since-revoked
// share isn't read back. Admins get AllowAll for every library.
func (a *API) userScopeMap(r *http.Request, u *auth.User) (map[int64]catalog.Scope, error) {
	scopes, err := a.cat.UserScopes(r.Context(), u.ID, u.Role == auth.RoleAdmin)
	if err != nil {
		return nil, err
	}
	m := make(map[int64]catalog.Scope, len(scopes))
	for _, s := range scopes {
		m[s.LibraryID] = s
	}
	return m, nil
}

// handleListProgress returns the caller's progress rows (for paths they can
// still access), used to seed an offline client's local copy on first sync.
func (a *API) handleListProgress(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	items, err := a.cat.ListProgress(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load progress")
		return
	}
	scopes, err := a.userScopeMap(r, u)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load progress")
		return
	}
	allowed := items[:0]
	for _, it := range items {
		if s, ok := scopes[it.LibraryID]; ok && s.Allows(it.Path) {
			allowed = append(allowed, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"progress": allowed})
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
	writeJSON(w, http.StatusOK, map[string]any{"progress": saved})
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
		writeError(w, http.StatusInternalServerError, "could not save bookmark")
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
		writeError(w, http.StatusInternalServerError, "could not save note")
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

// handleListAllHistory returns the caller's recent listening history across
// books, filtered to paths the caller can still access.
func (a *API) handleListAllHistory(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	items, err := a.cat.ListAllHistory(r.Context(), u.ID, queryInt(r, "limit", 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load history")
		return
	}
	scopes, err := a.userScopeMap(r, u)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load history")
		return
	}
	allowed := items[:0]
	for _, it := range items {
		if s, ok := scopes[it.LibraryID]; ok && s.Allows(it.Path) {
			allowed = append(allowed, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"history": allowed})
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
