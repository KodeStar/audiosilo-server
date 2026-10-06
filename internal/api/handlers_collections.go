package api

import (
	"errors"
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Collections (capability "collections"): a caller's own named, ordered lists of
// books, shareable read-only with named users. Every route is the caller's: a
// collection they neither own nor were shared is 404 (never 403, which would
// confirm it exists); a viewer may read and leave, and gets 403 not_owner on any
// write. Item reads pass through the CALLER's current access. The rules live in
// catalog (collections.go, lists.go).

// collectionID reads {id}, answering 400 when it is not a number.
func collectionID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid collection id")
	}
	return id, ok
}

// codeNotOwner is a viewer's write to a collection shared with them.
const codeNotOwner = "not_owner"

// writeCollectionError maps a collections error: a collection the caller can't
// open 404, a viewer's write 403 not_owner; anything else as writeCatalogError
// (the limits and bad input, else a logged 500).
func (a *API) writeCollectionError(w http.ResponseWriter, err error, op, generic string, logKV ...any) {
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		writeError(w, http.StatusNotFound, "collection not found")
	case errors.Is(err, catalog.ErrNotOwner):
		writeErrorCode(w, http.StatusForbidden, codeNotOwner, "only the collection's owner can change it")
	default:
		a.writeCatalogError(w, err, op, generic, logKV...)
	}
}

// handleListCollections answers the caller's collections: owned first, then
// those shared with them, each newest first.
func (a *API) handleListCollections(w http.ResponseWriter, r *http.Request) {
	scopes, ok := a.callerScopes(w, r, "could not load collections")
	if !ok {
		return
	}
	u := userFrom(r.Context())
	cols, err := a.cat.Collections(r.Context(), u.ID, scopes)
	if err != nil {
		a.writeCollectionError(w, err, "list collections failed", "could not load collections", "user", u.ID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"collections": cols})
}

// handleCreateCollection makes a collection owned by the caller.
func (a *API) handleCreateCollection(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeJSON(r, &in, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	u := userFrom(r.Context())
	col, err := a.cat.CreateCollection(r.Context(), u.ID, in.Name, in.Description)
	if err != nil {
		a.writeCollectionError(w, err, "create collection failed", "could not create the collection", "user", u.ID)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"collection": col})
}

// handleGetCollection answers a collection and its items as the caller sees them.
func (a *API) handleGetCollection(w http.ResponseWriter, r *http.Request) {
	id, ok := collectionID(w, r)
	if !ok {
		return
	}
	if scopes, ok := a.callerScopes(w, r, "could not load the collection"); ok {
		a.writeCollectionDetail(w, r, id, scopes)
	}
}

// writeCollectionDetail answers {collection, items} as GET /me/collections/{id}
// does (the item writes answer the same), as the caller (scopes) sees it.
func (a *API) writeCollectionDetail(w http.ResponseWriter, r *http.Request, id int64, scopes []catalog.Scope) {
	u := userFrom(r.Context())
	col, items, err := a.cat.CollectionDetail(r.Context(), id, u.ID, scopes)
	if err != nil {
		a.writeCollectionError(w, err, "load collection failed", "could not load the collection", "collection", id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"collection": col, "items": items})
}

// writeCollection answers {collection} as the caller sees it.
func (a *API) writeCollection(w http.ResponseWriter, r *http.Request, id int64) {
	scopes, ok := a.callerScopes(w, r, "could not load the collection")
	if !ok {
		return
	}
	u := userFrom(r.Context())
	col, err := a.cat.Collection(r.Context(), id, u.ID, scopes)
	if err != nil {
		a.writeCollectionError(w, err, "load collection failed", "could not load the collection", "collection", id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"collection": col})
}

