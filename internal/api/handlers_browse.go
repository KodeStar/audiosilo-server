package api

import (
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// The player's browse lists (the `browse_people` capability): a library's
// authors, narrators and series with their counts, the same aggregation as the
// admin's /admin/authors|narrators|series (catalog.People/Series) but within the
// caller's scope, so a share-scoped user never sees a count for a book outside
// their grant, and without the admin-only merge suggestions. Transport only.

// handleBrowsePeople serves GET /libraries/{id}/authors and /narrators.
func (a *API) handleBrowsePeople(field, key string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, scope, ok := a.browseScope(w, r)
		if !ok {
			return
		}
		agg, err := a.cat.People(r.Context(), field, id, &scope)
		if err != nil {
			a.writeCatalogError(w, err, "browse people failed", "could not list "+key, "library", id, "field", field)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{key: agg.People, "unknown": agg.Unknown})
	}
}

// handleBrowseSeries serves GET /libraries/{id}/series.
func (a *API) handleBrowseSeries(w http.ResponseWriter, r *http.Request) {
	id, scope, ok := a.browseScope(w, r)
	if !ok {
		return
	}
	series, err := a.cat.Series(r.Context(), id, &scope)
	if err != nil {
		a.writeCatalogError(w, err, "browse series failed", "could not list series", "library", id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"series": series})
}

// browseScope resolves {id} to the library and the caller's scope in it, writing
// the error (400 bad id, 403 no access, 404 unknown library) when it can't.
func (a *API) browseScope(w http.ResponseWriter, r *http.Request) (int64, catalog.Scope, bool) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return 0, catalog.Scope{}, false
	}
	_, scope, status, msg := a.libraryScope(r, id)
	if status != 0 {
		writeError(w, status, msg)
		return 0, catalog.Scope{}, false
	}
	return id, scope, true
}
