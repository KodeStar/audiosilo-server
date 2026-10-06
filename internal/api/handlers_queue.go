package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// The up-next queue (capability "queue") and the list plumbing collections share
// (handlers_collections.go). The list semantics live in catalog (lists.go); these
// handlers authorize, decode and answer.

// Error codes of the queue and collections.
const (
	codeQueueFull       = "queue_full"
	codeCollectionFull  = "collection_full"
	codeCollectionsFull = "collections_full"
	codeNotOwner        = "not_owner"
)

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
// /me/collections/{id}/items).
type listReplace struct {
	Items []catalog.Ref `json:"items"`
}

// callerScopes is the caller's current access, for a cross-library list read; on
// a failure it answers 500 with msg and returns false.
func (a *API) callerScopes(w http.ResponseWriter, r *http.Request, msg string) ([]catalog.Scope, bool) {
	u := userFrom(r.Context())
	scopes, err := a.cat.UserScopes(r.Context(), u.ID, u.Role == auth.RoleAdmin)
	if err != nil {
		a.log.Warn("load user scopes failed", "user", u.ID, "err", err)
		writeError(w, http.StatusInternalServerError, msg)
		return nil, false
	}
	return scopes, true
}

// decodeListAdd reads a listAdd body and resolves its book the way a single add
// does: the path through bookAt (a part or disc path to its book, an unindexed
// one indexed on demand; out of scope 403, not a book 404), answering any failure
// itself. The Ref is the book's own path, which is what the list stores.
func (a *API) decodeListAdd(w http.ResponseWriter, r *http.Request) (catalog.Ref, *int, bool) {
	var in listAdd
	if err := decodeJSON(r, &in, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return catalog.Ref{}, nil, false
	}
	if in.LibraryID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return catalog.Ref{}, nil, false
	}
	if in.Position != nil && *in.Position < 0 {
		writeError(w, http.StatusBadRequest, "invalid position")
		return catalog.Ref{}, nil, false
	}
	lib, path, scope, status, msg := a.scopedPath(r, in.LibraryID, in.Path)
	if status != 0 {
		writeError(w, status, msg)
		return catalog.Ref{}, nil, false
	}
	book, ok := a.bookAt(w, r, lib, scope, path, "book not found", "could not add the book")
	if !ok {
		return catalog.Ref{}, nil, false
	}
	return catalog.Ref{LibraryID: lib.ID, Path: book.RelPath}, in.Position, true
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
	scopes, ok := a.callerScopes(w, r, "could not load the queue")
	if !ok {
		return
	}
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
	var in listReplace
	if err := decodeJSON(r, &in, listBodyMax); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if len(in.Items) > catalog.MaxQueue {
		writeError(w, http.StatusBadRequest, "too many items")
		return
	}
	scopes, ok := a.callerScopes(w, r, "could not save the queue")
	if !ok {
		return
	}
	u := userFrom(r.Context())
	if err := a.cat.SetQueue(r.Context(), u.ID, in.Items, scopes); err != nil {
		a.writeListError(w, err, "set queue failed", "could not save the queue", "user", u.ID)
		return
	}
	a.handleGetQueue(w, r)
}

// handleAddToQueue queues one book (or moves a queued one to position) and
// answers the queue.
func (a *API) handleAddToQueue(w http.ResponseWriter, r *http.Request) {
	ref, position, ok := a.decodeListAdd(w, r)
	if !ok {
		return
	}
	u := userFrom(r.Context())
	err := a.cat.AddToQueue(r.Context(), u.ID, ref, position)
	if errors.Is(err, catalog.ErrListFull) {
		writeErrorCode(w, http.StatusConflict, codeQueueFull,
			"the queue holds at most "+strconv.Itoa(catalog.MaxQueue)+" books")
		return
	}
	if err != nil {
		a.writeListError(w, err, "add to queue failed", "could not add the book", "user", u.ID)
		return
	}
	a.handleGetQueue(w, r)
}

// handleRemoveFromQueue takes a book off the caller's queue (idempotent).
func (a *API) handleRemoveFromQueue(w http.ResponseWriter, r *http.Request) {
	ref, ok := listRemoveRef(w, r)
	if !ok {
		return
	}
	u := userFrom(r.Context())
	if err := a.cat.RemoveFromQueue(r.Context(), u.ID, ref); err != nil {
		a.writeListError(w, err, "remove from queue failed", "could not remove the book", "user", u.ID)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeListError maps the errors of the queue and collections: a collection the
// caller can't open 404, a viewer's write 403 not_owner, a create over the cap
// 409 collections_full, bad input 400; anything else as writeCatalogError does
// (a logged 500). A full list is answered by the caller, whose code it names.
func (a *API) writeListError(w http.ResponseWriter, err error, op, generic string, logKV ...any) {
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		writeError(w, http.StatusNotFound, "collection not found")
	case errors.Is(err, catalog.ErrNotOwner):
		writeErrorCode(w, http.StatusForbidden, codeNotOwner, "only the collection's owner can change it")
	case errors.Is(err, catalog.ErrCollectionsFull):
		writeErrorCode(w, http.StatusConflict, codeCollectionsFull,
			"you can have at most "+strconv.Itoa(catalog.MaxCollections)+" collections")
	case errors.Is(err, catalog.ErrTooManyItems):
		writeError(w, http.StatusBadRequest, "too many items")
	case errors.Is(err, catalog.ErrInvalidName):
		writeError(w, http.StatusBadRequest, "the name must be 1 to 100 characters, with no control characters")
	case errors.Is(err, catalog.ErrInvalidDescription):
		writeError(w, http.StatusBadRequest, "the description must be at most 1000 characters, with no control characters but line breaks and tabs")
	case errors.Is(err, catalog.ErrUnknownUser):
		writeError(w, http.StatusBadRequest, "unknown user")
	case errors.Is(err, catalog.ErrTooManyShares):
		writeError(w, http.StatusBadRequest,
			"a collection can be shared with at most "+strconv.Itoa(catalog.MaxCollectionShares)+" users")
	default:
		a.writeCatalogError(w, err, op, generic, logKV...)
	}
}
