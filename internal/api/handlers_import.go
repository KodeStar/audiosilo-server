package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/importer"
)

// Listening imports from Audiobookshelf (admin only): transport only; the
// fetching, matching, planning and applying are internal/importer's, the rows
// catalog's. The ABS token in a request body is handed to the importer and never
// stored, logged or answered back.

// maxImportBody caps an import request (a URL, a token and the user mappings).
const maxImportBody = 64 << 10

// authUsers is the importer's view of the accounts (auth.Service).
type authUsers struct{ auth *auth.Service }

func (u authUsers) ListUsers(ctx context.Context) ([]importer.User, error) {
	all, err := u.auth.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]importer.User, len(all))
	for i, x := range all {
		out[i] = toImporterUser(x)
	}
	return out, nil
}

func toImporterUser(u auth.User) importer.User {
	return importer.User{ID: u.ID, Username: u.Username, Admin: u.Role == auth.RoleAdmin}
}

func (u authUsers) User(ctx context.Context, id int64) (importer.User, error) {
	x, err := u.auth.GetUser(ctx, id)
	if errors.Is(err, auth.ErrNotFound) {
		return importer.User{}, importer.ErrUnknownUser
	}
	if err != nil {
		return importer.User{}, err
	}
	return toImporterUser(*x), nil
}

// writeImportError maps an importer or import error to its response; anything
// else is a logged 500.
func (a *API) writeImportError(w http.ResponseWriter, err error, op, generic string, logKV ...any) {
	var ae *importer.Error
	var ie *importer.InvalidError
	switch {
	case errors.Is(err, importer.ErrInvalidURL):
		writeErrorCode(w, http.StatusBadRequest, codeInvalidURL, err.Error())
	case errors.As(err, &ie):
		writeErrorCode(w, http.StatusBadRequest, codeInvalidImport, ie.Msg)
	case errors.As(err, &ae):
		writeErrorCode(w, http.StatusBadGateway, ae.Code, ae.Msg)
	case errors.Is(err, catalog.ErrImportRunning):
		writeErrorCode(w, http.StatusConflict, codeImportRunning, "that user already has an import in progress")
	case errors.Is(err, catalog.ErrNotFound):
		writeErrorCode(w, http.StatusNotFound, codeImportNotFound, "no such import")
	case errors.Is(err, catalog.ErrImportNotReady):
		writeErrorCode(w, http.StatusConflict, codeImportNotReady, "the import is not waiting for review")
	case errors.Is(err, catalog.ErrImportNotApplied):
		writeErrorCode(w, http.StatusConflict, codeImportNotApplied, "the import is not applied")
	case errors.Is(err, catalog.ErrImportApplied):
		writeErrorCode(w, http.StatusConflict, codeImportApplied, "undo the import before deleting it")
	default:
		a.writeCatalogError(w, err, op, generic, logKV...)
	}
}

// msgBadCutoff is the 400 for a cutoff that isn't one.
const msgBadCutoff = `cutoff must be "auto", null or a date`

// parseImportCutoff reads a present cutoff field: "auto" (the person's first
// listening here), null (none), or a time (optionalTime's: RFC3339, or a
// YYYY-MM-DD day, its start in server time).
func parseImportCutoff(raw json.RawMessage) (importer.Cutoff, bool) {
	if string(raw) == `"auto"` {
		return importer.Cutoff{Auto: true}, true
	}
	var t optionalTime
	if err := json.Unmarshal(raw, &t); err != nil {
		return importer.Cutoff{}, false
	}
	if t.Value == nil {
		return importer.Cutoff{}, true
	}
	return importer.Cutoff{At: *t.Value}, true
}

// handleImportUsers serves POST /admin/imports/abs/users {"url","token"}: checks
// the server is Audiobookshelf and the token works, and lists its users (only the
// token's own with a non-admin token), each with the AudioSilo user of the same
// name. 400 invalid_url | invalid_import (a missing or malformed token); 502
// abs_unreachable | not_abs | abs_unauthorized.
func (a *API) handleImportUsers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := decodeJSON(r, &req, maxImportBody); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	version, users, err := a.imports.Connect(r.Context(), req.URL, req.Token)
	if err != nil {
		var ae *importer.Error
		if errors.As(err, &ae) {
			a.log.Info("import: connecting to Audiobookshelf failed", "code", ae.Code, "err", errors.Unwrap(ae))
		}
		a.writeImportError(w, err, "import users failed", "could not list the Audiobookshelf users")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": version, "users": users})
}

