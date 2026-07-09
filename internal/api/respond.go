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
	"github.com/kodestar/audiosilo-server/internal/library"
)

type ctxKey int

const (
	userKey ctxKey = iota
	tokenKindKey
)

// userFrom returns the authenticated user from the request context.
func userFrom(ctx context.Context) *auth.User {
	u, _ := ctx.Value(userKey).(*auth.User)
	return u
}

// tokenKindFrom returns the kind of credential that authenticated the request
// (auth.KindSession or auth.KindAPI), or "" if the request is unauthenticated.
// Set by the authenticate middleware; read by denyAPIKey.
func tokenKindFrom(ctx context.Context) string {
	k, _ := ctx.Value(tokenKindKey).(string)
	return k
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
	switch {
	case errors.Is(err, catalog.ErrNameTaken):
		writeError(w, http.StatusConflict, "name already taken")
	case errors.Is(err, catalog.ErrInvalidCursor):
		writeError(w, http.StatusBadRequest, "invalid cursor")
	case errors.Is(err, catalog.ErrInvalidOverrideMode):
		writeError(w, http.StatusBadRequest, `mode must be "book" or "collection"`)
	case errors.Is(err, library.ErrOutsideRoot):
		writeError(w, http.StatusBadRequest, "invalid path")
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
