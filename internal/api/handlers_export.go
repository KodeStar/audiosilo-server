package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// handleExportLibrary serves a library's book list as a downloadable JSON file
// that the community metadata site (meta.audiosilo.app) can import, so a user can
// mark which entries of a series they own.
//
// Admin-only: it is a whole-library dump, not a scoped view, so it is not gated
// on share path rules - requireAdmin is the gate. The envelope is composed in
// internal/catalog (export.go); this handler is transport-only and writes it
// straight to the response with an encoder rather than buffering a string of
// its own.
//
// By default the file carries bibliographic facts only - no paths, sizes, codecs
// or anything else describing the filesystem, nor the owner's own tags - because
// it is meant to leave the server. ?include= takes a comma list for the admin's
// own scripts (catalog.ExportOptions): paths adds each row's paths, tags its
// tags, description its description. ?copies=separate makes one row per indexed
// book instead of merging copies of the same book (merged, the default). Any
// other value of either is a 400, so a typo never silently drops what was asked
// for.
func (a *API) handleExportLibrary(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return
	}
	var opt catalog.ExportOptions
	q := r.URL.Query()
	// Every include= counts (?include=paths&include=tags as much as
	// ?include=paths,tags), so none asked for is dropped.
	for _, include := range q["include"] {
		if include == "" {
			continue
		}
		for v := range strings.SplitSeq(include, ",") {
			switch v {
			case "paths":
				opt.IncludePaths = true
			case "tags":
				opt.IncludeTags = true
			case "description":
				opt.IncludeDescription = true
			default:
				writeError(w, http.StatusBadRequest, `include must be a comma list of "paths", "tags" and "description"`)
				return
			}
		}
	}
	// copies= is one choice: given twice (?copies=merged&copies=separate) it is
	// a 400 too, rather than the first silently winning.
	if len(q["copies"]) > 1 {
		writeError(w, http.StatusBadRequest, `copies must be "merged" or "separate", given once`)
		return
	}
	switch q.Get("copies") {
	case "", "merged":
	case "separate":
		opt.SeparateCopies = true
	default:
		writeError(w, http.StatusBadRequest, `copies must be "merged" or "separate"`)
		return
	}
	exp, err := a.cat.ExportLibraryBooks(r.Context(), id, Version, opt)
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
