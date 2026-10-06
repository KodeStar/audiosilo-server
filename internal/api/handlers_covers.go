package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"strconv"
	"sync"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/media"
)

// Cover thumbnails, for the player (GET /libraries/{id}/cover?size=, below) and
// the admin console's grids (POST /admin/covers); both make them in one place
// (coverArt, coverThumbnail), which also records the cover's colour on a book that
// has none for its current art (catalog.RecordCoverColors). The console
// can't load covers as plain <img> URLs: its session is a full-privilege admin
// bearer token, which must never ride in a URL (proxy logs, history), and its CSP
// allows images only from 'self' and data:. So it fetches covers itself and shows
// them as data: URLs - and a grid of a few hundred covers must not cost a few
// hundred requests or megabytes of full-size art for 158px tiles. One request
// returns up to maxCoverBatch small JPEG thumbnails, each already a data: URL.

const (
	// maxCoverBatch caps one request: about a screenful of a cover grid.
	maxCoverBatch = 60
	// defaultThumbSize is the longest side of a thumbnail in pixels: a 158px tile
	// on a 2x display.
	defaultThumbSize = 320
	// maxSidecarBytes bounds a sidecar image read whole to thumbnail it.
	maxSidecarBytes = 32 << 20
	// thumbCacheBytes bounds the in-memory thumbnail cache (~20 KB a 320px cover
	// as JPEG).
	thumbCacheBytes = 48 << 20
	// maxConcurrentThumbnails caps decodes across all requests: a large cover
	// decodes to tens of megabytes.
	maxConcurrentThumbnails = 4
)

// thumbSizes are the sizes a client may ask for; a fixed set keeps the cache
// from holding one copy per arbitrary size.
var thumbSizes = []int{160, defaultThumbSize, 640}

// coverThumb is one entry of the response, in request order. Data is a data: URL,
// or "" when the book has no cover art (or no book is indexed at the path).
type coverThumb struct {
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"`
	Data      string `json:"data"`
}

// maxCoverReads bounds one request's concurrent art reads (a slow network mount
// shouldn't get 60 at once); decodes are bounded across requests by thumbSem.
const maxCoverReads = 8

// handleAdminCovers serves POST /admin/covers {"books":[{library_id,path}],"size"}.
func (a *API) handleAdminCovers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Books []catalog.Ref `json:"books"`
		Size  int           `json:"size"`
	}
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	switch {
	case len(req.Books) == 0:
		writeError(w, http.StatusBadRequest, "books is required")
		return
	case len(req.Books) > maxCoverBatch:
		writeErrorCode(w, http.StatusBadRequest, codeTooLarge,
			fmt.Sprintf("too many covers in one request (at most %d)", maxCoverBatch))
		return
	case req.Size == 0:
		req.Size = defaultThumbSize
	case !slices.Contains(thumbSizes, req.Size):
		writeError(w, http.StatusBadRequest, fmt.Sprintf("size must be one of %v", thumbSizes))
		return
	}
	ctx := r.Context()
	libs, err := a.cat.ListLibraries(ctx)
	if err != nil {
		a.writeCatalogError(w, err, "list libraries for covers failed", "could not load covers")
		return
	}
	byID := make(map[int64]*catalog.Library, len(libs))
	for i := range libs {
		byID[libs[i].ID] = &libs[i]
	}
	// One lookup per library for the whole batch, before any file is touched.
	paths := make([]string, len(req.Books))
	byLib := map[int64][]string{}
	for i, ref := range req.Books {
		paths[i] = catalog.CleanRelPath(ref.Path)
		if byID[ref.LibraryID] != nil {
			byLib[ref.LibraryID] = append(byLib[ref.LibraryID], paths[i])
		}
	}
	sources := make(map[int64]map[string]catalog.CoverSource, len(byLib))
	for libID, ps := range byLib {
		if sources[libID], err = a.cat.CoverSources(ctx, libID, ps); err != nil {
			a.writeCatalogError(w, err, "cover sources failed", "could not load covers")
			return
		}
	}

	out := make([]coverThumb, len(req.Books))
	recs := make([]*catalog.CoverColorRecord, len(req.Books))
	reads := make(chan struct{}, maxCoverReads)
	var wg sync.WaitGroup
	for i, ref := range req.Books {
		out[i] = coverThumb{LibraryID: ref.LibraryID, Path: ref.Path}
		src, ok := sources[ref.LibraryID][paths[i]]
		if !ok {
			continue // an unknown library, or no book indexed at the path: no art
		}
		lib := byID[ref.LibraryID]
		wg.Go(func() {
			art := a.coverArt(ctx, lib, paths[i], src)
			if art == nil {
				return
			}
			// An image that can't be read is "" for now (not cached, so the next
			// page load tries again); one that can't be decoded is "" for good.
			jpg, rec, err := a.coverThumbnail(ctx, lib, paths[i], src, art, req.Size, reads)
			if err != nil || jpg == nil {
				return
			}
			out[i].Data = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpg)
			recs[i] = rec
		})
	}
	wg.Wait()
	a.recordCoverColors(ctx, recs...)
	writeJSON(w, http.StatusOK, map[string]any{"covers": out})
}

