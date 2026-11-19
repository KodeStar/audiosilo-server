package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/web/spa"
)

// callerForUser builds access for a user known outside this request: the id, role and
// a household member's rule, which u carries from its token lookup (or GetUser),
// so the scope reads cost no statement for it.
func callerForUser(u *auth.User) catalog.Caller {
	who := catalog.Caller{ID: u.ID, Admin: u.Role == auth.RoleAdmin}
	if r := u.MemberRule; r != nil {
		who.Member = &catalog.MemberRule{Owner: r.Owner, Access: r.Access, OwnerAdmin: r.OwnerAdmin}
	}
	return who
}

// callerOf applies this request's credential to its user, including the stored
// client on headerless media requests. Cast credentials never see podcasts.
func callerOf(ctx context.Context) catalog.Caller {
	who := callerForUser(userFrom(ctx))
	who.Podcasts = seesPodcasts(ctx)
	return who
}

// seesPodcasts is the old-client gate for this request's credential: whether
// what it is shown may include podcast libraries, its own or (in the console's
// views of a user) another person's.
func seesPodcasts(ctx context.Context) bool {
	cred := credentialFrom(ctx)
	return cred.Kind != auth.KindCast && auth.SeesPodcasts(cred.Client)
}

// handleListLibraries lists libraries the caller can reach (via any share, or
// for a household member through their owner: the member rule). A podcasts
// library is listed only while the podcasts module is on (server#179): turned
// off, the player shows none, as an old client never sees one; what's in it
// waits for the module to come back.
func (a *API) handleListLibraries(w http.ResponseWriter, r *http.Request) {
	who := callerOf(r.Context())
	who.Podcasts = who.Podcasts && a.moduleOn(bridge.ModulePodcasts)
	libs, err := a.cat.CallerLibraries(r.Context(), who)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list libraries")
		return
	}
	if u := userFrom(r.Context()); u.Role == auth.RoleAdmin && !u.IsDemo {
		writeJSON(w, http.StatusOK, map[string]any{"libraries": libs})
		return
	}
	// A listener (a demo visitor among them) is never told where the library
	// lives on the server's disk; an admin is, here and on the admin routes.
	out := make([]listenerLibrary, len(libs))
	for i, l := range libs {
		out[i] = listenerLibrary{Library: l}
	}
	writeJSON(w, http.StatusOK, map[string]any{"libraries": out})
}

// listenerLibrary is a library on GET /libraries for a caller who isn't an
// admin: catalog.Library without its root (the shallower, always-empty Root
// shadows the embedded one, and omitempty drops it).
type listenerLibrary struct {
	catalog.Library
	Root string `json:"root,omitempty"`
}

// libraryScope loads a library and the caller's effective access scope for it.
// A non-admin with no access there (no granting share, and for a household
// member nothing through their owner) gets 403.
func (a *API) libraryScope(r *http.Request, libraryID int64) (*catalog.Library, catalog.Scope, int, string) {
	scope, err := a.cat.CallerScope(r.Context(), callerOf(r.Context()), libraryID)
	if err != nil {
		return nil, scope, http.StatusInternalServerError, "access check failed"
	}
	return a.scopedLibrary(r, scope)
}

// callerScopes is the caller's current access in every library (CallerScopes), for
// a cross-library read or a request that reads more than one library; on a
// failure it answers 500 with msg and returns false.
func (a *API) callerScopes(w http.ResponseWriter, r *http.Request, msg string) ([]catalog.Scope, bool) {
	u := userFrom(r.Context())
	scopes, err := a.cat.CallerScopes(r.Context(), callerOf(r.Context()))
	if err != nil {
		a.log.Warn("load user scopes failed", "user", u.ID, "err", err)
		writeError(w, http.StatusInternalServerError, msg)
		return nil, false
	}
	return scopes, true
}

// gatedScopes is the caller's scopes for a whole-list replace
// (catalog.GatedScopes): what they may list, and keep, the podcast entries a
// replace from a player that can't see podcasts keeps (nil for a caller that
// sees them).
func (a *API) gatedScopes(w http.ResponseWriter, r *http.Request, msg string) (scopes, keep []catalog.Scope, ok bool) {
	scopes, keep, err := a.cat.GatedScopes(r.Context(), callerOf(r.Context()))
	if err != nil {
		a.log.Warn("load user scopes failed", "user", userFrom(r.Context()).ID, "err", err)
		writeError(w, http.StatusInternalServerError, msg)
		return nil, nil, false
	}
	return scopes, keep, true
}

// scopeIn is the caller's scope in one library out of their callerScopes, as the
// catalog computed it (the client gate and private shows apply to admins too),
// and whether the library was among them.
func scopeIn(scopes []catalog.Scope, libraryID int64) (catalog.Scope, bool) {
	for _, s := range scopes {
		if s.LibraryID == libraryID {
			return s, true
		}
	}
	return catalog.Scope{LibraryID: libraryID}, false
}

// scopedLibrary loads the library the caller's scope is for: 403 without a
// library grant, 404 for no such library. Private podcast filtering can leave a
// granted library with no visible content; its empty listings remain readable.
func (a *API) scopedLibrary(r *http.Request, scope catalog.Scope) (*catalog.Library, catalog.Scope, int, string) {
	if !scope.AllowAll && len(scope.Paths) == 0 {
		lib, err := a.grantedLibrary(r, scope.LibraryID)
		if err != nil {
			return nil, scope, http.StatusInternalServerError, "access check failed"
		}
		if lib != nil && lib.Kind == catalog.KindPodcasts {
			return lib, scope, 0, ""
		}
		return nil, scope, http.StatusForbidden, "no access to this library"
	}
	lib, err := a.cat.GetLibrary(r.Context(), scope.LibraryID)
	if errors.Is(err, catalog.ErrNotFound) {
		return nil, scope, http.StatusNotFound, "library not found"
	}
	if err != nil {
		return nil, scope, http.StatusInternalServerError, "could not load library"
	}
	return lib, scope, 0, ""
}

// grantedLibrary is library id when the caller holds a grant on it, judged
// before private-show narrowing (nil without one).
func (a *API) grantedLibrary(r *http.Request, id int64) (*catalog.Library, error) {
	libs, err := a.cat.CallerLibraries(r.Context(), callerOf(r.Context()))
	if err != nil {
		return nil, err
	}
	for i := range libs {
		if libs[i].ID == id {
			return &libs[i], nil
		}
	}
	return nil, nil
}

// browseScope resolves {id} to the library and the caller's scope in it, writing
// the error (400 bad id, 403 no access, 404 unknown library) when it can't.
func (a *API) browseScope(w http.ResponseWriter, r *http.Request) (*catalog.Library, catalog.Scope, bool) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return nil, catalog.Scope{}, false
	}
	lib, scope, status, msg := a.libraryScope(r, id)
	if status != 0 {
		writeError(w, status, msg)
		return nil, catalog.Scope{}, false
	}
	return lib, scope, true
}

// browseList is browseScope for a list answered through the list cache
// (listcache.go): it reads the content generation before any read, then answers a
// cache hit itself (write nil, as it does an error). Only a books library's
// answers are cached.
func (a *API) browseList(w http.ResponseWriter, r *http.Request) (*catalog.Library, catalog.Scope, func(any)) {
	gen, cacheable := a.cat.ContentGeneration()
	lib, scope, ok := a.browseScope(w, r)
	if !ok {
		return nil, scope, nil
	}
	return lib, scope, a.cachedList(w, r, gen, cacheable && lib.Kind == catalog.KindBooks, []catalog.Scope{scope})
}

