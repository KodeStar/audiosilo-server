package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/web/spa"
)

// handleListLibraries lists libraries the caller can reach (via any share).
func (a *API) handleListLibraries(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	libs, err := a.cat.AccessibleLibraries(r.Context(), u.ID, u.Role == "admin")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list libraries")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"libraries": libs})
}

// libraryScope loads a library and the caller's effective access scope for it.
// A non-admin with no granting share gets 403.
func (a *API) libraryScope(r *http.Request, libraryID int64) (*catalog.Library, catalog.Scope, int, string) {
	u := userFrom(r.Context())
	scope, err := a.cat.UserScope(r.Context(), u.ID, libraryID, u.Role == "admin")
	if err != nil {
		return nil, scope, http.StatusInternalServerError, "access check failed"
	}
	if !scope.AllowAll && len(scope.Paths) == 0 {
		return nil, scope, http.StatusForbidden, "no access to this library"
	}
	lib, err := a.cat.GetLibrary(r.Context(), libraryID)
	if errors.Is(err, catalog.ErrNotFound) {
		return nil, scope, http.StatusNotFound, "library not found"
	}
	if err != nil {
		return nil, scope, http.StatusInternalServerError, "could not load library"
	}
	return lib, scope, 0, ""
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

// authorizedPath resolves {id} + ?path=, checks the path is within the caller's
// scope, and returns the library + path. Used by every path-addressed endpoint.
func (a *API) authorizedPath(r *http.Request) (*catalog.Library, string, int, string) {
	lib, rel, _, status, msg := a.authorizedScope(r)
	return lib, rel, status, msg
}

// authorizedScope is authorizedPath plus the caller's scope, for an endpoint that
// resolves the path to a book (bookForPath), which may lie above it.
func (a *API) authorizedScope(r *http.Request) (*catalog.Library, string, catalog.Scope, int, string) {
	id, ok := pathInt(r, "id")
	if !ok {
		return nil, "", catalog.Scope{}, http.StatusBadRequest, "invalid library id"
	}
	lib, scope, status, msg := a.libraryScope(r, id)
	if status != 0 {
		return nil, "", scope, status, msg
	}
	// Normalize before the scope check so ".." can't smuggle an out-of-scope path
	// past a subtree grant (see catalog.CleanRelPath). An input that cleans away
	// to nothing ("", ".", "/", "Author/..") addresses no content.
	rel := catalog.CleanRelPath(r.URL.Query().Get("path"))
	if rel == "" {
		return nil, "", scope, http.StatusBadRequest, "path is required"
	}
	if !scope.Allows(rel) {
		return nil, "", scope, http.StatusForbidden, msgNoPathAccess
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
		return a.indexPath(ctx, *lib, path, scope.Allows)
	case err != nil:
		return nil, err
	case !scope.Allows(b.RelPath):
		return nil, library.ErrNotAllowed
	}
	return b, nil
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
	lib, scope, ok := a.browseScope(w, r)
	if !ok {
		return
	}
	var allow func(string) bool
	if !scope.AllowAll {
		allow = scope.VisibleInBrowse
	}
	listing, err := library.BrowseFS(lib.Root, r.URL.Query().Get("path"),
		queryInt(r, "offset", 0), queryInt(r, "limit", 200), allow, library.ParseIgnore(lib.IgnorePatterns))
	if errors.Is(err, library.ErrOutsideRoot) {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	if err != nil {
		writeError(w, http.StatusNotFound, "directory not found")
		return
	}
	a.annotateWithBooks(r, lib.ID, listing)
	writeJSON(w, http.StatusOK, listing)
}

// annotateWithBooks turns the raw filesystem listing into the hybrid view by
// attaching indexed metadata to entries that are books, so a browsing client
// sees titles/authors/durations alongside the raw tree.
func (a *API) annotateWithBooks(r *http.Request, libraryID int64, listing *library.Listing) {
	if len(listing.Entries) == 0 {
		return
	}
	paths := make([]string, len(listing.Entries))
	for i, e := range listing.Entries {
		paths[i] = e.Path
	}
	books, err := a.cat.BooksByPaths(r.Context(), libraryID, paths)
	if err != nil {
		a.log.Warn("annotate fs listing failed", "library", libraryID, "err", err)
		return
	}
	// Per-folder detection overrides let the console show/toggle a folder's
	// classification; best-effort, so a failure here doesn't drop the listing.
	overrides, err := a.cat.FolderOverrides(r.Context(), libraryID)
	if err != nil {
		a.log.Warn("annotate fs overrides failed", "library", libraryID, "err", err)
	}
	// Which folders a `book` override would join from their disc folders; likewise.
	// For an admin only (the console's Folders screen offers the join there): it is
	// no part of the player's listing, which stays as it was for everyone else.
	var split map[string]bool
	if u := userFrom(r.Context()); u != nil && u.Role == auth.RoleAdmin {
		if split, err = a.cat.SplitFolders(r.Context(), libraryID, paths); err != nil {
			a.log.Warn("annotate fs split discs failed", "library", libraryID, "err", err)
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
}

// handleListBooks serves the computed/hybrid view from the index, scoped to the
// caller's share path rules.
func (a *API) handleListBooks(w http.ResponseWriter, r *http.Request) {
	lib, scope, ok := a.browseScope(w, r)
	if !ok {
		return
	}
	page, err := a.cat.ListBooks(r.Context(), catalog.ListOptions{
		LibraryID: lib.ID,
		Author:    r.URL.Query().Get("author"),
		Series:    r.URL.Query().Get("series"),
		Narrator:  r.URL.Query().Get("narrator"),
		Sort:      r.URL.Query().Get("sort"),
		Limit:     queryInt(r, "limit", 50),
		Cursor:    r.URL.Query().Get("cursor"),
		Scope:     &scope,
	})
	if err != nil {
		a.writeCatalogError(w, err, "list books failed", "could not load books", "library", lib.ID)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// handleSearch runs a full-text search across the caller's accessible content,
// scoped per-library to their share path rules.
func (a *API) handleSearch(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	scopes, err := a.cat.UserScopes(r.Context(), u.ID, u.Role == "admin")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "search failed")
		return
	}
	books, err := a.cat.Search(r.Context(), r.URL.Query().Get("q"), scopes, queryInt(r, "limit", 50))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "search failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"books": books})
}

// handleRecentBooks returns the most recently added books across every library
// the caller can reach, scoped to their share path rules. A single cross-library
// endpoint so clients render one merged "recently added" list (rather than
// fanning out to each library's /books and concatenating).
func (a *API) handleRecentBooks(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	scopes, err := a.cat.UserScopes(r.Context(), u.ID, u.Role == "admin")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load books")
		return
	}
	books, err := a.cat.RecentBooks(r.Context(), scopes, queryInt(r, "limit", 50))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load books")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"books": books})
}

// handleItem returns full book detail (metadata + files + chapters + description)
// for a path, indexing it on demand if needed.
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
	dp := media.DirectPlayable(book.Codec)
	book.DirectPlayable = &dp
	writeJSON(w, http.StatusOK, book)
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
	// Emit [] rather than null for empty files/chapters so the envelope matches the
	// hand-mirrored client types (which declare these as non-null arrays). book is
	// a fresh per-request value, so normalizing in place is safe.
	if book.Files == nil {
		book.Files = []catalog.BookFile{}
	}
	if book.Chapters == nil {
		book.Chapters = []metadata.Chapter{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"library_id":      lib.ID,
		"path":            book.RelPath,
		"duration":        book.Duration,
		"is_folder":       book.IsFolder,
		"files":           book.Files,
		"chapters":        book.Chapters,
		"codec":           book.Codec,
		"direct_playable": media.DirectPlayable(book.Codec),
	})
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

// handleStream serves an audio file by path. By default it streams the file with
// Range support (?download=1 forces a download). With ?transcode=1 it re-encodes
// to MP3 via ffmpeg for codecs browsers can't decode; ?t=<seconds> starts that
// transcode mid-file (transcoded output is not byte-seekable, so seeking is by
// re-requesting). The path is the actual audio file (a chapter's file_path).
func (a *API) handleStream(w http.ResponseWriter, r *http.Request) {
	lib, rel, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	// Only ever stream recognized audio. The scope check bounds this to the caller's
	// grant, but the library root can also hold non-audio sidecar files (.nfo, .jpg,
	// a stray .env/backup); BrowseFS hides those from listings, and cover art has its
	// own endpoint, so serving them here would be an avenue to exfiltrate arbitrary
	// in-scope files by guessing paths.
	if !metadata.IsAudio(rel) {
		writeError(w, http.StatusNotFound, "not an audio file")
		return
	}
	abs, err := library.SafeJoin(lib.Root, rel)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	if r.URL.Query().Get("transcode") == "1" {
		if a.ffmpeg == "" {
			writeError(w, http.StatusServiceUnavailable, "transcoding is not available on this server")
			return
		}
		// Bound concurrent ffmpeg processes; when saturated, ask the client to retry
		// rather than forking another core-pinning encoder.
		select {
		case a.transcodeSem <- struct{}{}:
			defer func() { <-a.transcodeSem }()
		default:
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusServiceUnavailable, "server busy transcoding; try again shortly")
			return
		}
		a.streams.Note(credentialFrom(r.Context()).ID, lib.ID, rel, time.Now())
		start, _ := strconv.ParseFloat(r.URL.Query().Get("t"), 64)
		media.Transcode(w, r, abs, a.ffmpeg, start, a.log)
		return
	}
	media.ServeFile(w, r, abs, r.URL.Query().Get("download") == "1")
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
	art := book.ArtFiles()
	if art.CoverPath != "" {
		if abs, err := library.SafeJoin(lib.Root, art.CoverPath); err == nil {
			if fi, err := os.Stat(abs); err == nil && fi.Mode().IsRegular() {
				// The same lifetime as embedded art below. Without one a browser keeps
				// a sidecar image fresh by heuristic (a tenth of the file's age), so a
				// custom cover uploaded later would go unseen for weeks, not a day. Set
				// only for a file that is there, so a 404 is never cached.
				w.Header().Set("Cache-Control", "private, max-age=86400")
			}
			media.ServeFile(w, r, abs, false)
			return
		}
	}
	abs, err := library.SafeJoin(lib.Root, art.AudioPath)
	if err != nil {
		writeError(w, http.StatusNotFound, "no cover")
		return
	}
	data, mime, ok := media.EmbeddedCover(abs)
	if !ok {
		writeError(w, http.StatusNotFound, "no cover")
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(data)
}
