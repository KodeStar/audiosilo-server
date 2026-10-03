package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"sync"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/media"
)

// Cover thumbnails for the admin console's grids (POST /admin/covers). The console
// can't load covers as plain <img> URLs: its session is a full-privilege admin
// bearer token, which must never ride in a URL (proxy logs, history), and its CSP
// allows images only from 'self' and data:. So it fetches covers itself and shows
// them as data: URLs - and a grid of a few hundred covers must not cost a few
// hundred requests (the per-IP limiter allows a burst of 40) or megabytes of
// full-size art for 158px tiles. One request returns up to maxCoverBatch small
// JPEG thumbnails, each already a data: URL.

const (
	// maxCoverBatch caps one request: about a screenful of a cover grid.
	maxCoverBatch = 60
	// defaultThumbSize is the longest side of a thumbnail in pixels: a 158px tile
	// on a 2x display.
	defaultThumbSize = 320
	// maxSidecarBytes bounds a sidecar image read whole to thumbnail it.
	maxSidecarBytes = 32 << 20
	// thumbCacheBytes bounds the in-memory thumbnail cache (~20 KB a cover).
	thumbCacheBytes = 48 << 20
	// maxConcurrentThumbnails caps decodes across all requests: a large cover
	// decodes to tens of megabytes.
	maxConcurrentThumbnails = 4
)

// thumbSizes are the sizes a client may ask for; a fixed set keeps the cache
// from holding one copy per arbitrary size.
var thumbSizes = map[int]bool{160: true, defaultThumbSize: true, 640: true}

// coverThumb is one entry of the response, in request order. Data is a data: URL,
// or "" when the book has no cover art (or no book is indexed at the path).
type coverThumb struct {
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"`
	Data      string `json:"data"`
}

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
		writeErrorCode(w, http.StatusBadRequest, codeTooLarge, "too many covers in one request (at most 60)")
		return
	case req.Size == 0:
		req.Size = defaultThumbSize
	case !thumbSizes[req.Size]:
		writeError(w, http.StatusBadRequest, "size must be 160, 320 or 640")
		return
	}
	libs, err := a.cat.ListLibraries(r.Context())
	if err != nil {
		a.writeCatalogError(w, err, "list libraries for covers failed", "could not load covers")
		return
	}
	byID := make(map[int64]*catalog.Library, len(libs))
	for i := range libs {
		byID[libs[i].ID] = &libs[i]
	}

	out := make([]coverThumb, len(req.Books))
	jobs := make(chan int)
	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	for range min(maxConcurrentThumbnails, len(req.Books)) {
		wg.Go(func() {
			for i := range jobs {
				ref := req.Books[i]
				out[i] = coverThumb{LibraryID: ref.LibraryID, Path: ref.Path}
				lib := byID[ref.LibraryID]
				if lib == nil {
					continue // an unknown library has no covers
				}
				thumb, err := a.coverThumbnail(r.Context(), lib, catalog.CleanRelPath(ref.Path), req.Size)
				if err != nil {
					errOnce.Do(func() { firstErr = err })
					continue
				}
				if thumb != nil {
					out[i].Data = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(thumb)
				}
			}
		})
	}
	for i := range req.Books {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		a.writeCatalogError(w, firstErr, "cover thumbnails failed", "could not load covers")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"covers": out})
}

// artSource is where a book's cover comes from: a version that changes whenever
// the art does (the thumbnail cache key), and how to read the image. load returns
// (nil, nil) when the source turns out to hold no art, and an error only for a
// failure worth retrying (an unreadable network mount).
type artSource struct {
	version string
	load    func() ([]byte, error)
}

// coverThumbnail returns the JPEG thumbnail of the book at path, or nil when it has
// no usable art. Errors are database failures only; an image that can't be read
// or decoded is "no art" (and logged).
func (a *API) coverThumbnail(ctx context.Context, lib *catalog.Library, path string, size int) ([]byte, error) {
	src, err := a.coverArt(ctx, lib, path)
	if err != nil || src == nil {
		return nil, err
	}
	key := strconv.FormatInt(lib.ID, 10) + "\x00" + path + "\x00" + strconv.Itoa(size) + "\x00" + src.version
	if thumb, ok := a.thumbs.Get(key); ok {
		return thumb, nil
	}
	select {
	case a.thumbSem <- struct{}{}:
		defer func() { <-a.thumbSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	raw, err := src.load()
	if err != nil {
		// A read failure may be transient (a network mount): not cached, so the
		// next page load tries again.
		a.log.Debug("read cover art failed", "err", err, "library", lib.ID, "path", path)
		return nil, nil
	}
	var thumb []byte
	if raw != nil {
		if thumb, err = media.Thumbnail(raw, size); err != nil {
			a.log.Debug("cover thumbnail failed", "err", err, "library", lib.ID, "path", path)
			thumb = nil
		}
	}
	a.thumbs.Put(key, thumb)
	return thumb, nil
}

// coverArt resolves a book's cover the way GET /libraries/{id}/cover does: a custom
// cover at the path, then at the book the path resolves to, then the book's sidecar
// image, then its embedded art. nil when no book is indexed at the path. It never
// indexes on demand (the console asks for books it listed).
func (a *API) coverArt(ctx context.Context, lib *catalog.Library, path string) (*artSource, error) {
	if src, err := a.customArt(ctx, lib.ID, path); src != nil || err != nil {
		return src, err
	}
	book, err := a.cat.GetBookByPath(ctx, lib.ID, path)
	if errors.Is(err, catalog.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if book.RelPath != path {
		if src, err := a.customArt(ctx, lib.ID, book.RelPath); src != nil || err != nil {
			return src, err
		}
	}
	sidecar, audio := bookArtPaths(lib, book)
	switch {
	case sidecar != "":
		return fileArt("s", sidecar, func() ([]byte, error) {
			f, err := os.Open(sidecar)
			if err != nil {
				return nil, err
			}
			defer f.Close()
			return media.ReadLimited(f, maxSidecarBytes)
		}), nil
	case audio != "":
		return fileArt("e", audio, func() ([]byte, error) {
			data, _, _ := media.EmbeddedCover(audio)
			return data, nil
		}), nil
	}
	return nil, nil
}

// customArt is the custom cover uploaded for path, if any.
func (a *API) customArt(ctx context.Context, libID int64, path string) (*artSource, error) {
	info, err := a.cat.CoverInfo(ctx, libID, path)
	if errors.Is(err, catalog.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &artSource{version: "c" + info.UpdatedAt, load: func() ([]byte, error) {
		cv, err := a.cat.Cover(ctx, libID, path)
		if errors.Is(err, catalog.ErrNotFound) {
			return nil, nil // removed a moment ago
		}
		if err != nil {
			return nil, err
		}
		return cv.Data, nil
	}}, nil
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
