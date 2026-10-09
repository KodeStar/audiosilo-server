package api

import (
	"fmt"
	"net/http"
)

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

// maxSeriesBatch bounds the distinct series one GET /libraries/{id}/series/books
// answers: a screenful of series cards with room to spare, while one request
// stays a bounded number of indexed queries (one per name).
const maxSeriesBatch = 50

// handleSeriesBooks serves GET /libraries/{id}/series/books?name=A&name=B[&limit=N]
// (the `series_books` capability; the pages are catalog.ListSeriesBooks'). Names
// are verbatim, empty ones ignored and duplicates answered once, in
// first-occurrence order; 400 for no names or more than maxSeriesBatch distinct
// ones. Scope and library errors as /books.
func (a *API) handleSeriesBooks(w http.ResponseWriter, r *http.Request) {
	lib, scope, ok := a.browseScope(w, r)
	if !ok {
		return
	}
	var names []string
	seen := map[string]bool{}
	for _, n := range r.URL.Query()["name"] {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	switch {
	case len(names) == 0:
		writeError(w, http.StatusBadRequest, "at least one series name is required")
		return
	case len(names) > maxSeriesBatch:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d series per request", maxSeriesBatch))
		return
	}
	pages, err := a.cat.ListSeriesBooks(r.Context(), lib.ID, names, queryInt(r, "limit", 50), &scope)
	if err != nil {
		a.writeCatalogError(w, err, "series books failed", "could not load books", "library", lib.ID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"series": pages})
}
