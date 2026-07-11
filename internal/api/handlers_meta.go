package api

import (
	"errors"
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

// handleMeta resolves a book's asin/isbn against the community metadata API and
// returns a composed enrichment envelope. Transport-only: scope + path resolution
// reuse authorizedPath/bookForPath (exactly like item), the composition and cache
// live in internal/meta.
//
// Responses:
//   - metadata disabled: 404 (clients gate on the `metadata` capability, so they
//     never request this).
//   - book has neither asin nor isbn, or the lookup found no match: 200 {"matched": false}.
//   - upstream unreachable/error: 502.
//   - match: 200 {"matched": true, ...} (see internal/meta.Enrichment).
func (a *API) handleMeta(w http.ResponseWriter, r *http.Request) {
	if !a.metadataOn() {
		writeError(w, http.StatusNotFound, "metadata lookup not enabled")
		return
	}
	lib, path, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	book, err := a.bookForPath(r.Context(), lib, path)
	switch {
	case errors.Is(err, library.ErrNotIndexable):
		writeError(w, http.StatusNotFound, "no book at that path")
		return
	case err != nil:
		a.writeCatalogError(w, err, "load book for meta failed", "could not load book", "library", lib.ID, "path", path)
		return
	}
	if book.ASIN == "" && book.ISBN == "" {
		writeJSON(w, http.StatusOK, map[string]bool{"matched": false})
		return
	}

	env, err := a.meta.Enrich(r.Context(), book.ASIN, book.ISBN)
	switch {
	case errors.Is(err, meta.ErrNotFound):
		writeJSON(w, http.StatusOK, map[string]bool{"matched": false})
	case err != nil:
		a.log.Warn("meta lookup failed", "err", err, "library", lib.ID, "path", path)
		writeError(w, http.StatusBadGateway, "metadata service unavailable")
	default:
		writeJSON(w, http.StatusOK, env)
	}
}
