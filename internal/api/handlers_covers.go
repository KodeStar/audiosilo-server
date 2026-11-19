package api

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/covercolors"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/metadata"
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

// thumbSizes are the sizes a client may ask for, smallest first; a fixed set
// keeps the cache from holding one copy per arbitrary size. 96 is for a table
// row's 36-48px cover.
var thumbSizes = []int{96, colorThumbSize, defaultThumbSize, 640}

// colorThumbSize is the thumbnail colorCover makes when none is cached.
const colorThumbSize = 160

// coverThumb is one entry of the response, in request order. Data is a data: URL,
// or "" when the book has no cover art (or no book is indexed at the path).
// Version is the book's cover_version for this art, as its admin row has it once
// the request's colours are recorded (a first thumbnail moves the row's version
// to the art's own, RecordCoverColors), so the console files a thumbnail under
// the version a reload's row names: never under an older row's version that
// asked for it (the art has moved on since that row was read).
type coverThumb struct {
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"`
	Data      string `json:"data"`
	Version   string `json:"version,omitempty"`
}

// maxCoverReads bounds the art reads in progress across all requests (a slow
// network mount shouldn't get a grid's 60 at once), and so how many images are
// held in memory waiting to be decoded; decodes are bounded by thumbSem.
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
	if !thumbBatch(w, "books", len(req.Books), maxCoverBatch, &req.Size) || refsTooLong(w, req.Books) {
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
	// One lookup per known library for the whole batch, before any file is touched.
	paths, byLib := catalog.GroupRefs(req.Books)
	sources := make(map[int64]map[string]catalog.CoverSource, len(byLib))
	for libID, ps := range byLib {
		if byID[libID] == nil {
			continue // an unknown library: no art
		}
		if sources[libID], err = a.cat.CoverSources(ctx, libID, ps); err != nil {
			a.writeCatalogError(w, err, "cover sources failed", "could not load covers")
			return
		}
	}

	out := make([]coverThumb, len(req.Books))
	recs := make([]*catalog.CoverColorRecord, len(req.Books))
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
			jpg, rec, err := a.coverThumbnail(ctx, lib, paths[i], src, art, req.Size)
			if err != nil || jpg == nil {
				return
			}
			out[i].Data = jpegDataURL(jpg)
			// The version the row has once this colour is recorded (as
			// RecordCoverColors moves it), else the one it has now.
			identity := src.Art
			if rec != nil {
				identity = cmp.Or(rec.Version, rec.Art)
			}
			out[i].Version = catalog.CoverVersion(identity)
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
	size, ok := thumbSizeParam(w, r.URL.Query().Get("size"))
	if !ok {
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
	cond := conditional{etag: thumbETag("thumb", size, catalog.CoverVersion(art.version)), cacheControl: coverCache}
	if src.CustomAt != "" {
		cond.cacheControl = customCoverCache
	}
	// A book without a colour for this art goes on to read it (usually from the
	// thumbnail cache) even for a client revalidating its copy: conditional.serve
	// still answers that with a 304, and the colour is not left missing for as
	// long as every client holding the thumbnail only ever revalidates it.
	if src.Colored && src.Art == art.version && cond.notModified(w, r) {
		return
	}
	jpg, rec, err := a.coverThumbnail(ctx, lib, path, src, art, size)
	switch {
	case metadata.GaveUp(err):
		writeShareNotAnswering(w)
		return
	case err != nil:
		if ctx.Err() == nil {
			a.log.Warn("cover thumbnail failed", "err", err, "library", lib.ID, "path", path)
		}
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
// request tries again. Unless the book at path already holds a colour for this
// very art (its cover art identity is the art's version, and it is coloured),
// rec is the colour read from the thumbnail, to record (recordCoverColors); else
// nil. The identity check is what notices a sidecar replaced in place, which no
// index change shows.
func (a *API) coverThumbnail(ctx context.Context, lib *catalog.Library, path string, src catalog.CoverSource,
	art *artSource, size int) (jpg []byte, rec *catalog.CoverColorRecord, err error) {
	key := thumbKey(lib.ID, path, size, art.version)
	jpg, ok := a.thumbs.Get(key)
	if !ok {
		if jpg, err = a.makeThumbnail(ctx, lib, path, art, size); err != nil {
			return nil, nil, err
		}
		a.thumbs.Put(key, jpg)
	}
	if jpg == nil || (src.Colored && src.Art == art.version) {
		return jpg, nil, nil
	}
	return jpg, a.colorRecord(lib.ID, path, src, art, jpg), nil
}

// thumbETag is the validator of a thumbnail at size of art whose
// catalog.CoverVersion is version, kind naming the route ("thumb", "podcast",
// "community"). It carries media.ThumbnailRev, so it changes when what
// media.Thumbnail makes of the same art does: a strong validator names one body.
func thumbETag(kind string, size int, version string) string {
	return `"` + kind + "-" + strconv.Itoa(size) + "-" + media.ThumbnailRev + "-" + version + `"`
}

// thumbKey is the thumbnail cache's key for a book's art at size.
func thumbKey(libraryID int64, path string, size int, version string) string {
	return strconv.FormatInt(libraryID, 10) + "\x00" + path + "\x00" + strconv.Itoa(size) + "\x00" + version
}

// colorRecord is the colour read from jpg, a thumbnail of the book's art, to
// record against the art src was read under; nil when no palette could be read.
func (a *API) colorRecord(libraryID int64, path string, src catalog.CoverSource, art *artSource, jpg []byte) *catalog.CoverColorRecord {
	palette, err := media.PaletteOf(jpg)
	if err != nil {
		a.log.Debug("cover palette failed", "err", err, "library", libraryID, "path", path)
		return nil
	}
	return &catalog.CoverColorRecord{LibraryID: libraryID, Path: path, Art: src.Art, Version: art.version, Color: palette}
}

// colorCover is the background colour pass's Colorer (covercolors): the colour of
// a due book's cover, read like a thumbnail's. A thumbnail of the art already in
// the cache is used as it is; otherwise a colorThumbSize one is made and not
// cached, so a pass over a library never pushes out the thumbnails people are
// looking at. A book with no art, or none that decodes, gets a record of that (a
// zero Color); one whose art files aren't there is covercolors.ErrArtMissing, not
// "no colour" (an unmounted share). The record is compare-and-set like a
// thumbnail's, so art that has moved on since the book was listed keeps what it has.
func (a *API) colorCover(ctx context.Context, lib *catalog.Library, due catalog.CoverColorDue) (catalog.CoverColorRecord, error) {
	art := a.coverArt(ctx, lib, due.Path, due.Source)
	if art == nil {
		return catalog.CoverColorRecord{}, covercolors.ErrArtMissing
	}
	none := catalog.CoverColorRecord{LibraryID: lib.ID, Path: due.Path, Art: due.Source.Art, Version: art.version}
	var jpg []byte
	cached := false
	for _, size := range thumbSizes {
		if jpg, cached = a.thumbs.Get(thumbKey(lib.ID, due.Path, size, art.version)); cached {
			break
		}
	}
	if !cached {
		var err error
		if jpg, err = a.makeThumbnail(ctx, lib, due.Path, art, colorThumbSize); err != nil {
			return none, err
		}
	}
	if jpg == nil {
		return none, nil
	}
	if rec := a.colorRecord(lib.ID, due.Path, due.Source, art, jpg); rec != nil {
		return *rec, nil
	}
	return none, nil
}

// makeThumbnail reads the art and scales it to size: nil when there is no art or
// it can't be decoded, an error when the read failed or ctx ended. A coverReads
// slot is held from the read to the end of the decode, so however many requests
// want thumbnails, only that many images are ever held in memory (and read from
// a slow mount at once); thumbSem, taken after it, bounds the decodes.
func (a *API) makeThumbnail(ctx context.Context, lib *catalog.Library, path string, art *artSource, size int) ([]byte, error) {
	select {
	case a.coverReads <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-a.coverReads }()
	raw, err := art.load()
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
	return a.decodeThumbnail(ctx, media.Thumbnail, raw, size, "library", lib.ID, "path", path)
}

// decodeThumbnail scales art to size with thumb (media.Thumbnail, or
// media.ThumbnailJPEG so a small JPEG never comes back as its own bytes), one of
// the decodes thumbSem bounds: nil when the art can't be decoded, an error only
// when ctx ended. logArgs name the art in the debug log.
func (a *API) decodeThumbnail(ctx context.Context, thumb func([]byte, int) ([]byte, error), raw []byte, size int, logArgs ...any) ([]byte, error) {
	select {
	case a.thumbSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	jpg, err := thumb(raw, size)
	<-a.thumbSem
	if err != nil {
		a.log.Debug("cover thumbnail failed", append([]any{"err", err}, logArgs...)...)
		return nil, nil
	}
	return jpg, nil
}

// jpegDataURL is a JPEG as the data: URL the console shows it by.
func jpegDataURL(jpg []byte) string {
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpg)
}

// thumbSizeParam is a thumbnail route's ?size=, one of thumbSizes; false when it
// wrote the 400.
func thumbSizeParam(w http.ResponseWriter, raw string) (int, bool) {
	n, err := strconv.Atoi(raw)
	if err != nil || !slices.Contains(thumbSizes, n) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("size must be one of %v", thumbSizes))
		return 0, false
	}
	return n, true
}

// optThumbSize is a thumbnail route's optional ?size=: defaultThumbSize when
// unset, else thumbSizeParam's; false when it wrote the 400.
func optThumbSize(w http.ResponseWriter, q url.Values) (int, bool) {
	if !q.Has("size") {
		return defaultThumbSize, true
	}
	return thumbSizeParam(w, q.Get("size"))
}

// thumbBatch checks a thumbnail batch: its list (named field) holds 1 to max
// entries, and its size is one of thumbSizes, an unset one defaulting to
// defaultThumbSize; false when it wrote the 400.
func thumbBatch(w http.ResponseWriter, field string, n, max int, size *int) bool {
	switch {
	case n == 0:
		writeError(w, http.StatusBadRequest, field+" is required")
		return false
	case n > max:
		writeErrorCode(w, http.StatusBadRequest, codeTooLarge,
			fmt.Sprintf("too many covers in one request (at most %d)", max))
		return false
	case *size == 0:
		*size = defaultThumbSize
	case !slices.Contains(thumbSizes, *size):
		writeError(w, http.StatusBadRequest, fmt.Sprintf("size must be one of %v", thumbSizes))
		return false
	}
	return true
}

// coverColorWriteTimeout bounds recording thumbnails' colours, which runs before
// they are sent: the single writer may be held (a scan, a prune), and a colour
// is not worth holding the images back for longer than this.
const coverColorWriteTimeout = 250 * time.Millisecond

// recordCoverColors stores the colours coverThumbnail read (nil entries are
// skipped). A failure only costs the colours until the next thumbnail (which
// reads them again, revalidation included), so it is logged, not answered. The
// write is detached from the request, so a client that has moved on still
// leaves its colours behind, and bounded by coverColorWriteTimeout.
func (a *API) recordCoverColors(ctx context.Context, recs ...*catalog.CoverColorRecord) {
	var batch []catalog.CoverColorRecord
	for _, r := range recs {
		if r != nil {
			batch = append(batch, *r)
		}
	}
	if len(batch) == 0 {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), coverColorWriteTimeout)
	defer cancel()
	switch err := a.cat.RecordCoverColors(wctx, batch); {
	case err != nil && wctx.Err() != nil:
		a.log.Debug("record cover colours timed out (the writer is busy)", "err", err)
	case err != nil:
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
	art, err := library.LookArt(ctx, lib.Root, src, a.fileWait)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		// Unreadable rather than missing (a share that stopped answering
		// included: metadata.GaveUp): try again next time.
		return &artSource{version: "x", load: func() ([]byte, error) { return nil, err }}
	}
	version := art.Version()
	if version == "" { // not a regular file
		return nil
	}
	load := func() ([]byte, error) {
		data, _, err := media.EmbeddedCover(ctx, art.Abs, a.fileWait)
		return data, err
	}
	if art.Kind == catalog.ArtSidecar {
		load = func() ([]byte, error) { return a.readSidecar(ctx, art.Abs) }
	}
	return &artSource{version: version, load: load}
}

