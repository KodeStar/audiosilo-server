package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/metadata"
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
	addressChargedKey // rateLimit spent this request from its address's bucket
	gzipKey           // compress: the answer may be gzipped and the client takes it
)

// userFrom returns the authenticated user from the request context.
func userFrom(ctx context.Context) *auth.User {
	u, _ := ctx.Value(userKey).(*auth.User)
	return u
}

// credentialFrom returns the token that authenticated the request (its id is
// what the admin console calls a device; its Kind, auth.KindSession or
// auth.KindAPI, is what denyAPIKey checks; auth.KindCast only on a book's cover
// and stream), or the zero Credential when the request is unauthenticated. Set
// by the authenticate middleware (withCredential).
func credentialFrom(ctx context.Context) auth.Credential {
	c, _ := ctx.Value(credentialKey).(auth.Credential)
	return c
}

// withCredential returns ctx carrying the authenticated user and the credential
// that proved it (userFrom, credentialFrom).
func withCredential(ctx context.Context, u *auth.User, cred auth.Credential) context.Context {
	return context.WithValue(context.WithValue(ctx, userKey, u), credentialKey, cred)
}

// writeJSON writes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	jsonHeader(w, status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// writeJSONBytes writes body, an encoded 200 answer, as writeJSON would have.
func writeJSONBytes(w http.ResponseWriter, body []byte) {
	jsonHeader(w, http.StatusOK)
	_, _ = w.Write(body)
}

