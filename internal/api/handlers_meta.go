package api

import (
	"errors"
	"net/http"
	"strings"

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

// handleMetaWork returns one community work document by its metadata-site work
// id - the "catch me up on the previous book" lookup, since the series rails in
// the /libraries/{id}/meta envelope carry sibling work ids but not their
// characters/recaps.
//
// It is deliberately NOT library-scoped: the community metadata is global,
// read-only, already-public data keyed by a work id that carries no information
// about this server's content, so there is no path to authorize (a work id is
// not addressable to a book). Plain auth (any signed-in user, including demo
// and share-scoped accounts, matching the scope-independent parts of the /meta
// route) is the right gate. The id rides in a QUERY param because work ids are
// slugs that may contain characters awkward in a path segment; internal/meta
// URL-escapes it before calling upstream.
//
// Responses:
//   - metadata disabled: 404 (clients gate on the `metadata` capability).
//   - missing/blank id: 400.
//   - malformed id (too long / control characters): 400.
//   - unknown work id upstream: 404.
//   - upstream unreachable/error: 502.
//   - match: 200 {"work": {...}} (see internal/meta.MetaWork).
func (a *API) handleMetaWork(w http.ResponseWriter, r *http.Request) {
	if !a.metadataOn() {
		writeError(w, http.StatusNotFound, "metadata lookup not enabled")
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if !validWorkID(id) {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	work, err := a.meta.Work(r.Context(), id)
	switch {
	case errors.Is(err, meta.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such work")
	case err != nil:
		// Safe to log the id verbatim: validWorkID has already bounded its length
		// and rejected control characters/newlines.
		a.log.Warn("meta work lookup failed", "err", err, "work", id)
		writeError(w, http.StatusBadGateway, "metadata service unavailable")
	default:
		writeJSON(w, http.StatusOK, struct {
			Work *meta.MetaWork `json:"work"`
		}{work})
	}
}

// maxWorkIDLen bounds an accepted work id. Real metadata-site work slugs are
// tens of bytes ("the-martian"); 200 leaves generous headroom while keeping the
// value small enough to be a safe cache key and log field.
const maxWorkIDLen = 200

// validWorkID reports whether a work id is plausible enough to spend a cache
// entry and an outbound upstream GET on. This is transport-level input hygiene,
// not a slug grammar (the id space belongs to the metadata site, so the server
// must stay permissive about its contents): it only rejects the two shapes that
// have a cost here regardless of what upstream would say - an oversized id (Go
// accepts a ~1MB request line, and the id becomes a cache key and a log field)
// and one carrying control characters or newlines (log injection). Everything
// else is passed through and answered by upstream, with a 404 for an unknown id.
func validWorkID(id string) bool {
	if len(id) > maxWorkIDLen {
		return false
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
