package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// Watch renditions (Track W2, capability renditions): a small AAC copy of each
// audio file of a book, encoded once into <data>/cache/renditions/watch
// (media.NewRenditionCache) with the source's duration, so a watch downloads a
// book without a library write and positions and chapters hold. The file comes
// from GET /stream?rendition=watch; POST /renditions prepares a whole book, GET
// /renditions says where each file stands. A download is not a listen: none of
// this marks a listening session or a transcode.

// The renditions' error codes.
const (
	codeRenditionPending     = "rendition_pending"     // 503: still encoding (Retry-After)
	codeRenditionUnavailable = "rendition_unavailable" // 404: no ffmpeg or no data dir
	codeBadRendition         = "bad_rendition"         // 400: unknown kind, or a podcast library
	codeRenditionFailed      = "rendition_failed"      // 422: ffmpeg refused the file (remembered)
	codeRenditionError       = "rendition_error"       // 503: the encode failed for a reason not the file's (Retry-After)
)

// Retry-After values: a pending encode is checked again soon (a long file
// takes minutes), a failed one later.
const (
	renditionPendingRetry = 15 * time.Second
	renditionErrorRetry   = 60 * time.Second
)

// renditionKindAllowed answers a rendition request's kind and library: false,
// having written 400 bad_rendition, for a kind this server doesn't make or a
// podcast library (watch v1 has no podcasts), or 404 rendition_unavailable
// while renditions are off.
func (a *API) renditionKindAllowed(w http.ResponseWriter, lib *catalog.Library, kind string) bool {
	switch {
	case kind != media.RenditionWatch:
		writeErrorCode(w, http.StatusBadRequest, codeBadRendition, "unknown rendition kind")
	case lib.Kind == catalog.KindPodcasts:
		writeErrorCode(w, http.StatusBadRequest, codeBadRendition, "podcast episodes have no renditions")
	case !a.renditionsOn():
		writeErrorCode(w, http.StatusNotFound, codeRenditionUnavailable, "renditions are not available on this server")
	default:
		return true
	}
	return false
}

// writeRetry writes a 503 with code and msg, asking the client to come back
// after retry.
func writeRetry(w http.ResponseWriter, retry time.Duration, code, msg string) {
	w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())))
	writeErrorCode(w, http.StatusServiceUnavailable, code, msg)
}

// serveRendition answers GET /stream?rendition=<kind> for the audio file rel
// (already scope-checked): its rendition with Range and its own validators (a
// strong ETag naming the encode, the copy's mtime), ?download=1 an attachment
// named <stem>.m4a. A cold encode is waited for up to renditionWait, then 503
// rendition_pending while it goes on under the server's context; never a live
// transcode (not rangeable, so a watch couldn't resume it).
func (a *API) serveRendition(w http.ResponseWriter, r *http.Request, lib *catalog.Library, rel, kind string) {
	if !a.renditionKindAllowed(w, lib, kind) {
		return
	}
	if !metadata.IsAudio(rel) {
		writeError(w, http.StatusNotFound, "not an audio file")
		return
	}
	abs, err := library.SafeJoin(lib.Root, rel)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	if info, err := os.Stat(abs); err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	wait, cancel := context.WithTimeout(r.Context(), a.renditionWait)
	defer cancel()
	f, etag, err := a.renditions.Open(wait, a.baseCtx, a.ffmpeg, abs)
	if err != nil {
		switch {
		case r.Context().Err() != nil:
			// The client went away: nothing to answer.
		case errors.Is(err, media.ErrRefused):
			writeErrorCode(w, http.StatusUnprocessableEntity, codeRenditionFailed, "this file can't be encoded")
		case a.baseCtx.Err() != nil:
			writeError(w, http.StatusServiceUnavailable, "the server is shutting down")
		case wait.Err() != nil && errors.Is(err, context.DeadlineExceeded):
			writeRetry(w, renditionPendingRetry, codeRenditionPending, "the rendition is still being made")
		default:
			if !errors.Is(err, media.ErrUnavailable) {
				a.log.Warn("watch rendition failed", "path", abs, "err", err)
			}
			writeRetry(w, renditionErrorRetry, codeRenditionError, "the rendition could not be made now")
		}
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read the rendition")
		return
	}
	w.Header().Set("ETag", etag)
	name := strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs)) + ".m4a"
	media.ServeOpenedAs(w, r, f, name, info.ModTime(), r.URL.Query().Get("download") == "1")
}

// renditionFile is one audio file of a book in a renditions answer: its path
// (as /stream takes it), where its rendition stands, and its size once ready.
type renditionFile struct {
	Path  string           `json:"path"`
	State media.CacheState `json:"state"`
	Bytes int64            `json:"bytes,omitempty"`
}

// handlePrepareRenditions is POST /libraries/{id}/renditions?path=<book> with
// {"kind":"watch"}: every audio file of the book whose rendition is missing is
// queued (encoded one at a time in the background), and the answer, 202, says
// where each stands.
func (a *API) handlePrepareRenditions(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind string `json:"kind"`
	}
	a.renditionsOfBook(w, r, http.StatusAccepted, func() (string, bool) {
		if err := decodeJSONOptional(r, &in, 0); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request")
			return "", false
		}
		return in.Kind, true
	}, func(abs string) (media.CacheState, int64) { return a.renditions.Queue(a.baseCtx, a.ffmpeg, abs) })
}

// handleRenditionStatus is GET /libraries/{id}/renditions?path=<book>&kind=watch:
// where each audio file's rendition stands, queueing nothing.
func (a *API) handleRenditionStatus(w http.ResponseWriter, r *http.Request) {
	a.renditionsOfBook(w, r, http.StatusOK, func() (string, bool) { return r.URL.Query().Get("kind"), true },
		func(abs string) (media.CacheState, int64) { return a.renditions.Status(abs) })
}

// renditionsOfBook is the two renditions routes: the book at ?path= scope-checked
// as /item (403 outside the grant, 404 no book), the kind (readKind) checked,
// then each of its files, in play order, through state; answered with status.
func (a *API) renditionsOfBook(w http.ResponseWriter, r *http.Request, status int, readKind func() (string, bool), state func(abs string) (media.CacheState, int64)) {
	lib, path, scope, code, msg := a.authorizedScope(r)
	if code != 0 {
		writeError(w, code, msg)
		return
	}
	kind, ok := readKind()
	if !ok || !a.renditionKindAllowed(w, lib, kind) {
		return
	}
	book, ok := a.bookAt(w, r, lib, scope, path, "no book at that path", "could not load book")
	if !ok {
		return
	}
	files := make([]renditionFile, 0, len(book.Files))
	for _, f := range book.Files {
		out := renditionFile{Path: f.RelPath, State: media.CacheFailed}
		if abs, err := library.SafeJoin(lib.Root, f.RelPath); err == nil {
			out.State, out.Bytes = state(abs)
		}
		files = append(files, out)
	}
	writeJSON(w, status, map[string]any{"kind": kind, "files": files})
}