// authorizedPath resolves {id} + ?path=, checks the path is within the caller's
// scope, and returns the library + path. Used by every path-addressed endpoint.
func (a *API) authorizedPath(r *http.Request) (*catalog.Library, string, int, string) {
	lib, rel, _, status, msg := a.authorizedScope(r)
	return lib, rel, status, msg
}

// exactPath is authorizedPath taking ?path= in the spelling given (pathInScope,
// no catalog.Spelling): an admin's edit of a person's stored row, which the
// console names by the row's own path, so a row an older server stored in
// another spelling is the one edited, not the index spelling's.
func (a *API) exactPath(r *http.Request) (*catalog.Library, string, int, string) {
	id, ok := pathInt(r, "id")
	if !ok {
		return nil, "", http.StatusBadRequest, "invalid library id"
	}
	raw := r.URL.Query().Get("path")
	if len(raw) > catalog.MaxPathBytes {
		return nil, "", http.StatusBadRequest, msgPathTooLong
	}
	lib, scope, status, msg := a.libraryScope(r, id)
	if status != 0 {
		return nil, "", status, msg
	}
	lib, rel, _, status, msg := pathInScope(lib, scope, raw)
	return lib, rel, status, msg
}

// authorizedScope is authorizedPath plus the caller's scope, for an endpoint that
// resolves the path to a book (bookForPath), which may lie above it.
func (a *API) authorizedScope(r *http.Request) (*catalog.Library, string, catalog.Scope, int, string) {
	id, ok := pathInt(r, "id")
	if !ok {
		return nil, "", catalog.Scope{}, http.StatusBadRequest, "invalid library id"
	}
	return a.scopedPath(r, id, r.URL.Query().Get("path"))
}

// scopedPath is authorizedScope for a library id and a raw path the caller got
// from elsewhere.
func (a *API) scopedPath(r *http.Request, libraryID int64, rawPath string) (*catalog.Library, string, catalog.Scope, int, string) {
	lib, rel, scope, _, status, msg := a.scopedFile(r, libraryID, rawPath)
	return lib, rel, scope, status, msg
}

// scopedFile is scopedPath with refused (spelledPath): for the stream, the one
// route that opens the file at the path itself.
func (a *API) scopedFile(r *http.Request, libraryID int64, rawPath string) (*catalog.Library, string, catalog.Scope, bool, int, string) {
	// Over the cap is 400 whatever the library (server#291): before the
	// library is looked up, so an unknown or ungranted one doesn't answer
	// 404/403 for it.
	if len(rawPath) > catalog.MaxPathBytes {
		return nil, "", catalog.Scope{}, false, http.StatusBadRequest, msgPathTooLong
	}
	lib, scope, status, msg := a.libraryScope(r, libraryID)
	if status != 0 {
		return nil, "", scope, false, status, msg
	}
	return a.spelledPath(r.Context(), lib, scope, rawPath)
}

// scopedPathIn is scopedPath with the caller's scopes already loaded
// (callerScopes): a request body's book (POST /me/queue,
// /me/collections/{id}/items) whose answer reads the caller's lists with them.
func (a *API) scopedPathIn(r *http.Request, scopes []catalog.Scope, libraryID int64, rawPath string) (*catalog.Library, string, catalog.Scope, int, string) {
	// Over the cap first, as scopedPath: scopedLibrary may refuse first.
	if len(rawPath) > catalog.MaxPathBytes {
		return nil, "", catalog.Scope{}, http.StatusBadRequest, msgPathTooLong
	}
	scope, ok := scopeIn(scopes, libraryID)
	if !ok {
		// None granted, hidden by the client gate, or no such library: scopedPath
		// tells them apart as for any path (an admin's unknown library is a 404).
		return a.scopedPath(r, libraryID, rawPath)
	}
	lib, scope, status, msg := a.scopedLibrary(r, scope)
	if status != 0 {
		return nil, "", scope, status, msg
	}
	return a.spelledPathInScope(r.Context(), lib, scope, rawPath)
}

// msgPathTooLong is the 400 for a path over catalog.MaxPathBytes, on every
// path-addressed route (server#289, #291).
const msgPathTooLong = "path is too long"

// pathTooLong answers 400 msgPathTooLong for a raw path no file can have
// (true), before it is cleaned, looked up (a library, a book's ancestors) or
// stored: a route that takes a path outside pathInScope (an admin's folder
// override, browse, a list's remove, the sessions and Bridge jobs filters, a
// Bridge job or estimate, the Bridge's book read, rescan and enrichment)
// checks it first, before the library is looked up.
func pathTooLong(w http.ResponseWriter, rawPath string) bool {
	if len(rawPath) <= catalog.MaxPathBytes {
		return false
	}
	writeError(w, http.StatusBadRequest, msgPathTooLong)
	return true
}

// pathInScope cleans rawPath and checks scope allows it (400 empty or over
// catalog.MaxPathBytes, 403 outside, 404 through a hidden folder), taking the
// path in the spelling given: the Bridge's routes, which send the index's own
// paths. A player's route takes it through spelledPathInScope.
func pathInScope(lib *catalog.Library, scope catalog.Scope, rawPath string) (*catalog.Library, string, catalog.Scope, int, string) {
	rel, status, msg := cleanedPath(rawPath)
	if status != 0 {
		return nil, "", scope, status, msg
	}
	return relInScope(lib, scope, rel)
}

// spelledPathInScope is pathInScope with the cleaned path in the index's
// spelling (spelledIn: another Unicode spelling of an indexed path the caller's
// scope allows reads as the index's), before the scope and hidden checks, so
// those judge the path that is read. A failed lookup is a 500 (a timed-out one
// a 503), never the path as given.
func (a *API) spelledPathInScope(ctx context.Context, lib *catalog.Library, scope catalog.Scope, rawPath string) (*catalog.Library, string, catalog.Scope, int, string) {
	lib, rel, scope, _, status, msg := a.spelledPath(ctx, lib, scope, rawPath)
	return lib, rel, scope, status, msg
}

// spelledPath is spelledPathInScope with refused: the path, which the caller's
// scope admits as given (a rule written in that spelling), names only indexed
// paths the scope refuses (spelledIn's missing). It is the path as given, as
// for a path the index has nothing at: every route answers it as such (the
// catalog's from the index, which the on-demand index reads only in the
// spelling on disk), so nothing in the answer tells the caller the refused
// book is there. Only the stream opens the file at the path itself, which a
// disk that ignores the difference (macOS, SMB) would find in the refused
// book: it answers as for no file there (audioFile).
func (a *API) spelledPath(ctx context.Context, lib *catalog.Library, scope catalog.Scope, rawPath string) (*catalog.Library, string, catalog.Scope, bool, int, string) {
	rel, status, msg := cleanedPath(rawPath)
	if status != 0 {
		return nil, "", scope, false, status, msg
	}
	rel, refused, err := a.spelledIn(ctx, lib.ID, scope.Allows, rel)
	if err != nil {
		a.log.Warn("resolve path spelling failed", "library", lib.ID, "err", err)
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, "", scope, false, http.StatusServiceUnavailable, "request timed out"
		}
		return nil, "", scope, false, http.StatusInternalServerError, "could not resolve the path"
	}
	lib, rel, scope, status, msg = relInScope(lib, scope, rel)
	return lib, rel, scope, refused && status == 0, status, msg
}

