package api

import (
	"net/http"
	"strconv"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// The up-next queue (capability "queue") and the list plumbing collections share
// (handlers_collections.go). The list semantics live in catalog (lists.go); these
// handlers authorize, decode and answer. The caller's scopes are read once per
// request and serve both the write and the answer.

// listBodyMax caps a whole-list replace body: up to 1000 refs of long paths.
const listBodyMax = 4 << 20

// listAdd is the body of an add to a list (POST /me/queue, POST
// /me/collections/{id}/items): a book and where to put it (0-based in the stored
// order; absent = the end, or where it already is).
type listAdd struct {
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"`
	Position  *int   `json:"position"`
}

// listReplace is the body of a whole-list replace (PUT /me/queue, PUT
// /me/collections/{id}/items). Items is a pointer so an absent or null key can be
// told from an explicit [] (which clears the list).
type listReplace struct {
	Items *[]catalog.Ref `json:"items"`
}

// decodeListReplace reads a listReplace body, answering 400 itself for a
// malformed one or one without an items array: a whole replace deletes every
// stored entry not listed, so a body that names no list must not read as "empty
// it" (a client's {} or {"items": null} would wipe the list, hidden entries too).
func decodeListReplace(w http.ResponseWriter, r *http.Request) ([]catalog.Ref, bool) {
	var in listReplace
	if err := decodeJSON(r, &in, listBodyMax); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return nil, false
	}
	if in.Items == nil {
		writeError(w, http.StatusBadRequest, "items is required")
		return nil, false
	}
	return *in.Items, true
}

// decodeListAdd reads a listAdd body and resolves its book the way a single add
// does: the path through bookAt (a part or disc path to its book, an unindexed
// one indexed on demand; out of scope 403, not a book 404), answering any failure
// itself. The Ref is the book's own path, which is what the list stores; the
// scopes it checked it with (callerScopes) are returned for the answer.
func (a *API) decodeListAdd(w http.ResponseWriter, r *http.Request) (catalog.Ref, *int, []catalog.Scope, bool) {
	var in listAdd
	if err := decodeJSON(r, &in, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return catalog.Ref{}, nil, nil, false
	}
	if in.LibraryID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return catalog.Ref{}, nil, nil, false
	}
	if in.Position != nil && *in.Position < 0 {
		writeError(w, http.StatusBadRequest, "invalid position")
		return catalog.Ref{}, nil, nil, false
	}
	scopes, ok := a.callerScopes(w, r, "access check failed")
	if !ok {
		return catalog.Ref{}, nil, nil, false
	}
	lib, path, scope, status, msg := a.scopedPathIn(r, scopes, in.LibraryID, in.Path)
	if status != 0 {
		writeError(w, status, msg)
		return catalog.Ref{}, nil, nil, false
	}
	book, ok := a.bookAt(w, r, lib, scope, path, "book not found", "could not add the book")
	if !ok {
		return catalog.Ref{}, nil, nil, false
	}
	return catalog.Ref{LibraryID: lib.ID, Path: book.RelPath}, in.Position, scopes, true
}

// listRemoveRef reads ?library_id=&path= of a remove from a list. There is no
// access check: a remove only touches the caller's own rows, so a revoked path
// can still be cleaned up.
func listRemoveRef(w http.ResponseWriter, r *http.Request) (catalog.Ref, bool) {
	libID, err := strconv.ParseInt(r.URL.Query().Get("library_id"), 10, 64)
	if err != nil || libID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return catalog.Ref{}, false
	}
	path := catalog.CleanRelPath(r.URL.Query().Get("path"))
	if path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return catalog.Ref{}, false
	}
	return catalog.Ref{LibraryID: libID, Path: path}, true
}

// handleGetQueue answers the caller's up-next queue, in order, with only the
// books their current access allows (the writes answer the same).
func (a *API) handleGetQueue(w http.ResponseWriter, r *http.Request) {
	if scopes, ok := a.callerScopes(w, r, "could not load the queue"); ok {
		a.writeQueue(w, r, scopes)
	}
}

// writeQueue answers {"queue": [...]} as the caller (scopes) sees it.
func (a *API) writeQueue(w http.ResponseWriter, r *http.Request, scopes []catalog.Scope) {
	u := userFrom(r.Context())
	items, err := a.cat.Queue(r.Context(), u.ID, scopes)
	if err != nil {
		a.writeCatalogError(w, err, "load queue failed", "could not load the queue", "user", u.ID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queue": items})
}

// handleSetQueue replaces the caller's whole queue (the skip rule: only exactly
// indexed books in their scope; others are left out, not an error) and answers
// the stored result.
func (a *API) handleSetQueue(w http.ResponseWriter, r *http.Request) {
	items, ok := decodeListReplace(w, r)
	if !ok {
		return
	}
	scopes, ok := a.callerScopes(w, r, "could not save the queue")
	if !ok {
		return
	}
	u := userFrom(r.Context())
	if err := a.cat.SetQueue(r.Context(), u.ID, items, scopes); err != nil {
		a.writeCatalogError(w, err, "set queue failed", "could not save the queue", "user", u.ID)
		return
	}
	a.writeQueue(w, r, scopes)
}

// handleAddToQueue queues one book (or moves a queued one to position) and
// answers the queue. A full queue is 409 queue_full (writeCatalogError).
func (a *API) handleAddToQueue(w http.ResponseWriter, r *http.Request) {
	ref, position, scopes, ok := a.decodeListAdd(w, r)
	if !ok {
		return
	}
	u := userFrom(r.Context())
	if err := a.cat.AddToQueue(r.Context(), u.ID, ref, position); err != nil {
		a.writeCatalogError(w, err, "add to queue failed", "could not add the book", "user", u.ID)
		return
	}
	a.writeQueue(w, r, scopes)
}

// handleRemoveFromQueue takes a book off the caller's queue (idempotent).
func (a *API) handleRemoveFromQueue(w http.ResponseWriter, r *http.Request) {
	ref, ok := listRemoveRef(w, r)
	if !ok {
		return
	}
	u := userFrom(r.Context())
	if err := a.cat.RemoveFromQueue(r.Context(), u.ID, ref); err != nil {
		a.writeCatalogError(w, err, "remove from queue failed", "could not remove the book", "user", u.ID)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
