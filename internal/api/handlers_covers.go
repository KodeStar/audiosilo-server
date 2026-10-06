package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/media"
)

// Cover thumbnails, for the player (GET /libraries/{id}/cover?size=, below) and
// the admin console's grids (POST /admin/covers); both make them in one place
// (coverArt, coverThumbnail) and record the cover's colours and version on the
// book from the thumbnail (catalog.RecordCoverColors). The console
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
	reads := make(chan struct{}, maxCoverReads)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var recs []catalog.CoverColorRecord
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
			thumb, err := a.coverThumbnail(ctx, lib, paths[i], art, req.Size, reads)
			if err != nil || thumb.JPEG == nil {
				return
			}
			out[i].Data = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(thumb.JPEG)
			if rec, ok := coverColorRecord(lib.ID, paths[i], src, art, thumb); ok {
				mu.Lock()
				recs = append(recs, rec)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	a.recordCoverColors(ctx, recs...)
	writeJSON(w, http.StatusOK, map[string]any{"covers": out})
}

// handleCoverThumbnail serves GET /libraries/{id}/cover?size= for a path the
// caller's scope allows: the book's cover as a JPEG thumbnail of one of thumbSizes,
// from the same art, cache and decode bound as POST /admin/covers. The ETag is
// the art's version and the size, so a client revalidating an unchanged cover
// gets a 304 without anything being read; a custom cover is revalidated each time
// (it can be replaced at any moment), file art is fresh for a day like full art.
func (a *API) handleCoverThumbnail(w http.ResponseWriter, r *http.Request, lib *catalog.Library, path string, scope catalog.Scope) {
	size, err := strconv.Atoi(r.URL.Query().Get("size"))
	if err != nil || !slices.Contains(thumbSizes, size) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("size must be one of %v", thumbSizes))
		return
	}
	ctx := r.Context()
	book, err := a.bookForPath(ctx, lib, scope, path)
	switch {
	case errors.Is(err, library.ErrNotAllowed):
		writeError(w, http.StatusForbidden, msgNoPathAccess)
		return
	case errors.Is(err, library.ErrNotIndexable):
		writeError(w, http.StatusNotFound, "no cover")
		return
	case err != nil:
		a.writeCatalogError(w, err, "load cover failed", "could not load cover", "library", lib.ID, "path", path)
		return
	}
	sources, err := a.cat.CoverSources(ctx, lib.ID, []string{book.RelPath})
	if err != nil {
		a.writeCatalogError(w, err, "cover sources failed", "could not load cover", "library", lib.ID, "path", path)
		return
	}
	src, ok := sources[book.RelPath]
	var art *artSource
	if ok {
		art = a.coverArt(ctx, lib, book.RelPath, src)
	}
	if art == nil {
		writeError(w, http.StatusNotFound, "no cover")
		return
	}
	etag := `"thumb-` + strconv.Itoa(size) + "-" + catalog.CoverVersion(art.version) + `"`
	// Set only once there is art to answer with, so a 404 is never cached.
	setHeaders := func() {
		cacheControl := "private, max-age=86400"
		if src.CustomAt != "" {
			cacheControl = "private, no-cache"
		}
		w.Header().Set("Cache-Control", cacheControl)
		w.Header().Set("ETag", etag)
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" && strings.Contains(inm, etag) {
		setHeaders()
		w.WriteHeader(http.StatusNotModified)
		return
	}
	thumb, err := a.coverThumbnail(ctx, lib, book.RelPath, art, size, make(chan struct{}, 1))
	switch {
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not load cover")
		return
	case thumb.JPEG == nil:
		writeError(w, http.StatusNotFound, "no cover")
		return
	}
	if rec, ok := coverColorRecord(lib.ID, book.RelPath, src, art, thumb); ok {
		a.recordCoverColors(ctx, rec)
	}
	setHeaders()
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(thumb.JPEG))
}

// artSource is where a book's cover comes from: a version that changes whenever
// the art does (the thumbnail cache key, and what CoverVersion hashes), and how to
// read the image. load returns (nil, nil) when the source turns out to hold no
// art, and an error only for a failure worth retrying (an unreadable network
// mount).
type artSource struct {
	version string
	load    func() ([]byte, error)
}

// coverThumbnail returns the art's thumbnail at size, with the palette read from
// it, through the shared cache: a Thumb with no JPEG when the art can't be
// decoded (cached, so it isn't re-read every time). An error is a read that
// failed or was cancelled, not cached, so the next request tries again. reads
// bounds the caller's concurrent art reads; thumbSem bounds decodes across
// requests.
func (a *API) coverThumbnail(ctx context.Context, lib *catalog.Library, path string, art *artSource, size int, reads chan struct{}) (media.Thumb, error) {
	key := strconv.FormatInt(lib.ID, 10) + "\x00" + path + "\x00" + strconv.Itoa(size) + "\x00" + art.version
	if thumb, ok := a.thumbs.Get(key); ok {
		return thumb, nil
	}
	select {
	case reads <- struct{}{}:
	case <-ctx.Done():
		return media.Thumb{}, ctx.Err()
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
		return media.Thumb{}, err
	}
	var thumb media.Thumb
	if raw != nil {
		select {
		case a.thumbSem <- struct{}{}:
		case <-ctx.Done():
			return media.Thumb{}, ctx.Err()
		}
		jpg, palette, err := media.Thumbnail(raw, size)
		<-a.thumbSem
		if err != nil {
			a.log.Debug("cover thumbnail failed", "err", err, "library", lib.ID, "path", path)
		} else {
			thumb = media.Thumb{JPEG: jpg, Palette: palette}
		}
	}
	a.thumbs.Put(key, thumb)
	return thumb, nil
}

// coverColorRecord is what a thumbnail of art tells the book at path about its
// cover (catalog.RecordCoverColors), and false when the book already holds it: the
// same version with colours, whichever size they were read from (so the sizes
// don't take turns rewriting near-identical colours). A cache hit records too, so
// a version cleared by a re-index or DeleteCover is restored without a decode.
func coverColorRecord(libID int64, path string, src catalog.CoverSource, art *artSource, thumb media.Thumb) (catalog.CoverColorRecord, bool) {
	version := catalog.CoverVersion(art.version)
	if thumb.Palette.Bg == "" || (src.Version == version && src.Colored) {
		return catalog.CoverColorRecord{}, false
	}
	return catalog.CoverColorRecord{LibraryID: libID, Path: path, Source: src, Version: version,
		Color: catalog.CoverColor(thumb.Palette)}, true
}

// recordCoverColors stores thumbnails' colours on their books. A failure only
// costs the colours until the next thumbnail, so it is logged, not answered.
func (a *API) recordCoverColors(ctx context.Context, recs ...catalog.CoverColorRecord) {
	if err := a.cat.RecordCoverColors(ctx, recs); err != nil {
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