// spelledIn is rel in the index's spelling (catalog.Spelling) among the
// spellings allow (the caller's scope) admits, else rel as given: a spelling
// outside the caller's access is never taken, nor counted, so the answer (a 403
// where rel would read as missing, or a third spelling's) can't tell them an
// indexed path they can't reach is there. missing reports that rel, which allow
// admits as given (a rule written in that spelling), names only indexed paths
// it refuses: the caller answers as for a path with nothing there, and never
// opens rel on a disk that ignores the difference (macOS, SMB), where it would
// reach the refused book's files (spelledPath, /fs).
func (a *API) spelledIn(ctx context.Context, libraryID int64, allow func(string) bool, rel string) (spelled string, missing bool, err error) {
	spelled, outside, err := a.cat.Spelling(ctx, libraryID, rel, allow)
	return spelled, outside && (allow == nil || allow(rel)), err
}

// cleanedPath is rawPath cleaned (catalog.CleanRelPath), or the 400 for one over
// catalog.MaxPathBytes or one that cleans away to nothing.
func cleanedPath(rawPath string) (string, int, string) {
	// A path no file can have is refused before it is cleaned, looked up (a
	// book's ancestors are walked) or stored (server#289).
	if len(rawPath) > catalog.MaxPathBytes {
		return "", http.StatusBadRequest, msgPathTooLong
	}
	// Normalize before the scope check so ".." can't smuggle an out-of-scope path
	// past a subtree grant (see catalog.CleanRelPath). An input that cleans away
	// to nothing ("", ".", "/", "Author/..") addresses no content.
	rel := catalog.CleanRelPath(rawPath)
	if rel == "" {
		return "", http.StatusBadRequest, "path is required"
	}
	return rel, 0, ""
}

// relInScope checks scope allows rel, a cleaned path (403 outside, 404 through
// a hidden folder).
func relInScope(lib *catalog.Library, scope catalog.Scope, rel string) (*catalog.Library, string, catalog.Scope, int, string) {
	if !scope.Allows(rel) {
		return nil, "", scope, http.StatusForbidden, msgNoPathAccess
	}
	// A hidden path is never content (library.ContentPath), whether a route
	// resolves a book or a file directly (stream, cover). It reads as missing,
	// after the scope check so the answer says nothing more than a missing file's
	// would.
	if library.ContentPath(rel) != nil {
		return nil, "", scope, http.StatusNotFound, "not found"
	}
	return lib, rel, scope, 0, ""
}

// msgNoPathAccess is the 403 for a path outside the caller's scope, and for one
// inside it whose book lies outside it (bookForPath): the two read the same, so
// the answer says nothing about what is there.
const msgNoPathAccess = "no access to this path"

// bookForPath returns the indexed book for a (library, path) the caller's scope
// allows: the book at the path, else the indexed folder book holding it (a part
// path, or a disc folder of a joined book, which shipped clients still hold after
// the join), else the book read on demand if the background scan has not reached
// it yet. A book above the path must be allowed too: a share granted only a disc
// folder (made before its join) reaches neither the joined book nor its other
// discs, and never triggers its re-read. That is library.ErrNotAllowed, which
// handlers answer as a path outside scope (msgNoPathAccess).
func (a *API) bookForPath(ctx context.Context, lib *catalog.Library, scope catalog.Scope, path string) (*catalog.Book, error) {
	b, err := a.cat.GetBookByPath(ctx, lib.ID, path)
	if errors.Is(err, catalog.ErrNotFound) {
		b, err = a.cat.GetBookHolding(ctx, lib.ID, path)
	}
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		if lib.Kind == catalog.KindPodcasts {
			return nil, library.ErrNotIndexable
		}
		return a.indexOnDemand(ctx, lib, path, scope.Allows)
	case err != nil:
		return nil, err
	case !scope.Allows(b.RelPath):
		return nil, library.ErrNotAllowed
	}
	return b, nil
}

// indexOnDemand reads the book at path (cleaned, scope-checked, no book of the
// index at it) on demand (a.indexPath), each look at the share given fileWait:
// one that stopped answering is a 503 share_not_answering, not this request's
// own time running out. Only a path the disk has as given, byte for byte
// (library.SafeJoinListed, through the kept listings, a.listed, as asGiven's):
// any other is library.ErrNotIndexable, a path with nothing there. The
// scanner's own read stats the path before it checks its spelling
// (resolveBook, dirPresent), so on a disk that ignores case a case variant of
// a folder it has would cost a stat and the folders' listings where a missing
// path costs one failed look; here the two take the same steps, so how long
// the 404 takes never tells a rule spelled in another case that the folder is
// there, on any route that resolves a book (bookAt, a favourite).
func (a *API) indexOnDemand(ctx context.Context, lib *catalog.Library, path string, allow func(string) bool) (*catalog.Book, error) {
	if _, there, err := library.SafeJoinListed(ctx, lib.Root, path, a.fileWait, a.listed.Names); err != nil {
		return nil, err
	} else if !there {
		return nil, library.ErrNotIndexable
	}
	return a.indexPath(library.WithLookTimeout(ctx, a.fileWait), *lib, path, allow)
}

// bookAt is bookForPath for a handler: on a failure it writes the answer and
// returns false. A book above the path outside scope is the 403 msgNoPathAccess,
// no book at the path is a 404 notFound, anything else writeCatalogError's
// answer with generic.
func (a *API) bookAt(w http.ResponseWriter, r *http.Request, lib *catalog.Library, scope catalog.Scope, path, notFound, generic string) (*catalog.Book, bool) {
	book, err := a.bookForPath(r.Context(), lib, scope, path)
	switch {
	case errors.Is(err, library.ErrNotAllowed):
		writeError(w, http.StatusForbidden, msgNoPathAccess)
	case errors.Is(err, library.ErrNotIndexable):
		writeError(w, http.StatusNotFound, notFound)
	case err != nil:
		a.writeCatalogError(w, err, "load book for path failed", generic, "library", lib.ID, "path", path)
	default:
		return book, true
	}
	return nil, false
}

