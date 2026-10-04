package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/notify"
)

type ctxKey int

// Every request-context key, in one block so no two can share a value (ipKey
// once collided with the token-kind key, so clientIP returned "session" after
// authentication).
const (
	userKey ctxKey = iota
	credentialKey
	ipKey
)

// userFrom returns the authenticated user from the request context.
func userFrom(ctx context.Context) *auth.User {
	u, _ := ctx.Value(userKey).(*auth.User)
	return u
}

// credentialFrom returns the token that authenticated the request (its id is
// what the admin console calls a device; its Kind, auth.KindSession or
// auth.KindAPI, is what denyAPIKey checks), or the zero Credential when the
// request is unauthenticated. Set by the authenticate middleware.
func credentialFrom(ctx context.Context) auth.Credential {
	c, _ := ctx.Value(credentialKey).(auth.Credential)
	return c
}

// writeJSON writes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// writeError writes a JSON error envelope.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// Machine-readable error codes, added beside "error" for failures a person can
// act on, so a client branches on the code rather than on the English message
// (which stays free to change). Additive: clients that predate them read only
// "error".
const (
	codeUsernameTaken      = "username_taken"
	codeNameTaken          = "name_taken"
	codeLastAdmin          = "last_admin"
	codeAdminNeedsPassword = "admin_needs_password"
	codePasswordTooShort   = "password_too_short"
	codeCannotDeleteSelf   = "cannot_delete_self"
	codeFolderUnreadable   = "folder_unreadable"
	codePathNotAbsolute    = "path_not_absolute"
	codeInvalidOverride    = "invalid_override" // + "field": the field it names
	codeBookNotFound       = "book_not_found"
	codeMetadataOff        = "metadata_off"
	codeUnsupportedImage   = "unsupported_image"
	codeTooLarge           = "too_large"
	codeInvalidSchedule    = "invalid_schedule"
	codeInvalidPattern     = "invalid_pattern"
	codeNotIndexable       = "not_indexable"
	codeCurrentDevice      = "current_device"
	codeInvalidRange       = "invalid_range"
	codeNoAccess           = "no_access"
	codeInvalidSetting     = "invalid_setting"   // + "field": the setting it names
	codeSettingLocked      = "setting_locked"    // + "field": set by the environment or the launcher
	codeUnknownSetting     = "unknown_setting"   // + "field"
	codeSettingReadOnly    = "setting_read_only" // + "field"
	codeUpdateCheckOff     = "update_check_off"
	codeBackupRunning      = "backup_running"
	codeBackupNotFound     = "backup_not_found"
	codeInvalidBackup      = "invalid_backup"
	codeBackupTooNew       = "backup_too_new"
	codeInvalidTarget      = "invalid_target" // + "field": the destination field it names
	codeTooManyTargets     = "too_many_targets"
)

// writeErrorCode writes the error envelope with a machine-readable code.
func writeErrorCode(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// writeCatalogError maps a catalog/library error to an HTTP response. The
// cross-cutting domain sentinels that don't need a handler-specific message get
// a clean, leak-free 4xx; anything else is treated as an unexpected internal
// failure - logged with the supplied op + key/values and returned as a generic
// 500. Centralising this is what keeps every handler's error->status mapping
// exhaustive: a newly added sentinel is handled in one place rather than
// silently falling through to 500 in the handlers that forgot to special-case
// it. Handlers that need a bespoke not-found message (library/share/book) still
// check catalog.ErrNotFound / library.ErrNotIndexable themselves first.
func (a *API) writeCatalogError(w http.ResponseWriter, err error, op, genericMsg string, logKV ...any) {
	var oe *catalog.OverrideError
	var se *config.SettingError
	var te *notify.FieldError
	switch {
	case errors.As(err, &te):
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": te.Error(), "code": codeInvalidTarget, "field": te.Field,
		})
	case errors.Is(err, catalog.ErrTooManyTargets):
		writeErrorCode(w, http.StatusConflict, codeTooManyTargets, "a server can have at most 20 destinations")
	case errors.As(err, &se):
		status, code := http.StatusBadRequest, codeInvalidSetting
		switch se.Reason {
		case config.ReasonLocked:
			status, code = http.StatusConflict, codeSettingLocked
		case config.ReasonUnknown:
			code = codeUnknownSetting
		case config.ReasonReadOnly:
			code = codeSettingReadOnly
		}
		writeJSON(w, status, map[string]string{"error": se.Error(), "code": code, "field": se.Setting})
	case errors.As(err, &oe):
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": oe.Error(), "code": codeInvalidOverride, "field": oe.Field,
		})
	case errors.Is(err, catalog.ErrUnknownSort):
		writeError(w, http.StatusBadRequest, "unknown sort")
	case errors.Is(err, catalog.ErrUnsupportedImage):
		writeErrorCode(w, http.StatusUnsupportedMediaType, codeUnsupportedImage, "the cover must be a JPEG, PNG or WebP image")
	case errors.Is(err, catalog.ErrCoverTooLarge):
		writeErrorCode(w, http.StatusRequestEntityTooLarge, codeTooLarge, "the image is larger than 5 MB")
	case errors.Is(err, catalog.ErrUnknownIssue):
		writeError(w, http.StatusBadRequest, "unknown issue")
	case errors.Is(err, library.ErrInvalidSchedule):
		writeErrorCode(w, http.StatusBadRequest, codeInvalidSchedule, err.Error())
	case errors.Is(err, library.ErrInvalidIgnore):
		writeErrorCode(w, http.StatusBadRequest, codeInvalidPattern, err.Error())
	case errors.Is(err, catalog.ErrNameTaken):
		writeErrorCode(w, http.StatusConflict, codeNameTaken, "name already taken")
	case errors.Is(err, catalog.ErrInvalidCursor):
		writeError(w, http.StatusBadRequest, "invalid cursor")
	case errors.Is(err, catalog.ErrInvalidOverrideMode):
		writeError(w, http.StatusBadRequest, `mode must be "book" or "collection"`)
	case errors.Is(err, library.ErrOutsideRoot):
		writeError(w, http.StatusBadRequest, "invalid path")
	case errors.Is(err, catalog.ErrInvalidProgressEdit):
		writeError(w, http.StatusBadRequest, "those dates or that position don't fit this book")
	case errors.Is(err, catalog.ErrInvalidRange):
		writeErrorCode(w, http.StatusBadRequest, codeInvalidRange, "range must be 7d, 30d, 90d, 1y or a year")
	default:
		a.log.Warn(op, append([]any{"err", err}, logKV...)...)
		writeError(w, http.StatusInternalServerError, genericMsg)
	}
}

// decodeJSON reads a JSON body into v, enforcing a size cap and rejecting
// unknown fields.
func decodeJSON(r *http.Request, v any, maxBytes int64) error {
	if maxBytes <= 0 {
		maxBytes = 1 << 20 // 1 MiB default for control-plane payloads
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBytes))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// decodeJSONOptional is decodeJSON for endpoints whose body is optional: an
// absent/empty body leaves v at its zero value and returns nil, but a body that
// is present and malformed (or carries an unknown field) is still an error -
// optional means omittable, not a silent fall-through to the defaults.
func decodeJSONOptional(r *http.Request, v any, maxBytes int64) error {
	if err := decodeJSON(r, v, maxBytes); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// pathInt parses an int64 path value (e.g. {id}).
func pathInt(r *http.Request, name string) (int64, bool) {
	v := r.PathValue(name)
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// queryInt returns an int query parameter or def when absent/invalid.
func queryInt(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