// serveArt answers with the art of src (a book's own, or a show's sidecar) in
// the library at root, from the file the one rule names that the thumbnails and
// the scan's cover_version read too (library.ArtFile): 404 when there is none.
// Every look at it is bounded (fileWait): art on a share that stopped answering
// is a quick 503 share_not_answering, never a handler held for good.
func (a *API) serveArt(w http.ResponseWriter, r *http.Request, root string, src catalog.CoverSource) {
	art, err := library.LookArt(r.Context(), root, src, a.fileWait)
	switch {
	case metadata.GaveUp(err):
		writeShareNotAnswering(w)
		return
	case err != nil, art.Version() == "": // none, gone, unreadable, not a regular file
		writeError(w, http.StatusNotFound, "no cover")
		return
	}
	// The same lifetime for a sidecar image as embedded art: without one a
	// browser keeps it fresh by heuristic (a tenth of the file's age), so a
	// custom cover uploaded later would go unseen for weeks, not a day. Set only
	// for art that is there, so a 404 is never cached.
	if art.Kind == catalog.ArtSidecar {
		// Served from the open file: Range, conditional requests (a 304 reads
		// none of it) and the type from the file's name.
		w.Header().Set("Cache-Control", coverCache)
		if err := media.ServeFile(w, r, art.Abs, false, a.fileWait); err != nil {
			w.Header().Del("Cache-Control")
			writeShareNotAnswering(w)
		}
		return
	}
	data, mime, err := media.EmbeddedCover(r.Context(), art.Abs, a.fileWait)
	switch {
	case metadata.GaveUp(err):
		writeShareNotAnswering(w)
		return
	case data == nil:
		// No art, or a file that can't be read.
		writeError(w, http.StatusNotFound, "no cover")
		return
	}
	w.Header().Set("Cache-Control", coverCache)
	w.Header().Set("Content-Type", mime)
	_, _ = w.Write(data)
}

// readSidecar reads the sidecar image at abs whole (up to maxSidecarBytes,
// media.ErrImageTooLarge past it), its open and reads one metadata.Bounded call:
// a share that stopped answering costs the request fileWait at most
// (metadata.GaveUp).
func (a *API) readSidecar(ctx context.Context, abs string) ([]byte, error) {
	return metadata.Bounded(ctx, abs, a.fileWait, func() ([]byte, error) {
		f, _, err := metadata.OpenRegular(abs)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return media.ReadLimited(f, maxSidecarBytes)
	}, nil)
}

// fileWaitTimeout is API.fileWait: long enough for a cold read from a slow
// disk or share, well under the request timeout.
const fileWaitTimeout = 10 * time.Second

// streamOpenTimeout is API.streamWait, a stream's open. Longer than
// fileWaitTimeout: a sleeping NAS or USB disk takes longer than that to spin
// up, and a player waits for the audio it asked for where a cover can come
// later. A 503 is no retry for AVPlayer, so play would fail where it used to
// start late. Shorter than AVPlayer's resource timeout (about 60 s), so a
// share that stopped answering is still a 503 the player hears, not a request
// it gave up on itself.
const streamOpenTimeout = 45 * time.Second