// handleBrowseFS serves the filtered filesystem view: the real directory tree,
// scoped to the caller's share path rules, requiring no prior indexing.
func (a *API) handleBrowseFS(w http.ResponseWriter, r *http.Request) {
	if pathTooLong(w, r.URL.Query().Get("path")) {
		return
	}
	lib, scope, ok := a.browseScope(w, r)
	if !ok {
		return
	}
	if lib.Kind == catalog.KindPodcasts {
		listing, err := a.scanner.BrowsePodcasts(r.Context(), *lib, scope, userFrom(r.Context()).ID, r.URL.Query().Get("path"), queryInt(r, "offset", 0), queryInt(r, "limit", 200))
		if errors.Is(err, library.ErrNotIndexable) || errors.Is(err, catalog.ErrNotFound) {
			writeError(w, http.StatusNotFound, "directory not found")
			return
		}
		if err != nil {
			a.writeCatalogError(w, err, "browse podcasts failed", "Could not list shows.")
			return
		}
		writeJSON(w, 200, listing)
		return
	}

	var allow func(string) bool
	if !scope.AllowAll {
		allow = scope.VisibleInBrowse
	}
	// A folder asked for in another Unicode spelling is listed in the index's
	// (spelledIn), so the listing's path and its entries' are the paths the
	// index, and so annotateWithBooks, has. One whose index spelling the caller
	// can't see is missing, never listed from the disk in the spelling given.
	dir := r.URL.Query().Get("path")
	rel := catalog.CleanRelPath(dir)
	if rel != "" {
		spelled, missing, err := a.spelledIn(r.Context(), lib.ID, allow, rel)
		if err != nil {
			a.writeCatalogError(w, err, "resolve path spelling failed", "Could not list the folder.", "library", lib.ID)
			return
		}
		if missing {
			writeError(w, http.StatusNotFound, "directory not found")
			return
		}
		if spelled != rel {
			dir, rel = spelled, spelled
		}
	}
	// Nor is one outside the caller's share rules (allow), nor one the disk
	// doesn't spell as given (asGiven: a case variant, on a disk that ignores
	// case, of a folder the caller's rule never named), nor one through a link
	// out of the root, looked at before the folder is read, so such a folder is
	// never listed and answers as a missing one does. A path that climbs out of
	// the root by its names is BrowseFS's 400 first; looked at after it. One
	// that doesn't, as BrowseFS joins it (a leading slash is no climb:
	// "/Author" is the root's "Author", and would be read before the look
	// otherwise), can leave the root only through a link: one the index holds
	// (no look made before BrowseFS's) is "directory not found" too.
	early := rel != "" && filepath.IsLocal(filepath.Join(".", filepath.FromSlash(dir)))
	if early && !a.fsGiven(w, r, lib, rel, allow) {
		return
	}
	ignore := library.ParseIgnore(lib.IgnorePatterns)
	listing, err := library.BrowseFS(lib.Root, dir,
		queryInt(r, "offset", 0), queryInt(r, "limit", 200), allow, ignore)
	if errors.Is(err, library.ErrOutsideRoot) && !early {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	if err != nil {
		writeError(w, http.StatusNotFound, "directory not found")
		return
	}
	if rel != "" && !early && !a.fsGiven(w, r, lib, rel, allow) {
		return
	}
	a.annotateWithBooks(r, lib, scope, ignore, listing)
	writeJSON(w, http.StatusOK, listing)
}

// annotateWithBooks turns the raw filesystem listing into the hybrid view by
// attaching indexed metadata to entries that are books, so a browsing client
// sees titles/authors/durations alongside the raw tree, and marking the folders
// the scanner would read as books it hasn't indexed yet (pending_book) for any
// caller, within their scope. A books library's listing only: a podcast
// library's is BrowsePodcasts'.
func (a *API) annotateWithBooks(r *http.Request, lib *catalog.Library, scope catalog.Scope, ignore *library.Ignore, listing *library.Listing) {
	if len(listing.Entries) == 0 {
		return
	}
	paths := make([]string, len(listing.Entries))
	for i, e := range listing.Entries {
		paths[i] = e.Path
	}
	books, err := a.cat.BooksByPaths(r.Context(), lib.ID, paths)
	if err != nil {
		a.log.Warn("annotate fs listing failed", "library", lib.ID, "err", err)
		return
	}
	// Per-folder detection overrides let the console show/toggle a folder's
	// classification; best-effort, so a failure here doesn't drop the listing.
	overrides, overridesErr := a.cat.FolderOverrides(r.Context(), lib.ID)
	if overridesErr != nil {
		a.log.Warn("annotate fs overrides failed", "library", lib.ID, "err", overridesErr)
	}
	// Which folders a `book` override would join from their disc folders; likewise.
	// For an admin only (the console's Folders screen offers the join there): it is
	// no part of the player's listing, which stays as it was for everyone else.
	var split map[string]bool
	if u := userFrom(r.Context()); u != nil && u.Role == auth.RoleAdmin {
		if split, err = a.cat.SplitFolders(r.Context(), lib.ID, paths); err != nil {
			a.log.Warn("annotate fs split discs failed", "library", lib.ID, "err", err)
		}
	}
	library.MarkBooks(listing.Entries, books)
	for i := range listing.Entries {
		e := &listing.Entries[i]
		if m, ok := overrides[e.Path]; ok {
			e.Override = m
		}
		e.SplitDiscs = e.IsDir && split[e.Path]
	}
	// Without the overrides (or with only some: a read can fail part way) a
	// `collection` folder would read as a pending book: none are marked then, as
	// the listing was before.
	if overridesErr == nil {
		a.markPendingBooks(r.Context(), lib, scope, ignore, listing.Entries, overrides)
	}
}

// markPendingBooks is annotateWithBooks' pending_book pass (library.
// MarkPendingBooks), best-effort like the rest of the annotations: the folders
// holding an indexed book are read from the index first, so only the others
// are looked at on disk.
func (a *API) markPendingBooks(ctx context.Context, lib *catalog.Library, scope catalog.Scope, ignore *library.Ignore, entries []library.Entry, overrides map[string]string) {
	var dirs []string
	for _, e := range entries {
		if e.IsDir && !e.IsBook {
			dirs = append(dirs, e.Path)
		}
	}
	if len(dirs) == 0 {
		return
	}
	holding, err := a.cat.FoldersHoldingBooks(ctx, lib.ID, dirs)
	if err != nil {
		a.log.Warn("annotate fs pending books failed", "library", lib.ID, "err", err)
		return
	}
	library.MarkPendingBooks(ctx, *lib, entries, overrides, holding, scope.Allows, ignore)
}

// handleListBooks serves the computed/hybrid view from the index, scoped to the
// caller's share path rules.
func (a *API) handleListBooks(w http.ResponseWriter, r *http.Request) {
	lib, scope, write := a.browseList(w, r)
	if write == nil {
		return
	}
	page, err := a.cat.ListBooks(r.Context(), catalog.ListOptions{
		LibraryID:   lib.ID,
		Author:      r.URL.Query().Get("author"),
		Series:      r.URL.Query().Get("series"),
		Narrator:    r.URL.Query().Get("narrator"),
		Genre:       strings.TrimSpace(r.URL.Query().Get("genre")),
		Tag:         strings.TrimSpace(r.URL.Query().Get("tag")),
		Memberships: r.URL.Query().Get("memberships") == "1",
		Sort:        r.URL.Query().Get("sort"),
		Limit:       queryInt(r, "limit", 50),
		Cursor:      r.URL.Query().Get("cursor"),
		Scope:       &scope,
	})
	if err != nil {
		a.writeCatalogError(w, err, "list books failed", "could not load books", "library", lib.ID)
		return
	}
	write(page)
}

// handleSearch runs a full-text search across the caller's accessible content,
// scoped per-library to their share path rules.
func (a *API) handleSearch(w http.ResponseWriter, r *http.Request) {
	gen, cacheable := a.cat.ContentGeneration() // before any read (listcache.go)
	scopes, ok := a.callerScopes(w, r, "search failed")
	if !ok {
		return
	}
	write := a.cachedList(w, r, gen, cacheable, scopes)
	if write == nil {
		return
	}
	books, err := a.cat.Search(r.Context(), r.URL.Query().Get("q"), scopes, queryInt(r, "limit", 50))
	if err != nil {
		// The player aborts a type-ahead search on the next keystroke: a
		// request that gave up is no failure, so no log line.
		if errors.Is(r.Context().Err(), context.Canceled) {
			writeError(w, http.StatusInternalServerError, "search failed")
			return
		}
		a.writeCatalogError(w, err, "search failed", "search failed", "user", userFrom(r.Context()).ID, "scopes", len(scopes))
		return
	}
	write(map[string]any{"books": books})
}

// handleRecentBooks returns the most recently added books across every library
// the caller can reach, scoped to their share path rules. A single cross-library
// endpoint so clients render one merged "recently added" list (rather than
// fanning out to each library's /books and concatenating).
func (a *API) handleRecentBooks(w http.ResponseWriter, r *http.Request) {
	gen, cacheable := a.cat.ContentGeneration() // before any read (listcache.go)
	scopes, ok := a.callerScopes(w, r, "could not load books")
	if !ok {
		return
	}
	write := a.cachedList(w, r, gen, cacheable, scopes)
	if write == nil {
		return
	}
	books, err := a.cat.RecentBooks(r.Context(), scopes, queryInt(r, "limit", 50))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load books")
		return
	}
	write(map[string]any{"books": books})
}

