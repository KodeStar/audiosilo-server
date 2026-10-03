package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/metadata"
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

// authorizedPath resolves {id} + ?path=, checks the path is within the caller's
// scope, and returns the library + path. Used by every path-addressed endpoint.
func (a *API) authorizedPath(r *http.Request) (*catalog.Library, string, int, string) {
	id, ok := pathInt(r, "id")
	if !ok {
		return nil, "", http.StatusBadRequest, "invalid library id"
	}
	lib, scope, status, msg := a.libraryScope(r, id)
	if status != 0 {
		return nil, "", status, msg
	}
	// Normalize before the scope check so ".." can't smuggle an out-of-scope path
	// past a subtree grant (see catalog.CleanRelPath). An input that cleans away
	// to nothing ("", ".", "/", "Author/..") addresses no content.
	rel := catalog.CleanRelPath(r.URL.Query().Get("path"))
	if rel == "" {
		return nil, "", http.StatusBadRequest, "path is required"
	}
	if !scope.Allows(rel) {
		return nil, "", http.StatusForbidden, "no access to this path"
	}
	return lib, rel, 0, ""
}

// bookForPath returns the indexed book for a (library, path), indexing it on
// demand if the background scan has not reached it yet.
func (a *API) bookForPath(ctx context.Context, lib *catalog.Library, path string) (*catalog.Book, error) {
	if b, err := a.cat.GetBookByPath(ctx, lib.ID, path); err == nil {
		return b, nil
	} else if !errors.Is(err, catalog.ErrNotFound) {
		return nil, err
	}
	return a.scanner.IndexPath(ctx, *lib, path)
}

// handleBrowseFS serves the filtered filesystem view: the real directory tree,
// scoped to the caller's share path rules, requiring no prior indexing.
func (a *API) handleBrowseFS(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return
	}
	lib, scope, status, msg := a.libraryScope(r, id)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	var allow func(string) bool
	if !scope.AllowAll {
		allow = scope.VisibleInBrowse
	}
	listing, err := library.BrowseFS(lib.Root, r.URL.Query().Get("path"),
		queryInt(r, "offset", 0), queryInt(r, "limit", 200), allow)
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
	for i := range listing.Entries {
		e := &listing.Entries[i]
		if b, ok := books[e.Path]; ok {
			e.IsBook = true
			e.Title = b.Title
			e.Author = b.Author
			e.Series = b.Series
			e.SeriesIndex = b.SeriesIndex
			e.Duration = b.Duration
		}
		if m, ok := overrides[e.Path]; ok {
			e.Override = m
		}
	}
}

