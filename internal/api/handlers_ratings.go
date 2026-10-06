package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Ratings (player redesign Phase 1b, capability "ratings"): the caller's own 1-5
// star rating and note on a book, path-keyed like progress. Transport only; the
// rules live in catalog/ratings.go.

// handleGetRating answers the caller's rating of the book at ?path= (exact, like
// progress): {"rating": Rating | null}.
func (a *API) handleGetRating(w http.ResponseWriter, r *http.Request) {
	lib, path, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	rating, err := a.cat.GetRating(r.Context(), userFrom(r.Context()).ID, catalog.Ref{LibraryID: lib.ID, Path: path})
	if err != nil {
		a.writeCatalogError(w, err, "get rating failed", "could not load rating", "library", lib.ID, "path", path)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rating": rating})
}

// handlePutRating rates the book at ?path= ({rating: 1..5, note?}). A part path
// resolves to its book (bookAt: an unindexed book is read on demand, one above the
// path outside the caller's scope is 403, no book is 404) and the rating is stored
// on the book's own path.
func (a *API) handlePutRating(w http.ResponseWriter, r *http.Request) {
	lib, path, scope, status, msg := a.authorizedScope(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	var body struct {
		Rating *int   `json:"rating"`
		Note   string `json:"note"`
	}
	var typeErr *json.UnmarshalTypeError
	switch err := decodeJSON(r, &body, 0); {
	case errors.As(err, &typeErr) && typeErr.Field == "rating", err == nil && body.Rating == nil:
		writeError(w, http.StatusBadRequest, catalog.ErrInvalidRating.Error()) // 4.5, "4", null or absent
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	// Checked before bookAt, which may read an unindexed book from disk.
	if _, err := catalog.CheckRating(*body.Rating, body.Note); err != nil {
		a.writeCatalogError(w, err, "check rating failed", "could not save rating")
		return
	}
	book, ok := a.bookAt(w, r, lib, scope, path, "no book at this path", "could not save rating")
	if !ok {
		return
	}
	saved, err := a.cat.SetRating(r.Context(), userFrom(r.Context()).ID,
		catalog.Ref{LibraryID: lib.ID, Path: book.RelPath}, *body.Rating, body.Note)
	if err != nil {
		a.writeCatalogError(w, err, "save rating failed", "could not save rating", "library", lib.ID, "path", book.RelPath)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rating": saved})
}

// handleDeleteRating clears the caller's rating of the book at ?path= (exact).
// Idempotent: 204 whether or not there was one.
func (a *API) handleDeleteRating(w http.ResponseWriter, r *http.Request) {
	lib, path, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	if err := a.cat.DeleteRating(r.Context(), userFrom(r.Context()).ID, catalog.Ref{LibraryID: lib.ID, Path: path}); err != nil {
		a.writeCatalogError(w, err, "delete rating failed", "could not remove rating", "library", lib.ID, "path", path)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListRatings answers the caller's ratings across every library they can
// still reach, newest first, each with its book when indexed: {"ratings": [...]}.
// A rating under a since-revoked share stays stored but is not returned.
func (a *API) handleListRatings(w http.ResponseWriter, r *http.Request) {
	scopes, ok := a.callerScopes(w, r, "could not load ratings")
	if !ok {
		return
	}
	u := userFrom(r.Context())
	ratings, err := a.cat.ListRatings(r.Context(), u.ID, scopes)
	if err != nil {
		a.writeCatalogError(w, err, "list ratings failed", "could not load ratings")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ratings": ratings})
}
