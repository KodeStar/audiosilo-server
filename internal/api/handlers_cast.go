package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Casting (capability cast): a player mints a cast token for one book and hands
// the receiver URLs carrying it instead of its own session token. The token's
// rules are auth's (auth.CastGrant, ResolveCastToken); castAuthenticate applies
// them to the media routes.

// castTokenBody is POST /cast/token's body.
type castTokenBody struct {
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"`
}

// handleMintCastToken answers POST /cast/token {library_id, path} with 201
// {token, expires_at}: a cast token for the book at path (a part path resolves to
// its book, whose path the token then covers). The caller must be allowed the
// book (400/403/404 as the media routes answer). Demo accounts may cast (they can
// play); an API key may not (a credential minted by a key must not outlive the
// key's revocation: denyAPIKey's containment rule).
func (a *API) handleMintCastToken(w http.ResponseWriter, r *http.Request) {
	if denyAPIKey(w, r) {
		return
	}
	var body castTokenBody
	if err := decodeJSON(r, &body, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	lib, rel, scope, status, msg := a.scopedPath(r, body.LibraryID, body.Path)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	book, ok := a.bookAt(w, r, lib, scope, rel, "book not found", "could not load book")
	if !ok {
		return
	}
	u := userFrom(r.Context())
	token, expires, err := a.auth.IssueCastToken(r.Context(), u.ID, lib.ID, book.RelPath)
	if err != nil {
		a.log.Warn("mint cast token failed", "user", u.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "could not start casting")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"token": token, "expires_at": expires})
}

// castAuthenticate authenticates a book media request (GET
// /libraries/{id}/cover|stream) by a cast token: only when the request carries
// no Authorization header (a cast token is never a bearer credential) and its
// ?token= is a live cast token whose book covers the requested (library, path).
// The person's current scope is checked by the handler, as for any request. It
// returns the request's context carrying the token's user and a KindCast
// credential, or false.
func (a *API) castAuthenticate(r *http.Request) (context.Context, bool) {
	if r.Header.Get("Authorization") != "" {
		return nil, false
	}
	token := strings.TrimSpace(r.URL.Query().Get("token"))
	if token == "" {
		return nil, false
	}
	libID, ok := pathInt(r, "id")
	if !ok {
		return nil, false
	}
	u, grant, err := a.auth.ResolveCastToken(r.Context(), token)
	if err != nil {
		return nil, false
	}
	if !grant.Covers(libID, catalog.CleanRelPath(r.URL.Query().Get("path"))) {
		return nil, false
	}
	ctx := context.WithValue(r.Context(), userKey, u)
	return context.WithValue(ctx, credentialKey, auth.Credential{ID: grant.ID, Kind: auth.KindCast}), true
}

// mediaBucket is the media rate limiter's key for a credential: its token id, or
// for a cast token its own id space (cast_tokens ids would collide with tokens').
func mediaBucket(c auth.Credential) string {
	if c.Kind == auth.KindCast {
		return "cast:" + strconv.FormatInt(c.ID, 10)
	}
	return strconv.FormatInt(c.ID, 10)
}