// handleCoverThumbnail serves GET /libraries/{id}/cover?size= for a path the
// caller's scope allows: the cover of the book at the path (else of the book
// holding it, bookForPath, with its 403/404) as a JPEG thumbnail of one of
// thumbSizes, from the same art, cache and decode bound as POST /admin/covers.
// The ETag is the art's version and the size, so revalidating an unchanged cover
// is a 304 with nothing read; a custom cover is revalidated each time (it can be
// replaced at any moment), file art is fresh for a day like full art.
func (a *API) handleCoverThumbnail(w http.ResponseWriter, r *http.Request, lib *catalog.Library, path string, scope catalog.Scope) {
	size, err := strconv.Atoi(r.URL.Query().Get("size"))
	if err != nil || !slices.Contains(thumbSizes, size) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("size must be one of %v", thumbSizes))
		return
	}
	ctx := r.Context()
	sources, err := a.cat.CoverSources(ctx, lib.ID, []string{path})
	if err != nil {
		a.writeCatalogError(w, err, "cover sources failed", "could not load cover", "library", lib.ID, "path", path)
		return
	}
	src, ok := sources[path]
	if !ok {
		// No book indexed at the path itself: a part path, a disc folder, or a
		// book not indexed yet.
		book, found := a.bookAt(w, r, lib, scope, path, "no cover", "could not load cover")
		if !found {
			return
		}
		path = book.RelPath
		if sources, err = a.cat.CoverSources(ctx, lib.ID, []string{path}); err != nil {
			a.writeCatalogError(w, err, "cover sources failed", "could not load cover", "library", lib.ID, "path", path)
			return
		}
		src, ok = sources[path]
	}
	var art *artSource
	if ok {
		art = a.coverArt(ctx, lib, path, src)
	}
	if art == nil {
		writeError(w, http.StatusNotFound, "no cover")
		return
	}
	cond := conditional{etag: `"thumb-` + strconv.Itoa(size) + "-" + catalog.CoverVersion(art.version) + `"`,
		cacheControl: "private, max-age=86400"}
	if src.CustomAt != "" {
		cond.cacheControl = customCoverCache
	}
	if cond.notModified(w, r) {
		return
	}
	jpg, rec, err := a.coverThumbnail(ctx, lib, path, src, art, size, make(chan struct{}, 1))
	switch {
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not load cover")
		return
	case jpg == nil:
		writeError(w, http.StatusNotFound, "no cover")
		return
	}
	a.recordCoverColors(ctx, rec)
	cond.serve(w, r, "image/jpeg", jpg)
}

// artSource is where a book's cover comes from: a version that changes whenever
// the art does (the thumbnail cache key, and the thumbnail's ETag), and how to
// read the image. load returns (nil, nil) when the source turns out to hold no
// art, and an error only for a failure worth retrying (an unreadable network
// mount).
type artSource struct {
	version string
	load    func() ([]byte, error)
}