func jsonHeader(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
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
	codeUsernameTaken            = "username_taken"
	codeUsernameRequired         = "username_required"
	codeCurrentPasswordIncorrect = "current_password_incorrect"
	codeInviteUsed               = "invite_used"    // redeem, exchange: a real invite with no uses left
	codeInviteExpired            = "invite_expired" // redeem, exchange: a real invite past its expiry
	codeNameTaken                = "name_taken"
	codeConfigLibrary            = "config_library" // a library the config declares: renamed, moved or deleted here
	codeLastAdmin                = "last_admin"
	codeAdminNeedsPassword       = "admin_needs_password"
	codeOIDCNoSignIn             = "oidc_no_sign_in"      // POST /auth/oidc/finish without a finished sign-in's cookie
	codeOIDCSignInExpired        = "oidc_sign_in_expired" // POST /auth/oidc/finish: the sign-in's token spent or expired
	codeOIDCIntentMismatch       = "oidc_intent_mismatch" // POST /auth/oidc/finish: the body's "intent" isn't the sign-in's tag (403)
	codePasswordTooShort         = "password_too_short"
	codeCannotDeleteSelf         = "cannot_delete_self"
	codeFolderUnreadable         = "folder_unreadable"
	codePermissionDenied         = "permission_denied" // a folder the server may not use; the message names it, the user and the fix
	codePathNotAbsolute          = "path_not_absolute"
	codeInvalidOverride          = "invalid_override" // + "field": the field it names
	codeBookNotFound             = "book_not_found"
	codeChaptersInvalid          = "chapters_invalid" // + "detail": catalog.ChapterEdit* (PUT .../book/chapters)
	codeChaptersBasis            = "chapters_basis"   // PUT .../book/chapters made on other files than the book's now
	codeMetadataOff              = "metadata_off"
	codeUpcomingOff              = "upcoming_off" // an upcoming route (upcomingOff) while upcoming.enabled is off (metadata on)
	codeNotFound                 = "not_found"    // no such resource (the router's fallback, a library's delete, an admin aggregate's library, GET /me/upcoming/cover)
	codeModuleOff                = "module_off"   // a managed module's route while the module is off (modules.go)
	codeUnsupportedImage         = "unsupported_image"
	codeCoverUnavailable         = "cover_unavailable"
	codeShareNotAnswering        = "share_not_answering" // 503: a cover's or stream's file on a drive or share that stopped answering, or an on-demand read of a book another read of it holds past a look's time (metadata.GaveUp)
	codeStopping                 = "stopping"            // 503: a read the server's stop ended (library.ErrStopping)
	codeTooLarge                 = "too_large"
	codeInvalidSchedule          = "invalid_schedule"
	codeInvalidPattern           = "invalid_pattern"
	codeInvalidMetaSource        = "invalid_metadata_source"
	codeNotIndexable             = "not_indexable"
	codeCurrentDevice            = "current_device"
	codeInvalidRange             = "invalid_range"
	codeNoAccess                 = "no_access"
	codeNotABook                 = "not_a_book"      // POST /cast/token for a podcast episode (403)
	codeInvalidSetting           = "invalid_setting" // + "field": the setting it names
	codeInvalidSetupStep         = "invalid_setup_step"
	codeSettingLocked            = "setting_locked"    // + "field": set by an AUDIOSILO_* variable
	codeUnknownSetting           = "unknown_setting"   // + "field"
	codeSettingReadOnly          = "setting_read_only" // + "field"
	codeUpdateCheckOff           = "update_check_off"
	codeNotMirrorMode            = "not_mirror_mode"
	codeBackupRunning            = "backup_running"
	codeBackupNotFound           = "backup_not_found"
	codeInvalidBackup            = "invalid_backup"
	codeBackupTooNew             = "backup_too_new"
	codeInvalidTarget            = "invalid_target" // + "field", "reason" (notify.Reason*), "max" for a length
	codeTooManyTargets           = "too_many_targets"
	codeQueueFull                = "queue_full"
	codeCollectionFull           = "collection_full"
	codeCollectionsFull          = "collections_full"
	codeNoRegion                 = "no_region"
	codeMatchRunBusy             = "match_run_busy"
	codeReportOff                = "report_off" // an unknown_asin match run while the unknown-ASIN report is off
	codeMatchRunNotReady         = "match_run_not_ready"
	codeMatchRunNotRunning       = "match_run_not_running"
	// Listening imports (handlers_import.go); the ABS failures carry importer.Code*.
	codeInvalidURL       = "invalid_url"
	codeInvalidImport    = "invalid_import"
	codeImportRunning    = "import_running"
	codeImportNotFound   = "import_not_found"
	codeImportNotReady   = "import_not_ready"
	codeImportNotApplied = "import_not_applied"
	codeImportApplied    = "import_applied"
	// Single sign-on (handlers_oidc.go) and an account's email.
	codeInvalidEmail    = "invalid_email"
	codeIdentityLinked  = "identity_linked"   // the sign-in is linked to an account meanwhile
	codeUserHasIdentity = "user_has_identity" // the account has an identity from this provider
	codeNotLinkable     = "not_linkable"      // a household member or demo account
)