// handleStartImport serves POST /admin/imports/abs {"url","token","users":
// [{"abs_user_id","abs_username"?,"user_id"}],"cutoff"?}: one import per mapped
// user, fetched in the background (poll GET /admin/imports/{id}). abs_username
// (the users list's) names the source while it is fetched, or if it fails; the
// fetch replaces it with ABS's own. cutoff: omitted or "auto" (each person's first
// listening here), null (none), or a time. 202 {imports}; 400 invalid_url |
// invalid_import; 409 import_running.
func (a *API) handleStartImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL   string `json:"url"`
		Token string `json:"token"`
		Users []struct {
			ABSUserID   string `json:"abs_user_id"`
			ABSUsername string `json:"abs_username"`
			UserID      int64  `json:"user_id"`
		} `json:"users"`
		Cutoff json.RawMessage `json:"cutoff"`
	}
	if err := decodeJSON(r, &req, maxImportBody); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	cutoff := importer.Cutoff{Auto: true} // the default
	if len(req.Cutoff) > 0 {
		var ok bool
		if cutoff, ok = parseImportCutoff(req.Cutoff); !ok {
			writeErrorCode(w, http.StatusBadRequest, codeInvalidImport, msgBadCutoff)
			return
		}
	}
	start := importer.StartRequest{URL: req.URL, Token: req.Token, Cutoff: cutoff}
	for _, m := range req.Users {
		start.Users = append(start.Users, importer.Mapping{ABSUserID: m.ABSUserID, ABSUsername: m.ABSUsername,
			UserID: m.UserID})
	}
	imps, err := a.imports.Start(r.Context(), a.baseCtx, start)
	if err != nil {
		a.writeImportError(w, err, "start import failed", "could not start the import")
		return
	}
	for _, imp := range imps {
		a.audit(r, "import.start", imp.Username, map[string]any{"import": imp.ID, "source": imp.Source})
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"imports": imps})
}

// handleListImports serves GET /admin/imports?user_id=: the imports (of one
// user), newest first.
func (a *API) handleListImports(w http.ResponseWriter, r *http.Request) {
	userID, ok := parseOptionalID(r.URL.Query().Get("user_id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid user_id")
		return
	}
	imps, err := a.cat.ListImports(r.Context(), userID)
	if err != nil {
		a.writeCatalogError(w, err, "list imports failed", "could not list the imports")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"imports": imps})
}

// importID reads the {id} path value, answering 400 itself (false then).
func importID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid import id")
	}
	return id, ok
}

// handleGetImport serves GET /admin/imports/{id}: the import with its unmatched
// items. 404 import_not_found.
func (a *API) handleGetImport(w http.ResponseWriter, r *http.Request) {
	id, ok := importID(w, r)
	if !ok {
		return
	}
	d, err := a.cat.GetImport(r.Context(), id)
	if err != nil {
		a.writeImportError(w, err, "get import failed", "could not load the import", "import", id)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// handleUpdateImport serves PATCH /admin/imports/{id} {"cutoff"}: a reviewed
// import's cutoff ("auto", null or a time), planned again from what was fetched.
// 404; 409 import_not_ready unless in review.
func (a *API) handleUpdateImport(w http.ResponseWriter, r *http.Request) {
	id, ok := importID(w, r)
	if !ok {
		return
	}
	var req struct {
		Cutoff json.RawMessage `json:"cutoff"`
	}
	if err := decodeJSON(r, &req, maxImportBody); err != nil || len(req.Cutoff) == 0 {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	cutoff, ok := parseImportCutoff(req.Cutoff)
	if !ok {
		writeErrorCode(w, http.StatusBadRequest, codeInvalidImport, msgBadCutoff)
		return
	}
	d, err := a.imports.SetCutoff(r.Context(), id, cutoff)
	if err != nil {
		a.writeImportError(w, err, "update import failed", "could not update the import", "import", id)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// handleApplyImport serves POST /admin/imports/{id}/apply: writes the import
// (replacing the person's previous one from Audiobookshelf). 404; 409
// import_not_ready unless in review.
func (a *API) handleApplyImport(w http.ResponseWriter, r *http.Request) {
	id, ok := importID(w, r)
	if !ok {
		return
	}
	// One transaction: let it finish even if the request's time runs out.
	imp, err := a.imports.Apply(context.WithoutCancel(r.Context()), id)
	if err != nil {
		a.writeImportError(w, err, "apply import failed", "could not apply the import", "import", id)
		return
	}
	details := map[string]any{"import": imp.ID}
	if imp.Summary != nil {
		details["sessions"], details["listened"] = imp.Summary.Sessions, imp.Summary.Listened
	}
	a.audit(r, "import.apply", imp.Username, details)
	writeJSON(w, http.StatusOK, imp)
}

// handleUndoImport serves POST /admin/imports/{id}/undo: takes an applied import
// back out (progress the person changed since is left alone). 404; 409
// import_not_applied.
func (a *API) handleUndoImport(w http.ResponseWriter, r *http.Request) {
	id, ok := importID(w, r)
	if !ok {
		return
	}
	imp, err := a.cat.UndoImport(r.Context(), id)
	if err != nil {
		a.writeImportError(w, err, "undo import failed", "could not undo the import", "import", id)
		return
	}
	a.audit(r, "import.undo", imp.Username, map[string]any{"import": imp.ID})
	writeJSON(w, http.StatusOK, imp)
}

// handleDeleteImport serves DELETE /admin/imports/{id}: removes an import that
// isn't applied (its record and fetched history). 204; 404; 409 import_applied
// (undo it first) or import_not_ready while it is being applied.
func (a *API) handleDeleteImport(w http.ResponseWriter, r *http.Request) {
	id, ok := importID(w, r)
	if !ok {
		return
	}
	if err := a.cat.DeleteImport(r.Context(), id); err != nil {
		a.writeImportError(w, err, "delete import failed", "could not delete the import", "import", id)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
