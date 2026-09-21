package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// handleExportLibrary serves a library's book list as a downloadable JSON file
// that the community metadata site (meta.audiosilo.app) can import, so a user can
// mark which entries of a series they own.
//
// Admin-only: it is a whole-library dump, not a scoped view, so it is not gated
// on share path rules - requireAdmin is the gate. The envelope is composed in
// internal/catalog (export.go); this handler is transport-only and streams it
// straight to the response with an encoder rather than buffering a string.
//
// The file carries bibliographic facts only - no paths, sizes, codecs or anything
// else describing the filesystem - because it is meant to leave the server.
func (a *API) handleExportLibrary(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return
	}
	exp, err := a.cat.ExportLibraryBooks(r.Context(), id, Version)
	if errors.Is(err, catalog.ErrNotFound) {
		writeError(w, http.StatusNotFound, "library not found")
		return
	}
	if err != nil {
		a.writeCatalogError(w, err, "export library failed", "could not export library", "library", id)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// strconv.Quote gives the quoted-string form browsers expect (same convention
	// as media.ServeFile's download header).
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(exp.Filename()))
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(exp); err != nil {
		// The status and headers are already out; all that is left is a log line.
		a.log.Warn("export library: write failed", "err", err, "library", id)
	}
}
