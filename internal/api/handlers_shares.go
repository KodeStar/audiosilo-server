package api

import (
	"errors"
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

func (a *API) handleListShares(w http.ResponseWriter, r *http.Request) {
	shares, err := a.cat.ListShares(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list shares")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shares": shares})
}

func (a *API) handleGetShare(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid share id")
		return
	}
	share, err := a.cat.GetShare(r.Context(), id)
	if errors.Is(err, catalog.ErrNotFound) {
		writeError(w, http.StatusNotFound, "share not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load share")
		return
	}
	writeJSON(w, http.StatusOK, share)
}

func (a *API) handleCreateShare(w http.ResponseWriter, r *http.Request) {
	var s catalog.Share
	if err := decodeJSON(r, &s, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if s.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	// CreateShare inserts the share row and any supplied path rules atomically,
	// so a failed rule rolls the whole thing back — no orphaned share to clean up
	// from the transport layer.
	created, err := a.cat.CreateShare(r.Context(), s)
	if err != nil {
		a.writeCatalogError(w, err, "create share failed", "could not create share", "name", s.Name)
		return
	}
	full, _ := a.cat.GetShare(r.Context(), created.ID)
	writeJSON(w, http.StatusCreated, full)
}

func (a *API) handleUpdateShare(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid share id")
		return
	}
	var s catalog.Share
	if err := decodeJSON(r, &s, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	updated, err := a.cat.UpdateShare(r.Context(), id, s)
	if errors.Is(err, catalog.ErrNotFound) {
		writeError(w, http.StatusNotFound, "share not found")
		return
	}
	if err != nil {
		a.writeCatalogError(w, err, "update share failed", "could not update share", "share", id)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (a *API) handleDeleteShare(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid share id")
		return
	}
	if err := a.cat.DeleteShare(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete share")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sharePathReq is the body for adding/removing a path rule.
type sharePathReq struct {
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"` // "" = whole library
}

func (a *API) handleAddSharePath(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid share id")
		return
	}
	var req sharePathReq
	if err := decodeJSON(r, &req, 0); err != nil || req.LibraryID == 0 {
		writeError(w, http.StatusBadRequest, "library_id is required")
		return
	}
	if err := a.cat.AddSharePath(r.Context(), id, catalog.PathRule{LibraryID: req.LibraryID, Path: req.Path}); err != nil {
		writeError(w, http.StatusInternalServerError, "could not add path")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleRemoveSharePath(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid share id")
		return
	}
	var req sharePathReq
	if err := decodeJSON(r, &req, 0); err != nil || req.LibraryID == 0 {
		writeError(w, http.StatusBadRequest, "library_id is required")
		return
	}
	if err := a.cat.RemoveSharePath(r.Context(), id, catalog.PathRule{LibraryID: req.LibraryID, Path: req.Path}); err != nil {
		writeError(w, http.StatusInternalServerError, "could not remove path")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleGrantShare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID  int64 `json:"user_id"`
		ShareID int64 `json:"share_id"`
	}
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := a.cat.GrantShare(r.Context(), req.UserID, req.ShareID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not grant share")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleRevokeShare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID  int64 `json:"user_id"`
		ShareID int64 `json:"share_id"`
	}
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := a.cat.RevokeShare(r.Context(), req.UserID, req.ShareID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not revoke share")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGrantWholeLibrary is convenience sugar: grant a user an entire library
// (creates/grants a whole-library share under the hood).
func (a *API) handleGrantWholeLibrary(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID    int64 `json:"user_id"`
		LibraryID int64 `json:"library_id"`
	}
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := a.cat.GrantWholeLibrary(r.Context(), req.UserID, req.LibraryID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not grant library")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
