package api

import (
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Casting (capability cast): a player mints a cast token for one book and hands
// the receiver URLs carrying it instead of its own session token. The token's
// rules are auth's (cast.go); authenticate accepts one only on a book's cover and
// stream (requireBookMediaAuth).

// handleMintCastToken answers POST /cast/token {library_id, path} with 201
// {token, expires_at}: a cast token for the book at path (a part path resolves to
// its book, whose path the token then covers), ending with the caller's own
// token. The caller must be allowed the book (400/403/404 as the media routes
// answer); a podcast episode is 403 not_a_book. Demo accounts may cast (they can play); an API key may not
// (denyAPIKey's containment rule: a key never mints another credential, even a
// cast token, which would die with it). The console's chapter editor, whose
// <audio> streams the book the same way, sends "editor": true for an editor token
// (auth.IssueEditorToken: a shorter life and a cap apart from the person's cast
// tokens, so editing never ends a cast); it allows no more than a cast token, so
// any caller may ask for one.
func (a *API) handleMintCastToken(w http.ResponseWriter, r *http.Request) {
	if denyAPIKey(w, r) {
		return
	}
	var body struct {
		LibraryID int64  `json:"library_id"`
		Path      string `json:"path"`
		Editor    bool   `json:"editor"`
	}
	if err := decodeJSON(r, &body, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	lib, rel, scope, status, msg := a.scopedPath(r, body.LibraryID, body.Path)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	if lib.Kind == catalog.KindPodcasts {
		// Episodes are not cast (a cast token covers a book): 403 as before,
		// with a code a player can branch on.
		writeErrorCode(w, http.StatusForbidden, codeNotABook, "Only books can be cast.")
		return
	}
	book, ok := a.bookAt(w, r, lib, scope, rel, "book not found", "could not load book")
	if !ok {
		return
	}
	u := userFrom(r.Context())
	issue := a.auth.IssueCastToken
	if body.Editor {
		issue = a.auth.IssueEditorToken
	}
	token, expires, err := issue(r.Context(), u.ID, credentialFrom(r.Context()).ID, lib.ID, book.RelPath)
	if err != nil {
		a.log.Warn("mint cast token failed", "user", u.ID, "editor", body.Editor, "err", err)
		writeError(w, http.StatusInternalServerError, "could not start casting")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"token": token, "expires_at": expires})
}