// writeErrorCode writes the error envelope with a machine-readable code.
func writeErrorCode(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// writeErrorReason writes the error envelope with a code and the reason that
// narrows it (the console words it as errors.<code>.<reason>).
func writeErrorReason(w http.ResponseWriter, status int, code, reason, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code, "reason": reason})
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
	var ce *catalog.ChapterEditError
	var se *config.SettingError
	var te *notify.FieldError
	switch {
	case errors.As(err, &te):
		body := map[string]any{"error": te.Error(), "code": codeInvalidTarget, "field": te.Field, "reason": te.Reason}
		if te.Max > 0 {
			body["max"] = te.Max
		}
		writeJSON(w, http.StatusBadRequest, body)
	case errors.Is(err, catalog.ErrTooManyTargets):
		writeErrorCode(w, http.StatusConflict, codeTooManyTargets,
			fmt.Sprintf("a server can have at most %d destinations", catalog.MaxNotifyTargets))
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
	case errors.As(err, &ce):
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": codeChaptersInvalid, "code": codeChaptersInvalid, "detail": ce.Detail,
		})
	case errors.Is(err, catalog.ErrChaptersBasis):
		writeErrorCode(w, http.StatusConflict, codeChaptersBasis, codeChaptersBasis)
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
	case errors.Is(err, catalog.ErrNotBookLibrary):
		writeErrorCode(w, 409, "not_book_library", "This action needs a books library.")
	case errors.Is(err, catalog.ErrInvalidKind):
		writeErrorCode(w, 400, "invalid_kind", "Kind must be books or podcasts.")
	case errors.Is(err, catalog.ErrKindImmutable):
		writeErrorCode(w, 400, "kind_immutable", "Library kind cannot be changed.")
	case errors.Is(err, catalog.ErrLibraryGrant):
		// A path edit on a library grant's share (server#254): its folders change
		// by granting or revoking the library on the person's page.
		writeErrorCode(w, http.StatusConflict, "library_grant",
			"this share gives a whole library; grant or revoke the library instead of editing its folders")
	case errors.Is(err, catalog.ErrRootOverlaps):
		writeErrorCode(w, 409, "root_overlaps", "A podcast library root cannot overlap another library.")
	case errors.Is(err, catalog.ErrInvalidMetadataSource):
		writeErrorCode(w, http.StatusBadRequest, codeInvalidMetaSource, err.Error())
	case errors.Is(err, catalog.ErrNameTaken):
		writeErrorCode(w, http.StatusConflict, codeNameTaken, "name already taken")
	case errors.Is(err, catalog.ErrInvalidCursor):
		writeError(w, http.StatusBadRequest, "invalid cursor")
	case errors.Is(err, catalog.ErrInvalidOverrideMode):
		writeError(w, http.StatusBadRequest, `mode must be "book" or "collection"`)
	case errors.Is(err, catalog.ErrInvalidSetupStep):
		writeErrorCode(w, http.StatusBadRequest, codeInvalidSetupStep, err.Error())
	case errors.Is(err, catalog.ErrInvalidSupportChoice):
		writeError(w, http.StatusBadRequest, `action must be "donated" or "snoozed"`)
	case errors.Is(err, library.ErrOutsideRoot):
		writeError(w, http.StatusBadRequest, "invalid path")
	case errors.Is(err, catalog.ErrInvalidProgressEdit):
		writeError(w, http.StatusBadRequest, msgBadProgressEdit)
	case errors.Is(err, catalog.ErrInvalidRange):
		writeErrorCode(w, http.StatusBadRequest, codeInvalidRange, "range must be 7d, 30d, 90d, 1y or a year")
	case errors.Is(err, catalog.ErrInvalidGoal):
		writeError(w, http.StatusBadRequest, fmt.Sprintf("books_per_year must be a whole number from 1 to %d", catalog.MaxBooksPerYear))
	// The player's lists, collections and ratings (Phase 1b).
	case errors.Is(err, catalog.ErrQueueFull):
		writeErrorCode(w, http.StatusConflict, codeQueueFull, fmt.Sprintf("the queue holds at most %d books", catalog.MaxQueue))
	case errors.Is(err, catalog.ErrCollectionFull):
		writeErrorCode(w, http.StatusConflict, codeCollectionFull,
			fmt.Sprintf("a collection holds at most %d books", catalog.MaxCollectionItems))
	case errors.Is(err, catalog.ErrCollectionsFull):
		writeErrorCode(w, http.StatusConflict, codeCollectionsFull,
			fmt.Sprintf("you can have at most %d collections", catalog.MaxCollections))
	case errors.Is(err, catalog.ErrTooManyItems):
		writeError(w, http.StatusBadRequest, "too many items")
	case errors.Is(err, catalog.ErrInvalidName):
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("the name must be 1 to %d characters, with no control characters", catalog.MaxCollectionName))
	case errors.Is(err, catalog.ErrInvalidDescription):
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"the description must be at most %d characters, with no control characters but line breaks and tabs",
			catalog.MaxCollectionDescription))
	case errors.Is(err, catalog.ErrUnknownUser):
		writeError(w, http.StatusBadRequest, "unknown user")
	case errors.Is(err, catalog.ErrTooManyShares):
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("a collection can be shared with at most %d users", catalog.MaxCollectionShares))
	case errors.Is(err, catalog.ErrInvalidRating):
		writeError(w, http.StatusBadRequest, catalog.ErrInvalidRating.Error())
	case errors.Is(err, catalog.ErrRatingNoteTooLong):
		writeError(w, http.StatusBadRequest, catalog.ErrRatingNoteTooLong.Error())
	// Annotations (Phase 4): bookmark and note checks, an edit naming nothing.
	case errors.Is(err, catalog.ErrInvalidLabel), errors.Is(err, catalog.ErrBookmarkNoteTooLong),
		errors.Is(err, catalog.ErrNoteBodyTooLong), errors.Is(err, catalog.ErrInvalidPosition),
		errors.Is(err, catalog.ErrNothingToChange):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, library.ErrStopping):
		// A read of the book the server's stop ended (the request was still
		// waiting for it): try again once the server is back.
		setRetryAfter(w, 10*time.Second)
		writeErrorCode(w, http.StatusServiceUnavailable, codeStopping, "the server is stopping; try again shortly")
	case metadata.GaveUp(err):
		writeShareNotAnswering(w)
	case errors.Is(err, context.DeadlineExceeded):
		// A bounded lookup that ran out of time (a stalled writer) is the
		// timeout middleware's 503, also on the one route it doesn't wrap that
		// bounds its lookup (the ebook's: handleEbook).
		a.log.Warn(op, append([]any{"err", err}, logKV...)...)
		writeError(w, http.StatusServiceUnavailable, "request timed out")
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