// handleListBooks serves the computed/hybrid view from the index, scoped to the
// caller's share path rules.
func (a *API) handleListBooks(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return
	}
	_, scope, status, msg := a.libraryScope(r, id)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	page, err := a.cat.ListBooks(r.Context(), catalog.ListOptions{
		LibraryID: id,
		Author:    r.URL.Query().Get("author"),
		Series:    r.URL.Query().Get("series"),
		Sort:      r.URL.Query().Get("sort"),
		Limit:     queryInt(r, "limit", 50),
		Cursor:    r.URL.Query().Get("cursor"),
		Scope:     &scope,
	})
	if err != nil {
		a.writeCatalogError(w, err, "list books failed", "could not load books", "library", id)
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

// handleItem returns full book detail (metadata + files + chapters) for a path,
// indexing it on demand if needed.
func (a *API) handleItem(w http.ResponseWriter, r *http.Request) {
	lib, path, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	book, err := a.bookForPath(r.Context(), lib, path)
	switch {
	case errors.Is(err, library.ErrNotIndexable):
		writeError(w, http.StatusNotFound, "no book at that path")
	case err != nil:
		a.writeCatalogError(w, err, "load book failed", "could not load book", "library", lib.ID, "path", path)
	default:
		dp := media.DirectPlayable(book.Codec)
		book.DirectPlayable = &dp
		writeJSON(w, http.StatusOK, book)
	}
}

// handleChapters returns a book's normalized playable units. Each chapter
// carries file_path so playback is purely path-based and a single chaptered m4b
// and a folder of mp3 parts render identically.
func (a *API) handleChapters(w http.ResponseWriter, r *http.Request) {
	lib, path, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	book, err := a.bookForPath(r.Context(), lib, path)
	switch {
	case errors.Is(err, library.ErrNotIndexable):
		writeError(w, http.StatusNotFound, "no book at that path")
		return
	case err != nil:
		a.writeCatalogError(w, err, "load chapters failed", "could not load chapters", "library", lib.ID, "path", path)
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
	if inm := r.Header.Get("If-None-Match"); inm != "" && strings.Contains(inm, coverETag(info.UpdatedAt)) {
		w.Header().Set("Cache-Control", "private, no-cache")
		w.Header().Set("ETag", coverETag(info.UpdatedAt))
		w.WriteHeader(http.StatusNotModified)
		return true, nil
	}
	cv, err := a.cat.Cover(r.Context(), libID, path)
	if errors.Is(err, catalog.ErrNotFound) {
		return false, nil // removed since the check above: fall back to the book's art
	}
	if err != nil {
		return false, err
	}
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("ETag", coverETag(cv.UpdatedAt))
	w.Header().Set("Content-Type", cv.MIME)
	// A zero modtime: no Last-Modified (see above); ServeContent still honours the
	// ETag for conditional and Range requests.
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(cv.Data))
	return true, nil
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

// bookArtPaths returns where a book's own art (not a custom cover) is read from:
// its sidecar image when the scanner recorded one, else the art embedded in its
// first audio file. A path that doesn't resolve inside the library root is "".
// A recorded sidecar wins even when it has since vanished (the cover is then
// missing, not silently swapped for embedded art).
func bookArtPaths(lib *catalog.Library, book *catalog.Book) (sidecar, audio string) {
	if book.CoverPath != "" {
		if abs, err := library.SafeJoin(lib.Root, book.CoverPath); err == nil {
			return abs, ""
		}
	}
	primary := book.RelPath
	if book.IsFolder && len(book.Files) > 0 {
		primary = book.Files[0].RelPath
	}
	if abs, err := library.SafeJoin(lib.Root, primary); err == nil {
		audio = abs
	}
	return "", audio
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
		start, _ := strconv.ParseFloat(r.URL.Query().Get("t"), 64)
		media.Transcode(w, r, abs, a.ffmpeg, start, a.log)
		return
	}
	media.ServeFile(w, r, abs, r.URL.Query().Get("download") == "1")
}

// handleCover serves a book's cover for a path: a custom cover uploaded in the
// admin console, else a sibling cover file if indexed, else embedded art from the
// book's primary audio file.
func (a *API) handleCover(w http.ResponseWriter, r *http.Request) {
	lib, path, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	if a.answerCustomCover(w, r, lib.ID, path) {
		return
	}
	book, err := a.bookForPath(r.Context(), lib, path)
	switch {
	case errors.Is(err, library.ErrNotIndexable):
		writeError(w, http.StatusNotFound, "no cover")
		return
	case err != nil:
		a.writeCatalogError(w, err, "load cover failed", "could not load cover", "library", lib.ID, "path", path)
		return
	}
	// The lookup above was by the requested path. A part path resolves to its folder
	// book, and bookForPath may have just indexed the book (a custom cover is served
	// only for an indexed one): either way the book's own path can carry a custom
	// cover, ahead of its own art.
	if a.answerCustomCover(w, r, lib.ID, book.RelPath) {
		return
	}
	sidecar, audio := bookArtPaths(lib, book)
	if sidecar != "" {
		if fi, err := os.Stat(sidecar); err == nil && fi.Mode().IsRegular() {
			// The same lifetime as embedded art below. Without one a browser keeps
			// a sidecar image fresh by heuristic (a tenth of the file's age), so a
			// custom cover uploaded later would go unseen for weeks, not a day. Set
			// only for a file that is there, so a 404 is never cached.
			w.Header().Set("Cache-Control", "private, max-age=86400")
		}
		media.ServeFile(w, r, sidecar, false)
		return
	}
	if audio == "" {
		writeError(w, http.StatusNotFound, "no cover")
		return
	}
	data, mime, ok := media.EmbeddedCover(audio)
	if !ok {
		writeError(w, http.StatusNotFound, "no cover")
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(data)
}
