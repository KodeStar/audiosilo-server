package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Annotations (player redesign Phase 4, capability "annotations"): the owner's
// edits of a bookmark or note by id, and the caller's bookmarks, notes and
// listening history across books, a page at a time. Every route is the caller's
// own: another user's id, an unknown one, or one whose book is outside the
// caller's current access is 404 (never 403, which would confirm it exists).
// Transport only; the rules live in catalog/annotations.go.

// handleEditBookmark changes the caller's bookmark {id} ({note?, label?}: an
// omitted field stays) and answers the whole bookmark, as POST does.
func (a *API) handleEditBookmark(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid bookmark id")
		return
	}
	var in struct {
		Note  *string `json:"note"`
		Label *string `json:"label"`
	}
	if err := decodeJSONOptional(r, &in, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	scopes, ok := a.callerScopes(w, r, "could not save bookmark")
	if !ok {
		return
	}
	saved, err := a.cat.EditBookmark(r.Context(), userFrom(r.Context()).ID, id,
		catalog.BookmarkEdit{Note: in.Note, Label: in.Label}, scopes)
	if errors.Is(err, catalog.ErrNotFound) {
		writeError(w, http.StatusNotFound, "bookmark not found")
		return
	}
	if err != nil {
		a.writeCatalogError(w, err, "edit bookmark failed", "could not save bookmark", "bookmark", id)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// handleEditNote changes the caller's note {id} ({body?, position?}: an omitted
// field stays) and answers the whole note, as POST does.
func (a *API) handleEditNote(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid note id")
		return
	}
	var in struct {
		Body     *string  `json:"body"`
		Position *float64 `json:"position"`
	}
	var typeErr *json.UnmarshalTypeError
	switch err := decodeJSONOptional(r, &in, 0); {
	case errors.As(err, &typeErr) && typeErr.Field == "position":
		writeError(w, http.StatusBadRequest, catalog.ErrInvalidPosition.Error()) // "12", true
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	scopes, ok := a.callerScopes(w, r, "could not save note")
	if !ok {
		return
	}
	saved, err := a.cat.EditNote(r.Context(), userFrom(r.Context()).ID, id,
		catalog.NoteEdit{Body: in.Body, Position: in.Position}, scopes)
	if errors.Is(err, catalog.ErrNotFound) {
		writeError(w, http.StatusNotFound, "note not found")
		return
	}
	if err != nil {
		a.writeCatalogError(w, err, "edit note failed", "could not save note", "note", id)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// pageOptions reads ?limit= and ?cursor= (catalog.PageOptions).
func pageOptions(r *http.Request) catalog.PageOptions {
	return catalog.PageOptions{Limit: queryInt(r, "limit", 100), Cursor: r.URL.Query().Get("cursor")}
}

// handleListMyBookmarks answers a page of the caller's bookmarks across every
// library they can still reach, newest first, each with its book when indexed:
// {"bookmarks": [...], "next_cursor"?}. One under a revoked share stays stored
// but is not returned.
func (a *API) handleListMyBookmarks(w http.ResponseWriter, r *http.Request) {
	scopes, ok := a.callerScopes(w, r, "could not load bookmarks")
	if !ok {
		return
	}
	page, err := a.cat.ListMyBookmarks(r.Context(), userFrom(r.Context()).ID, scopes, pageOptions(r))
	if err != nil {
		a.writeCatalogError(w, err, "list bookmarks failed", "could not load bookmarks")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// handleListMyNotes is handleListMyBookmarks for notes: {"notes": [...], "next_cursor"?}.
func (a *API) handleListMyNotes(w http.ResponseWriter, r *http.Request) {
	scopes, ok := a.callerScopes(w, r, "could not load notes")
	if !ok {
		return
	}
	page, err := a.cat.ListMyNotes(r.Context(), userFrom(r.Context()).ID, scopes, pageOptions(r))
	if err != nil {
		a.writeCatalogError(w, err, "list notes failed", "could not load notes")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// handleListAllHistory is handleListMyBookmarks for listening spans, newest
// ended_at first: {"history": [...], "next_cursor"?}. It predates the capability;
// without ?cursor= it answers the first page, as it always has (the rows' book
// and next_cursor are additive).
func (a *API) handleListAllHistory(w http.ResponseWriter, r *http.Request) {
	scopes, ok := a.callerScopes(w, r, "could not load history")
	if !ok {
		return
	}
	page, err := a.cat.ListAllHistory(r.Context(), userFrom(r.Context()).ID, scopes, pageOptions(r))
	if err != nil {
		a.writeCatalogError(w, err, "list history failed", "could not load history")
		return
	}
	writeJSON(w, http.StatusOK, page)
}