// decodeBody decodes a JSON body of at most maxBytes into v (true): a longer
// one answers 400 too_large, so a client tells a body refused for its size
// apart from one that doesn't decode (400 invalid request).
func decodeBody(w http.ResponseWriter, r *http.Request, v any, maxBytes int64) bool {
	err := decodeJSON(r, v, maxBytes)
	if err == nil {
		return true
	}
	if tooBig := (*http.MaxBytesError)(nil); errors.As(err, &tooBig) {
		writeErrorCode(w, http.StatusBadRequest, codeTooLarge,
			fmt.Sprintf("the request is larger than %d bytes", tooBig.Limit))
		return false
	}
	writeError(w, http.StatusBadRequest, "invalid request")
	return false
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

// pathID is pathInt answering 400 msg when the segment isn't a number.
func pathID(w http.ResponseWriter, r *http.Request, name, msg string) (int64, bool) {
	id, ok := pathInt(r, name)
	if !ok {
		writeError(w, http.StatusBadRequest, msg)
	}
	return id, ok
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

// apiNoRoutePattern catches every /api/v1/ path no route claims, under each of
// apiMethods (a pattern with no method would conflict with the web package's
// "GET /").
const apiNoRoutePattern = "/api/v1/"

// apiMethods are the methods the API's routes use (GET covers HEAD).
var apiMethods = []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

// apiNoRoute answers a request no API route claims with the JSON error envelope:
// 405 method_not_allowed with Allow when the path is a route under another
// method (what the mux itself would have said before this catch-all outranked
// it), else 404 not_found.
func apiNoRoute(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var allow []string
		for _, m := range apiMethods {
			if m == r.Method {
				continue
			}
			other := r.Clone(r.Context())
			other.Method = m
			if _, pattern := mux.Handler(other); pattern != "" && pattern != m+" "+apiNoRoutePattern {
				allow = append(allow, m)
			}
		}
		if len(allow) > 0 {
			w.Header().Set("Allow", strings.Join(allow, ", "))
			writeErrorCode(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		writeErrorCode(w, http.StatusNotFound, codeNotFound, "not found")
	})
}

// writeShareNotAnswering is the 503 for a library file a look at was given up
// on (metadata.GaveUp): its drive or share stopped answering, or enough looks
// there hang already that this one wasn't tried.
func writeShareNotAnswering(w http.ResponseWriter) {
	setRetryAfter(w, 30*time.Second)
	writeErrorCode(w, http.StatusServiceUnavailable, codeShareNotAnswering,
		"the drive or share holding this file is not answering; try again later")
}
