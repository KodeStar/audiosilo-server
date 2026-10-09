package api

import "net/http"

// The player's browse lists (the `browse_people` capability): a library's
// authors, narrators and series with their counts, the same aggregation as the
// admin's /admin/authors|narrators|series (catalog.People/Series) but within the
// caller's scope, so a share-scoped user never sees a count for a book outside
// their grant, and without the admin-only merge suggestions. Transport only.

// handleBrowsePeople serves GET /libraries/{id}/authors and /narrators.
func (a *API) handleBrowsePeople(field, key string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lib, scope, ok := a.browseScope(w, r)
		if !ok {
			return
		}
		agg, err := a.cat.People(r.Context(), field, lib.ID, &scope)
		if err != nil {
			a.writeCatalogError(w, err, "browse people failed", "could not list "+key, "library", lib.ID, "field", field)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{key: agg.People, "unknown": agg.Unknown})
	}
}

// handleBrowseSeries serves GET /libraries/{id}/series.
func (a *API) handleBrowseSeries(w http.ResponseWriter, r *http.Request) {
	lib, scope, ok := a.browseScope(w, r)
	if !ok {
		return
	}
	series, err := a.cat.Series(r.Context(), lib.ID, &scope, r.URL.Query().Get("memberships") == "1")
	if err != nil {
		a.writeCatalogError(w, err, "browse series failed", "could not list series", "library", lib.ID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"series": series})
}