// handleItem returns full book detail (metadata + files + chapters + description)
// for a path, indexing it on demand if needed. A book with an ebook and a
// read-along map carries readalong (readalongOf), only here among the player's
// book reads; its ebook is read again when the file changed since the index
// read it (library.Scanner.RefreshEbook).
func (a *API) handleItem(w http.ResponseWriter, r *http.Request) {
	lib, path, scope, status, msg := a.authorizedScope(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	book, ok := a.bookAt(w, r, lib, scope, path, "no book at that path", "could not load book")
	if !ok {
		return
	}
	if book.Episode != nil {
		states, err := a.podcastFetchStatuses(r, book.LibraryID, []string{book.RelPath})
		if err != nil {
			a.writeCatalogError(w, err, "load podcast fetch status failed", "could not load book")
			return
		}
		if st, ok := states[book.RelPath]; ok {
			book.Episode.Fetch = &st
		}
	}
	answering := a.warmRemux(r.Context(), lib, book)
	dp := a.directPlayable(book)
	book.DirectPlayable = &dp
	// An EPUB replaced in place since the index read it: its size (and the
	// row) follow it now, not at the next scan. On a share that stopped
	// answering the look is given up on (fileWait) and the stored one answers.
	// Once the warm's look was given up on, neither this nor the read-along's
	// is tried: each would wait out its own time on the same share and answer
	// the same (the stored ebook, no read-along), three of them the request's
	// whole time.
	if answering {
		a.scanner.RefreshEbook(r.Context(), *lib, book, a.fileWait)
		book.Readalong = a.readalongOfBook(r.Context(), lib, book)
	}
	book.OtherRecordings = a.otherRecordings(r.Context(), lib, book)
	writeJSON(w, http.StatusOK, book)
}

// otherRecordings is the caller's other recordings of book's community work
// for /item (catalog.OtherRecordings over their scopes; the `other_recordings`
// capability): none with metadata off, in a podcasts library, or for a book
// with no work. It is an extra like `readalong`: a failure to read them is
// logged and the book goes out without them.
func (a *API) otherRecordings(ctx context.Context, lib *catalog.Library, book *catalog.Book) []catalog.Book {
	if !a.metadataOn() || lib.Kind != catalog.KindBooks || book.WorkID == "" {
		return nil
	}
	scopes, err := a.cat.CallerScopes(ctx, callerOf(ctx))
	if err == nil {
		var books []catalog.Book
		if books, err = a.cat.OtherRecordings(ctx, scopes, book); err == nil {
			return books
		}
	}
	if ctx.Err() == nil {
		a.log.Warn("load other recordings failed", "err", err, "library", lib.ID, "path", book.RelPath)
	}
	return nil
}

// handleChapters returns a book's normalized playable units. Each chapter
// carries file_path so playback is purely path-based and a single chaptered m4b
// and a folder of mp3 parts render identically.
func (a *API) handleChapters(w http.ResponseWriter, r *http.Request) {
	lib, path, scope, status, msg := a.authorizedScope(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	book, ok := a.bookAt(w, r, lib, scope, path, "no book at that path", "could not load chapters")
	if !ok {
		return
	}
	a.warmRemux(r.Context(), lib, book)
	// Emit [] rather than null for empty files/chapters so the envelope matches the
	// hand-mirrored client types (which declare these as non-null arrays). book is
	// a fresh per-request value, so normalizing in place is safe.
	if book.Files == nil {
		book.Files = []catalog.BookFile{}
	}
	if book.Chapters == nil {
		book.Chapters = []metadata.Chapter{}
	}
	out := map[string]any{
		"library_id":      lib.ID,
		"path":            book.RelPath,
		"duration":        book.Duration,
		"is_folder":       book.IsFolder,
		"files":           book.Files,
		"chapters":        book.Chapters,
		"codec":           book.Codec,
		"direct_playable": a.directPlayable(book),
	}
	// Additive: present only when the chapters are a community list fitted onto
	// the audio ("community") or the ones the Bridge found in the narration
	// ("generated"), so the player can say where they came from.
	if book.ChaptersSource != "" {
		out["chapters_source"] = book.ChaptersSource
	}
	writeJSON(w, http.StatusOK, out)
}

// serveCustomCover answers a cover request from a custom cover uploaded in the
// admin console, if the path has one. A custom cover can be replaced at any time,
// so it is revalidated (no-cache + ETag) rather than cached for a day, and a
// still-fresh conditional request is answered from the cover's timestamp alone,
// without reading the image. The validator is an ETag, not Last-Modified, on
// purpose: once the cover is removed, a client revalidating with If-Modified-Since
// would get a 304 from the book's own (older) sibling cover file and keep showing
// the removed image; an If-None-Match never matches that fallback. served is false
// when the path has no custom cover (or no book is indexed there).
func (a *API) serveCustomCover(w http.ResponseWriter, r *http.Request, libID int64, path string) (served bool, err error) {
	info, err := a.cat.CoverInfo(r.Context(), libID, path)
	if errors.Is(err, catalog.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if (conditional{etag: coverETag(info.UpdatedAt), cacheControl: customCoverCache}).notModified(w, r) {
		return true, nil
	}
	cv, err := a.cat.Cover(r.Context(), libID, path)
	if errors.Is(err, catalog.ErrNotFound) {
		return false, nil // removed since the check above: fall back to the book's art
	}
	if err != nil {
		return false, err
	}
	conditional{etag: coverETag(cv.UpdatedAt), cacheControl: customCoverCache}.serve(w, r, cv.MIME, cv.Data)
	return true, nil
}

// customCoverCache is a custom cover's Cache-Control (and its thumbnails'): it can
// be replaced at any moment, so it is revalidated every time.
const customCoverCache = "private, no-cache"

// coverCache is the Cache-Control of a cover that is not custom: file art and its
// thumbnails, and a community cover's thumbnail (the image behind a community
// cover URL does not change; a new cover is a new URL).
const coverCache = "private, max-age=86400"

// conditional is a response revalidated by its ETag alone: no Last-Modified (a
// zero modtime), so a validator never matches another source's file dates.
type conditional struct {
	etag, cacheControl string
}

// notModified answers r with a 304 when its If-None-Match names the ETag (as
// http.ServeContent in serve would), reporting whether it did, so the body need
// not be read at all.
func (c conditional) notModified(w http.ResponseWriter, r *http.Request) bool {
	inm := r.Header.Get("If-None-Match")
	if inm == "" || !spa.ETagListMatches(inm, c.etag) {
		return false
	}
	c.setHeaders(w)
	w.WriteHeader(http.StatusNotModified)
	return true
}

// serve sends body; ServeContent still honours the ETag for conditional and
// Range requests.
func (c conditional) serve(w http.ResponseWriter, r *http.Request, contentType string, body []byte) {
	c.setHeaders(w)
	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
}

func (c conditional) setHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", c.cacheControl)
	w.Header().Set("ETag", c.etag)
}

// answerCustomCover answers r from the custom cover at path if there is one, and
// reports whether the request was answered (the cover served, or an error written).
func (a *API) answerCustomCover(w http.ResponseWriter, r *http.Request, libID int64, path string) bool {
	served, err := a.serveCustomCover(w, r, libID, path)
	if err != nil {
		a.writeCatalogError(w, err, "load custom cover failed", "could not load cover", "library", libID, "path", path)
	}
	return served || err != nil
}

// coverETag is a custom cover's validator, derived from when it was stored.
func coverETag(updatedAt string) string {
	modified, _ := time.Parse(time.RFC3339Nano, updatedAt)
	return `"cover-` + strconv.FormatInt(modified.UnixNano(), 36) + `"`
}

// directPlayable is what /item and /chapters say as direct_playable: the book
// plays in browsers as stored, or it is MP3 in Matroska and this server serves
// that as its remuxed MP3 (handleStream).
func (a *API) directPlayable(book *catalog.Book) bool {
	return book.PlaysInBrowsers() || (a.remuxOn() && book.PlaysRemuxed())
}

// warmRemux starts, in the background, the remuxes of the book's MP3-in-MKA
// files while the remux is on, so the player's first stream finds them cached
// (a cold remux of a long file takes seconds, past a player's stall timer). It
// never blocks (warmRemuxes). It reports false when a join went unanswered
// (metadata.Unanswered): the book's share is not answering, for a caller with
// more looks to make there (handleItem).
func (a *API) warmRemux(ctx context.Context, lib *catalog.Library, book *catalog.Book) (answering bool) {
	if !a.remuxing(lib) {
		return true
	}
	var rels []string
	for _, f := range book.Files {
		if book.FileRemuxable(f) {
			rels = append(rels, f.RelPath)
		}
	}
	_, answering = a.warmRemuxes(ctx, lib, rels)
	return answering
}

// remuxing reports whether lib's MP3-in-MKA files are served as their remux:
// the remux is on, and lib is no podcast library (an episode's stream never
// remuxes).
func (a *API) remuxing(lib *catalog.Library) bool {
	return a.remuxOn() && lib.Kind != catalog.KindPodcasts
}

// warmRemuxes joins rels (MP3-in-MKA files of one book) to lib's root and
// starts their remuxes in the background: media.FileCache.WarmAll runs them
// one at a time under the server's context, skipping the cached and the known
// refusals, and runs one book's list once however often it is asked (by its
// first path, so every caller passes the book's whole list). Each join is a
// metadata.Bounded call (ctx, at most fileWait): on a share that stopped
// answering none is warmed and the rest are not looked at (answering false).
// abs holds each rel's path, "" for one not joined.
func (a *API) warmRemuxes(ctx context.Context, lib *catalog.Library, rels []string) (abs []string, answering bool) {
	abs, answering = a.joinRemuxes(ctx, lib, rels)
	if answering {
		var paths []string
		for _, p := range abs {
			if p != "" {
				paths = append(paths, p)
			}
		}
		a.remux.WarmAll(a.baseCtx, a.ffmpeg, paths)
	}
	return abs, answering
}

// joinRemuxes is warmRemuxes' joins without the warm: abs holds each rel's
// path under lib's root, "" for one not joined, and answering is false (the
// rest not looked at) once a join went unanswered.
func (a *API) joinRemuxes(ctx context.Context, lib *catalog.Library, rels []string) (abs []string, answering bool) {
	abs = make([]string, len(rels))
	for i, rel := range rels {
		p, err := library.SafeJoinBounded(ctx, lib.Root, rel, a.fileWait)
		if metadata.Unanswered(ctx, err) {
			return abs, false
		}
		if err == nil {
			abs[i] = p
		}
	}
	return abs, true
}

// remuxColdWait is how long a stream request waits for a remux that isn't
// cached yet before it answers with the live transcode instead (the remux goes
// on, and the next request gets the copy): under the player's 3 s stall timer
// and Android's 8 s read timeout.
const remuxColdWait = 2 * time.Second

// remuxDownloadWait bounds how long a download, or a stream with every
// transcode slot busy, waits for a remux still running past remuxColdWait
// (server#212) before it is transcoded live after all: nothing is sent while it
// waits, so it stays under the 60 s an idle response gets from Android's
// downloader (OkHttp's read timeout) and a reverse proxy's default (nginx's
// proxy_read_timeout), which would otherwise fail a long book's remux with
// nothing saved.
const remuxDownloadWait = 45 * time.Second

// handleStream serves an audio file by path. By default it streams the file with
// Range support (?download=1 forces a download). With ?transcode=1 it re-encodes
// to MP3 via ffmpeg for codecs browsers can't decode (with ?download=1 an
// attachment named <stem>.mp3); ?t=<seconds> starts that transcode mid-file
// (transcoded output is not byte-seekable, so seeking is by re-requesting).
// An .mka holding MP3 is served, at this same URL, as its
// lossless remux to plain MP3 (server#174: no browser and no iOS player opens
// Matroska) while ffmpeg is configured, unless ?original=1 asks for the stored
// bytes; one ffmpeg can't remux (another codec inside) is served as stored, and
// one whose remux can't be had now (still running after remuxColdWait, or
// remuxDownloadWait for a download or with every transcode slot busy; low disk,
// an ffmpeg failure) is transcoded live, as ?transcode=1 would, since
// direct_playable told the web player it needn't ask for that. ?rendition=watch
// asks for the file's watch rendition instead (serveRendition), whatever else
// the query says. The path is the actual audio file (a chapter's file_path).
// Every look at the file on its way to the client (the path's join, its open, a
// copy's or a transcode's look at it, a podcast episode's checks) is a
// metadata.Bounded call under one bound, streamWait from the request's start:
// a share that stopped answering is a 503 share_not_answering by then.
func (a *API) handleStream(w http.ResponseWriter, r *http.Request) {
	deadline := time.Now().Add(a.streamWait)
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return
	}
	lib, rel, _, refused, status, msg := a.scopedFile(r, id, r.URL.Query().Get("path"))
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	if q := r.URL.Query(); q.Has("rendition") {
		a.serveRendition(w, r, lib, rel, refused, q.Get("rendition"), deadline)
		return
	}
	if lib.Kind == catalog.KindPodcasts {
		a.servePodcastStream(w, r, lib, rel, deadline)
		return
	}

	abs, ok := a.audioFile(w, r, lib, rel, refused, deadline)
	if !ok {
		return
	}

	if r.URL.Query().Get("transcode") == "1" {
		a.serveTranscode(w, r, lib.ID, rel, abs, deadline)
		return
	}
	download := r.URL.Query().Get("download") == "1"
	if a.remuxOn() && strings.EqualFold(filepath.Ext(rel), ".mka") && r.URL.Query().Get("original") != "1" {
		var served bool
		if served, deadline = a.serveRemuxed(w, r, lib.ID, rel, abs, download, deadline); served {
			return
		}
	}
	if err := media.ServeFile(w, r, abs, download, waitLeft(deadline)); err != nil {
		writeShareNotAnswering(w)
	}
}

// waitLeft is what is left of a stream's bound (deadline, handleStream) for its
// next look, at least a nanosecond: a metadata.Bounded call given 0 would wait
// with none.
func waitLeft(deadline time.Time) time.Duration {
	return max(time.Until(deadline), time.Nanosecond)
}

// afterWait is a stream's deadline once it has waited on something that was no
// look at the file (a remux running): moved on to leave the next look at least
// a look's time (fileWait), not a bound the wait may have used up, which would
// answer 503 share_not_answering from a share that answers.
func (a *API) afterWait(deadline time.Time) time.Time {
	if d := time.Now().Add(a.fileWait); d.After(deadline) {
		return d
	}
	return deadline
}

// lookAtSource is media.Source's look at abs, the copy a remux or rendition
// makes from, bounded by the stream's deadline: answered when the request went
// away or the look was given up on (a 503 share_not_answering written), else
// the look's error (no regular file there).
func lookAtSource(w http.ResponseWriter, r *http.Request, abs string, deadline time.Time) (answered bool, err error) {
	_, err = media.Source(r.Context(), abs, waitLeft(deadline))
	switch {
	case r.Context().Err() != nil:
		return true, err
	case metadata.GaveUp(err):
		writeShareNotAnswering(w)
		return true, err
	}
	return false, err
}

// audioFile is the stream's guard on rel (scope-checked already): its absolute
// path, or false having written 404 for a file that isn't recognized audio, 400
// for a path that escapes the root, and 503 share_not_answering for a join
// whose looks (bounded, up to deadline) were given up on. The scope check
// bounds a stream to the caller's grant, but the library root can also hold
// non-audio sidecar files (.nfo, .jpg, a stray .env/backup); BrowseFS hides
// those from listings, and cover art has its own endpoint, so serving them
// here would be an avenue to exfiltrate arbitrary in-scope files by guessing
// paths. A path the disk doesn't spell as given (asGiven: a case variant, on a
// disk that ignores case, of a file a share rule spelled that way never
// named), and a refused rel (spelledPath: another spelling of a book outside
// the caller's scope), joined with the same steps as a missing path, is then
// one no file has (noFile), so every form of the stream answers as for a
// missing file and none opens the file it would find.
func (a *API) audioFile(w http.ResponseWriter, r *http.Request, lib *catalog.Library, rel string, refused bool, deadline time.Time) (string, bool) {
	if !metadata.IsAudio(rel) {
		writeError(w, http.StatusNotFound, "not an audio file")
		return "", false
	}
	abs, held, there, err := a.asGiven(r.Context(), lib, rel, deadline)
	if err == nil && held {
		abs, err = library.SafeJoinBounded(r.Context(), lib.Root, rel, waitLeft(deadline))
	}
	switch {
	case errors.Is(err, library.ErrOutsideRoot):
		writeError(w, http.StatusBadRequest, "invalid path")
		return "", false
	case err != nil:
		a.writeJoinError(w, r, err, lib, "check a stream's path failed", "Could not load the file.")
		return "", false
	}
	if refused || !(held || there) {
		abs = noFile(abs)
	}
	return abs, true
}

// asGiven is how the stream and /fs, the routes that open rel (cleaned,
// scope-checked) itself, learn whether the library's disk has it as given,
// byte for byte: held when the index holds it as given (catalog.IndexHolds,
// the scan read it from the listings; nothing looked at, abs empty), else
// library.SafeJoinListed's join (abs, there; listings through a.listed, the
// looks bounded by deadline). Share rules match byte for byte, so a rule
// spelled in another case than the disk's names nothing there, like a rule
// naming a missing folder; but a disk that ignores case (macOS's, a Windows or
// SMB share) opens "émile zola/x.m4b" as "Émile Zola/x.m4b", a book that rule
// never named, so a path that isn't there as given answers as missing. A
// missing path and a case variant take the same steps, each name looked for
// in its folder's listing before it is looked at (no lstat a variant answers
// and a missing name doesn't), so how long the check takes never tells a rule
// spelled in another case that the folder is there. err is a failed index
// lookup, library.ErrOutsideRoot (a link out of the root, nothing below it
// looked at), a look given up on (metadata.GaveUp) or ctx's.
func (a *API) asGiven(ctx context.Context, lib *catalog.Library, rel string, deadline time.Time) (abs string, held, there bool, err error) {
	if held, err := a.cat.IndexHolds(ctx, lib.ID, rel); err != nil || held {
		return "", held, false, err
	}
	abs, there, err = library.SafeJoinListed(ctx, lib.Root, rel, waitLeft(deadline), a.listed.Names)
	return abs, false, there, err
}

// writeJoinError answers asGiven's err (a look given up on is a 503
// share_not_answering, a failed lookup a 500), or nothing when the client
// left, with no one to answer.
func (a *API) writeJoinError(w http.ResponseWriter, r *http.Request, err error, lib *catalog.Library, op, msg string) {
	if !metadata.GaveUp(err) && r.Context().Err() != nil {
		return
	}
	a.writeCatalogError(w, err, op, msg, "library", lib.ID)
}

// fsGiven is /fs's asGiven: false when it has answered, a folder the disk
// doesn't spell as given as "directory not found", and so one through a link
// out of the root (whatever the folder it points to holds: nothing below the
// link is looked at). So is a folder outside the caller's share rules (allow,
// Scope.VisibleInBrowse; nil allows every one), answered before any listing is
// read: a listing of it would hold none of their entries, and a 200 for it
// where a missing one is a 404 would tell them it is there.
func (a *API) fsGiven(w http.ResponseWriter, r *http.Request, lib *catalog.Library, rel string, allow func(string) bool) bool {
	if allow != nil && !allow(rel) {
		writeError(w, http.StatusNotFound, "directory not found")
		return false
	}
	_, held, there, err := a.asGiven(r.Context(), lib, rel, time.Now().Add(a.fileWait))
	switch {
	case err == nil && (held || there):
		return true
	case err == nil || errors.Is(err, library.ErrOutsideRoot):
		writeError(w, http.StatusNotFound, "directory not found")
	default:
		a.writeJoinError(w, r, err, lib, "check a folder's path failed", "Could not list the folder.")
	}
	return false
}

// noFile is abs as a path no file has: a NUL byte is in no file name, so every
// look at it (a stat, an open) fails at once without reaching the disk, as a
// missing file's does, and each of the stream's forms answers as it does for
// one (audioFile).
func noFile(abs string) string { return abs + "\x00" }

// serveCopy answers with a cached copy f (media.FileCache.Open) and closes it:
// Range and the copy's strong etag, download an attachment named name. No
// Last-Modified: the copy's mtime is the cache's LRU clock (moved on a hit), so
// a client resuming by date (If-Range) would get the whole file again; it
// resumes by the etag, which a date validator kept for the stored file never
// matches either.
func serveCopy(w http.ResponseWriter, r *http.Request, f *os.File, etag, name string, download bool) {
	defer f.Close()
	w.Header().Set("ETag", etag)
	media.ServeOpenedAs(w, r, f, name, time.Time{}, download)
}

// serveTranscode answers with the live MP3 transcode of the library file abs
// (rel in library lib) from ?t=, in one of the transcode slots.
func (a *API) serveTranscode(w http.ResponseWriter, r *http.Request, lib int64, rel, abs string, deadline time.Time) {
	h, r := a.beginTranscode(w, r, lib, rel)
	if h == nil {
		return
	}
	defer a.endTranscode(h)
	a.transcodeHeld(w, r, h, abs, deadline)
}

// transcodeHeld is serveTranscode once h is taken (the caller ends it): the
// transcode from h's start, written through h's writer, h.ended set once it
// ran to its end; with ?download=1 an attachment named <stem>.mp3. Its look
// at abs given up on at the stream's deadline is a 503 share_not_answering.
func (a *API) transcodeHeld(w http.ResponseWriter, r *http.Request, h *transcodeHold, abs string, deadline time.Time) {
	ended, err := media.Transcode(h.writer(w), r, abs, a.ffmpeg, h.ask.start, h.ask.download, waitLeft(deadline), transcodeIdleFor(r, h.ask), a.log)
	h.ended = ended
	if err != nil {
		writeShareNotAnswering(w)
	}
}

// transcodeIdleFor is how long r's transcode of ask waits for its client to
// read before it ends: media.TranscodeIdle for the console's chapter editor
// (its cast token, auth.Credential.Editor), which asks for its stream again
// after a 10 s pause, and for a download (ask.download, the very flag
// tokenHoldToTake spares a hold by: no other ask takes its slot over), so a
// client that stopped reading it (a paused browser download; a transcode
// resumes by no Range, so its save starts again anyway) would hold it for
// good; 0 (no idle end) for every other client's stream. The web player does
// not ask again for a transcode it loaded but never played, so it would stall
// after a long wait, and a Cast receiver asks for nothing again on resume.
func transcodeIdleFor(r *http.Request, ask transcodeAsk) time.Duration {
	if c := credentialFrom(r.Context()); c.Kind == auth.KindCast && c.Editor || ask.download {
		return media.TranscodeIdle
	}
	return 0
}

// serveRemuxed answers with abs's remuxed MP3 (media.NewRemuxer) with Range and
// its own validator: a strong ETag from the cache key and no Last-Modified
// (serveCopy), so an If-Range or If-Modified-Since a client kept from the
// stored Matroska (a 2.4.x download) never earns a 304 or MP3 bytes spliced
// onto Matroska. When the remux can't be had now (still running after
// remuxColdWait, ErrUnavailable, an ffmpeg or cache failure) it answers
// with the live transcode, the remux going on for the next request. A remux still
// running is waited for instead (server#212), up to remuxDownloadWait, when the
// request is a download (whose saved copy would otherwise be the lossy
// transcode) or every transcode slot is busy (which would otherwise be a 503). It reports
// false, having written nothing, when the stored file should be served instead
// (ErrRefused: not MP3 inside, or no regular file there), with the deadline
// that file's open has (afterWait: the waits for a remux that then refused the
// file were no looks at it); true also when
// the request went away while waiting. Its look at abs (media.Source) given up
// on at the stream's deadline is a 503 share_not_answering. A HEAD starts no
// remux and waits for none: the cached copy's headers, else the transcode's.
func (a *API) serveRemuxed(w http.ResponseWriter, r *http.Request, lib int64, rel, abs string, download bool, deadline time.Time) (served bool, left time.Time) {
	answered, err := lookAtSource(w, r, abs, deadline)
	if answered || err != nil {
		return answered, deadline // err: no regular file there, the stored file's answer (a 404)
	}
	var f *os.File
	var etag string
	if r.Method == http.MethodHead {
		// A HEAD starts no remux: the copy when it is cached, else the
		// transcode's answer (no ffmpeg run), as a GET would be answered now.
		f, etag, err = a.remux.Cached(r.Context(), abs)
	} else {
		wait, cancel := context.WithTimeout(r.Context(), a.remuxWait)
		defer cancel()
		f, etag, err = a.remux.Open(wait, a.baseCtx, a.ffmpeg, abs)
	}
	var held *transcodeHold // a transcode slot taken while the remux runs on
	// Still remuxing when the wait ran out (it goes on): not a failure to warn of.
	running := r.Context().Err() == nil && errors.Is(err, media.ErrPending)
	if running {
		if !download {
			held, r = a.takeTranscodeSlot(w, r, transcodeAskOf(r, lib, rel))
		}
		if held == nil {
			// A download, or no slot free: wait for the remux, up to
			// remuxDownloadWait, then transcode after all.
			long, cancelLong := context.WithTimeout(r.Context(), a.remuxLongWait)
			defer cancelLong()
			f, etag, err = a.remux.Open(long, a.baseCtx, a.ffmpeg, abs)
			// A remux that failed by its own timeout is a failure, not this wait.
			running = r.Context().Err() == nil && errors.Is(err, media.ErrPending)
		} else {
			defer a.endTranscode(held)
		}
	}
	if err != nil {
		// The waits for the remux were no looks at the file (the share answered
		// lookAtSource): the next look, the transcode's or the stored file's
		// open, is given at least a look's time (a download whose remux ran
		// long, or refused the file late).
		deadline = a.afterWait(deadline)
		switch {
		case r.Context().Err() != nil:
			return true, deadline
		case errors.Is(err, media.ErrRefused):
			return false, deadline
		case a.baseCtx.Err() != nil:
			// The server is stopping, and its remux with it: no new transcode now.
			writeError(w, http.StatusServiceUnavailable, "the server is shutting down")
			return true, deadline
		case running:
			// Still remuxing (it goes on): this request is transcoded.
		case !errors.Is(err, media.ErrUnavailable):
			a.log.Warn("remux failed: transcoding Matroska instead", "path", abs, "err", err)
		}
		if held != nil {
			a.noteTranscode(r, lib, rel)
			a.transcodeHeld(w, r, held, abs, deadline)
			return true, deadline
		}
		a.serveTranscode(w, r, lib, rel, abs, deadline)
		return true, deadline
	}
	serveCopy(w, r, f, etag, media.StemName(abs, a.remux.Ext()), download)
	return true, deadline
}

// beginTranscode admits one ?transcode=1 request for the file at rel: 503 when
// ffmpeg isn't configured or no slot is free for it (Retry-After 5), else its
// hold (takeTranscodeSlot: a repeat ask takes its earlier one's over, a token
// past its cap one of its streams', never a download's), for
// endTranscode once served, and r with the slot's context (a takeover ends
// it), the device's session marked transcoded (noteTranscode). A HEAD takes
// no slot and is answered as a GET would be, the 503 included, with no ffmpeg
// run (media.Transcode), so a client can ask whether its transcode would run,
// and with what the same ask's running GET has sent (headerTranscodeSent).
func (a *API) beginTranscode(w http.ResponseWriter, r *http.Request, lib int64, rel string) (*transcodeHold, *http.Request) {
	if a.ffmpeg == "" {
		writeError(w, http.StatusServiceUnavailable, "transcoding is not available on this server")
		return nil, r
	}
	// Bound concurrent ffmpeg processes; when saturated, ask the client to retry
	// rather than forking another core-pinning encoder.
	h, r := a.takeTranscodeSlot(w, r, transcodeAskOf(r, lib, rel))
	if h == nil {
		writeTranscodeBusy(w)
		return nil, r
	}
	h.describe(w)
	a.noteTranscode(r, lib, rel)
	return h, r
}

// noteTranscode marks the device's session transcoded (a cast token's is its
// sender's), for a transcode about to run: a HEAD runs none.
func (a *API) noteTranscode(r *http.Request, lib int64, rel string) {
	if r.Method != http.MethodHead {
		a.streams.Note(credentialFrom(r.Context()).DeviceID, lib, rel, time.Now())
	}
}

// writeTranscodeBusy is the 503 for a transcode no slot is free for.
func writeTranscodeBusy(w http.ResponseWriter) {
	setRetryAfter(w, 5*time.Second)
	writeError(w, http.StatusServiceUnavailable, "server busy transcoding; try again shortly")
}

// handleCover serves a book's cover for a path: a custom cover uploaded in the
// admin console, else a sibling cover file if indexed, else embedded art from the
// book's primary audio file. With ?size= it is a JPEG thumbnail of that art
// instead (handleCoverThumbnail).
func (a *API) handleCover(w http.ResponseWriter, r *http.Request) {
	lib, path, scope, status, msg := a.authorizedScope(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	if lib.Kind == catalog.KindPodcasts {
		a.servePodcastCover(w, r, lib, path)
		return
	}

	if r.URL.Query().Has("size") {
		a.handleCoverThumbnail(w, r, lib, path, scope)
		return
	}
	if a.answerCustomCover(w, r, lib.ID, path) {
		return
	}
	book, ok := a.bookAt(w, r, lib, scope, path, "no cover", "could not load cover")
	if !ok {
		return
	}
	// The lookup above was by the requested path. A part path resolves to its folder
	// book, and bookForPath may have just indexed the book (a custom cover is served
	// only for an indexed one): either way the book's own path can carry a custom
	// cover, ahead of its own art.
	if a.answerCustomCover(w, r, lib.ID, book.RelPath) {
		return
	}
	a.serveArt(w, r, lib.Root, book.ArtFiles())
}