// handleUpdateCollection renames a collection and/or changes its description
// (owner only).
func (a *API) handleUpdateCollection(w http.ResponseWriter, r *http.Request) {
	id, ok := collectionID(w, r)
	if !ok {
		return
	}
	var in struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if err := decodeJSON(r, &in, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	u := userFrom(r.Context())
	if err := a.cat.UpdateCollection(r.Context(), id, u.ID, in.Name, in.Description); err != nil {
		a.writeCollectionError(w, err, "update collection failed", "could not save the collection", "collection", id)
		return
	}
	a.writeCollection(w, r, id)
}

// handleDeleteCollection deletes the caller's own collection, or takes a viewer
// off one shared with them.
func (a *API) handleDeleteCollection(w http.ResponseWriter, r *http.Request) {
	id, ok := collectionID(w, r)
	if !ok {
		return
	}
	u := userFrom(r.Context())
	if err := a.cat.DeleteCollection(r.Context(), id, u.ID); err != nil {
		a.writeCollectionError(w, err, "delete collection failed", "could not delete the collection", "collection", id)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetCollectionItems replaces a collection's items (owner only; the queue's
// skip rule) and answers the detail.
func (a *API) handleSetCollectionItems(w http.ResponseWriter, r *http.Request) {
	id, ok := collectionID(w, r)
	if !ok {
		return
	}
	items, ok := decodeListReplace(w, r)
	if !ok {
		return
	}
	scopes, ok := a.callerScopes(w, r, "could not save the collection")
	if !ok {
		return
	}
	u := userFrom(r.Context())
	if err := a.cat.SetCollectionItems(r.Context(), id, u.ID, items, scopes); err != nil {
		a.writeCollectionError(w, err, "set collection items failed", "could not save the collection", "collection", id)
		return
	}
	a.writeCollectionDetail(w, r, id, scopes)
}

// handleAddCollectionItem adds one book to a collection (owner only; the queue's
// add semantics; full is 409 collection_full) and answers the detail. Who may
// write is settled before the book is resolved, so a stranger or a viewer
// triggers no read or indexing.
func (a *API) handleAddCollectionItem(w http.ResponseWriter, r *http.Request) {
	id, ok := collectionID(w, r)
	if !ok {
		return
	}
	u := userFrom(r.Context())
	if err := a.cat.RequireCollectionOwner(r.Context(), id, u.ID); err != nil {
		a.writeCollectionError(w, err, "load collection failed", "could not add the book", "collection", id)
		return
	}
	ref, position, scopes, ok := a.decodeListAdd(w, r)
	if !ok {
		return
	}
	if err := a.cat.AddCollectionItem(r.Context(), id, u.ID, ref, position, scopes); err != nil {
		a.writeCollectionError(w, err, "add collection item failed", "could not add the book", "collection", id)
		return
	}
	a.writeCollectionDetail(w, r, id, scopes)
}

// handleRemoveCollectionItem takes a book off a collection (owner only;
// idempotent).
func (a *API) handleRemoveCollectionItem(w http.ResponseWriter, r *http.Request) {
	id, ok := collectionID(w, r)
	if !ok {
		return
	}
	ref, ok := listRemoveRef(w, r)
	if !ok {
		return
	}
	u := userFrom(r.Context())
	if err := a.cat.RemoveCollectionItem(r.Context(), id, u.ID, ref); err != nil {
		a.writeCollectionError(w, err, "remove collection item failed", "could not remove the book", "collection", id)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetCollectionShares replaces who a collection is shared with (owner only;
// never for a demo account).
func (a *API) handleSetCollectionShares(w http.ResponseWriter, r *http.Request) {
	id, ok := collectionID(w, r)
	if !ok || a.nonDemoAccount(w, r) == nil {
		return
	}
	var in struct {
		// A pointer, as listReplace's items: an absent or null list is a bad body,
		// not "unshare everyone" (an explicit [] is that).
		UserIDs *[]int64 `json:"user_ids"`
	}
	if err := decodeJSON(r, &in, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if in.UserIDs == nil {
		writeError(w, http.StatusBadRequest, "user_ids is required")
		return
	}
	u := userFrom(r.Context())
	if err := a.cat.SetCollectionShares(r.Context(), id, u.ID, *in.UserIDs); err != nil {
		a.writeCollectionError(w, err, "set collection shares failed", "could not share the collection", "collection", id)
		return
	}
	a.writeCollection(w, r, id)
}

// handleShareTargets lists who the caller may share a collection with (never for
// a demo account).
func (a *API) handleShareTargets(w http.ResponseWriter, r *http.Request) {
	if a.nonDemoAccount(w, r) == nil {
		return
	}
	u := userFrom(r.Context())
	users, err := a.cat.ShareTargets(r.Context(), u.ID)
	if err != nil {
		a.writeCollectionError(w, err, "list share targets failed", "could not load users", "user", u.ID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}
