package api

import (
	"context"
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
	a.writeEdit(w, saved, err, "bookmark", id)
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
	if !decodeNoteJSON(w, decodeJSONOptional(r, &in, 0)) {
		return
	}
	scopes, ok := a.callerScopes(w, r, "could not save note")
	if !ok {
		return
	}
	saved, err := a.cat.EditNote(r.Context(), userFrom(r.Context()).ID, id,
		catalog.NoteEdit{Body: in.Body, Position: in.Position}, scopes)
	a.writeEdit(w, saved, err, "note", id)
}

// writeEdit answers an owner's edit of their what {id}: the whole row, 404 "<what>
// not found" for a row that isn't theirs to edit, else writeCatalogError's answer
// (bad input 400, anything else a logged 500).
func (a *API) writeEdit(w http.ResponseWriter, saved any, err error, what string, id int64) {
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		writeError(w, http.StatusNotFound, what+" not found")
	case err != nil:
		a.writeCatalogError(w, err, "edit "+what+" failed", "could not save "+what, what, id)
	default:
		writeJSON(w, http.StatusOK, saved)
	}
}

// decodeNoteJSON answers a note body's decode error (POST and PATCH alike): a
// position of the wrong type ("12", true) is 400 invalid position, anything else
// malformed 400 invalid request. It reports whether the body decoded.
func decodeNoteJSON(w http.ResponseWriter, err error) bool {
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &typeErr) && typeErr.Field == "position":
		writeError(w, http.StatusBadRequest, catalog.ErrInvalidPosition.Error())
	case err != nil:
		writeError(w, http.StatusBadRequest, "invalid request")
	default:
		return true
	}
	return false
}

// pageOptions reads ?limit= and ?cursor= (catalog.PageOptions; an absent or bad
// limit is 0, which the catalog's clamp turns into its default).
func pageOptions(r *http.Request) catalog.PageOptions {
	return catalog.PageOptions{Limit: queryInt(r, "limit", 0), Cursor: r.URL.Query().Get("cursor")}
}

// serveMyPage answers a page of one of the caller's lists across books (list
// reads it within their current access; what names it in an error).
func serveMyPage[P any](a *API, w http.ResponseWriter, r *http.Request, what string,
	list func(ctx context.Context, userID int64, scopes []catalog.Scope, opt catalog.PageOptions) (P, error)) {
	scopes, ok := a.callerScopes(w, r, "could not load "+what)
	if !ok {
		return
	}
	page, err := list(r.Context(), userFrom(r.Context()).ID, scopes, pageOptions(r))
	if err != nil {
		a.writeCatalogError(w, err, "list "+what+" failed", "could not load "+what)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// handleListMyBookmarks answers a page of the caller's bookmarks across every
// library they can still reach, newest first, each with its book when indexed:
// {"bookmarks": [...], "next_cursor"?}. One under a revoked share stays stored
// but is not returned.
func (a *API) handleListMyBookmarks(w http.ResponseWriter, r *http.Request) {
	serveMyPage(a, w, r, "bookmarks", a.cat.ListMyBookmarks)
}

// handleListMyNotes is handleListMyBookmarks for notes: {"notes": [...], "next_cursor"?}.
func (a *API) handleListMyNotes(w http.ResponseWriter, r *http.Request) {
	serveMyPage(a, w, r, "notes", a.cat.ListMyNotes)
}

// handleListAllHistory is handleListMyBookmarks for listening spans, newest
// ended_at first: {"history": [...], "next_cursor"?}. It predates the capability;
// without ?cursor= it answers the first page, as it always has (the rows' book
// and next_cursor are additive).
func (a *API) handleListAllHistory(w http.ResponseWriter, r *http.Request) {
	serveMyPage(a, w, r, "history", a.cat.ListAllHistory)
}