// coverThumbnail returns the art's JPEG thumbnail at size through the shared
// cache, nil when the art can't be decoded (cached, so it isn't re-read every
// time); an error is a read that failed or was cancelled, not cached, so the next
// request tries again. When the book at path has no colour for its current art
// (src), rec is the colour read from the thumbnail, to record
// (recordCoverColors); else nil. reads bounds the caller's concurrent art reads;
// thumbSem bounds decodes across requests.
func (a *API) coverThumbnail(ctx context.Context, lib *catalog.Library, path string, src catalog.CoverSource,
	art *artSource, size int, reads chan struct{}) (jpg []byte, rec *catalog.CoverColorRecord, err error) {
	key := strconv.FormatInt(lib.ID, 10) + "\x00" + path + "\x00" + strconv.Itoa(size) + "\x00" + art.version
	jpg, ok := a.thumbs.Get(key)
	if !ok {
		if jpg, err = a.makeThumbnail(ctx, lib, path, art, size, reads); err != nil {
			return nil, nil, err
		}
		a.thumbs.Put(key, jpg)
	}
	if jpg == nil || src.Colored {
		return jpg, nil, nil
	}
	palette, err := media.PaletteOf(jpg)
	if err != nil {
		a.log.Debug("cover palette failed", "err", err, "library", lib.ID, "path", path)
		return jpg, nil, nil
	}
	return jpg, &catalog.CoverColorRecord{LibraryID: lib.ID, Path: path, Art: src.Art, Color: palette}, nil
}

// makeThumbnail reads the art and scales it to size: nil when there is no art or
// it can't be decoded, an error when the read failed or ctx ended.
func (a *API) makeThumbnail(ctx context.Context, lib *catalog.Library, path string, art *artSource, size int, reads chan struct{}) ([]byte, error) {
	select {
	case reads <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	raw, err := art.load()
	<-reads
	if errors.Is(err, media.ErrImageTooLarge) {
		// An oversized sidecar stays oversized: no art (cached), not a retry that
		// re-reads it on every page.
		raw, err = nil, nil
	}
	if err != nil {
		a.log.Debug("read cover art failed", "err", err, "library", lib.ID, "path", path)
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	select {
	case a.thumbSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	jpg, err := media.Thumbnail(raw, size)
	<-a.thumbSem
	if err != nil {
		a.log.Debug("cover thumbnail failed", "err", err, "library", lib.ID, "path", path)
		return nil, nil
	}
	return jpg, nil
}

// recordCoverColors stores the colours coverThumbnail read (nil entries are
// skipped). A failure only costs the colours until the next thumbnail, so it is
// logged, not answered.
func (a *API) recordCoverColors(ctx context.Context, recs ...*catalog.CoverColorRecord) {
	var batch []catalog.CoverColorRecord
	for _, r := range recs {
		if r != nil {
			batch = append(batch, *r)
		}
	}
	if err := a.cat.RecordCoverColors(ctx, batch); err != nil {
		a.log.Warn("record cover colours failed", "err", err)
	}
}

// coverArt is where the book at path takes its cover from, in the order
// GET /libraries/{id}/cover serves them (catalog.CoverSource): its custom cover,
// its sidecar image (inside the library root), the art embedded in its audio.
func (a *API) coverArt(ctx context.Context, lib *catalog.Library, path string, src catalog.CoverSource) *artSource {
	if src.CustomAt != "" {
		return &artSource{version: catalog.CustomArtVersion(src.CustomAt), load: func() ([]byte, error) {
			cv, err := a.cat.Cover(ctx, lib.ID, path)
			if errors.Is(err, catalog.ErrNotFound) {
				return nil, nil // removed a moment ago
			}
			if err != nil {
				return nil, err
			}
			return cv.Data, nil
		}}
	}
	if src.CoverPath != "" {
		if abs, err := library.SafeJoin(lib.Root, src.CoverPath); err == nil {
			return fileArt("s", abs, func() ([]byte, error) {
				f, err := os.Open(abs)
				if err != nil {
					return nil, err
				}
				defer f.Close()
				return media.ReadLimited(f, maxSidecarBytes)
			})
		}
	}
	if abs, err := library.SafeJoin(lib.Root, src.AudioPath); err == nil {
		return fileArt("e", abs, func() ([]byte, error) {
			data, _, _ := media.EmbeddedCover(abs)
			return data, nil
		})
	}
	return nil
}

// fileArt is art read from a file, versioned by its size and modification time. A
// file that is gone or not a regular file has no art.
func fileArt(kind, abs string, load func() ([]byte, error)) *artSource {
	fi, err := os.Stat(abs)
	if err != nil || !fi.Mode().IsRegular() {
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			// Unreadable rather than missing: try again next time.
			return &artSource{version: "x", load: func() ([]byte, error) { return nil, err }}
		}
		return nil
	}
	return &artSource{version: fmt.Sprintf("%s%d-%d", kind, fi.Size(), fi.ModTime().UnixNano()), load: load}
}
