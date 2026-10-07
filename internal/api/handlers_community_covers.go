package api

import (
	"bytes"
	"context"
	"errors"
	"image"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

// Community covers in the match dialog: a candidate's cover_url is an image on its
// own host, which the console can't load (its CSP takes images only from the
// server), so the server fetches it (meta.Service.FetchCover, bounded there): as
// thumbnails to show (POST /admin/meta/covers), and whole to keep as the book's
// custom cover when the admin takes it (PUT /admin/libraries/{id}/cover/community).
// The fetches never hold coverReads, which is for the library's own art: only the
// decode is shared (decodeThumbnail). A thumbnail's fetch holds communityReads
// instead, from the fetch to the end of its decode, so however many dialogs ask,
// only that many community images are ever held in memory waiting for a decode.

const (
	// maxCommunityCoverReads bounds the community covers being fetched or
	// waiting to be decoded for thumbnails, across requests.
	maxCommunityCoverReads = 4
	// maxCommunityCovers caps one POST /admin/meta/covers: the dialog's
	// candidates and the recordings of the one being compared.
	maxCommunityCovers = 12
	// maxCommunityCoverBytes bounds a community cover read whole, to thumbnail
	// or to keep.
	maxCommunityCoverBytes = 16 << 20
	// communityCoversBudget bounds one batch: what hasn't arrived by then is
	// answered as "" rather than holding the rest back.
	communityCoversBudget = 20 * time.Second
	// storedCoverSide is the longest side a community cover larger than an
	// upload may be (catalog.MaxCoverBytes) is re-encoded within to be kept.
	storedCoverSide = 1600
)

// handleCommunityCovers serves POST /admin/meta/covers {"urls":[...],"size"}:
// JPEG thumbnails of community cover images as data: URLs, in request order; ""
// for one that couldn't be fetched or decoded. Metadata off -> 404 metadata_off,
// as every outbound call stops with it.
func (a *API) handleCommunityCovers(w http.ResponseWriter, r *http.Request) {
	if a.metadataOff(w) {
		return
	}
	var req struct {
		URLs []string `json:"urls"`
		Size int      `json:"size"`
	}
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if !thumbBatch(w, "urls", len(req.URLs), maxCommunityCovers, &req.Size) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), communityCoversBudget)
	defer cancel()
	out := make([]string, len(req.URLs))
	var wg sync.WaitGroup
	for i, u := range req.URLs {
		wg.Go(func() {
			if jpg := a.communityThumbnail(ctx, u, req.Size); jpg != nil {
				out[i] = jpegDataURL(jpg)
			}
		})
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"covers": out})
}

// communityThumbnail is a community cover's thumbnail at size through the shared
// cache (keyed by the URL), nil when there is none. A URL that isn't one, an
// image too large to read and one that can't be decoded are cached as none; a
// fetch that failed is not, so the next dialog tries again.
func (a *API) communityThumbnail(ctx context.Context, rawURL string, size int) []byte {
	key := "r\x00" + rawURL + "\x00" + strconv.Itoa(size)
	if jpg, ok := a.thumbs.Get(key); ok {
		return jpg
	}
	select {
	case a.communityReads <- struct{}{}:
	case <-ctx.Done():
		return nil
	}
	defer func() { <-a.communityReads }()
	raw, err := a.fetchCover(ctx, rawURL, maxCommunityCoverBytes)
	var jpg []byte
	switch {
	case errors.Is(err, meta.ErrCoverURL) || errors.Is(err, media.ErrImageTooLarge):
	case err != nil:
		a.log.Debug("fetch community cover failed", "err", err, "url", rawURL)
		return nil
	default:
		if jpg, err = a.decodeThumbnail(ctx, raw, size, "url", rawURL); err != nil {
			return nil
		}
	}
	a.thumbs.Put(key, jpg)
	return jpg
}

// handleAdminSetCommunityCover serves PUT /admin/libraries/{id}/cover/community?path=
// {"url"}: a community cover (a match candidate's cover_url), fetched by the server
// and kept as the book's custom cover exactly as an upload is (saveCustomCover),
// once keepableCover has checked it. Metadata off -> 404 metadata_off; no book ->
// 404 book_not_found; a URL that isn't absolute http(s) -> 400; the image can't be
// fetched -> 502 cover_unavailable; too large -> 413 too_large; not an image -> 415
// unsupported_image.
func (a *API) handleAdminSetCommunityCover(w http.ResponseWriter, r *http.Request) {
	if a.metadataOff(w) {
		return
	}
	lib, p, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	ctx := r.Context()
	// The book first: no fetch for a path that holds none.
	if _, err := a.cat.GetBookByPath(ctx, lib.ID, p); err != nil {
		a.writeBookError(w, err, "load book for cover failed", "could not save the cover", "library", lib.ID, "path", p)
		return
	}
	data, err := a.fetchCover(ctx, req.URL, maxCommunityCoverBytes)
	switch {
	case errors.Is(err, meta.ErrCoverURL):
		writeError(w, http.StatusBadRequest, "url must be an absolute http(s) URL")
		return
	case errors.Is(err, media.ErrImageTooLarge):
		err = catalog.ErrCoverTooLarge
	case err != nil:
		if ctx.Err() == nil {
			a.log.Warn("fetch community cover failed", "err", err, "url", req.URL)
		}
		writeErrorCode(w, http.StatusBadGateway, codeCoverUnavailable, "could not fetch the cover")
		return
	default:
		data, err = a.keepableCover(ctx, data, req.URL)
	}
	a.saveCustomCover(w, r, lib, p, data, err, map[string]any{"source": catalog.SourceCommunity})
}

// keepableCover is a fetched community cover as it may be kept: refused
// (catalog.ErrUnsupportedImage) when its header isn't an image's, too large
// (catalog.ErrCoverTooLarge) past media.MaxThumbnailSourcePixels, which a player
// showing it whole would have to decode (a small file can claim huge dimensions),
// re-encoded within storedCoverSide when it is larger than an upload may be
// (catalog.MaxCoverBytes), else as fetched.
func (a *API) keepableCover(ctx context.Context, data []byte, rawURL string) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	switch {
	case err != nil || cfg.Width <= 0 || cfg.Height <= 0:
		return nil, catalog.ErrUnsupportedImage
	case int64(cfg.Width)*int64(cfg.Height) > media.MaxThumbnailSourcePixels:
		return nil, catalog.ErrCoverTooLarge
	case len(data) <= catalog.MaxCoverBytes:
		return data, nil
	}
	jpg, err := a.decodeThumbnail(ctx, data, storedCoverSide, "url", rawURL)
	if err == nil && jpg == nil {
		err = catalog.ErrUnsupportedImage
	}
	return jpg, err
}
