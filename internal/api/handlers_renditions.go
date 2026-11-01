package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/media"
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
	setRetryAfter(w, retry)
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
	abs, ok := audioFile(w, lib, rel)
	if !ok {
		return
	}
	wait, cancel := context.WithTimeout(r.Context(), a.renditionWait)
	defer cancel()
	f, etag, err := a.renditions.Open(wait, a.baseCtx, a.ffmpeg, abs)
	if err != nil {
		switch {
		case r.Context().Err() != nil:
			// The client went away: nothing to answer.
		case errors.Is(err, media.ErrNoSource):
			writeError(w, http.StatusNotFound, "not found")
		case errors.Is(err, media.ErrRefused):
			writeErrorCode(w, http.StatusUnprocessableEntity, codeRenditionFailed, "this file can't be encoded")
		case a.baseCtx.Err() != nil:
			writeError(w, http.StatusServiceUnavailable, "the server is shutting down")
		case errors.Is(err, media.ErrPending):
			writeRetry(w, renditionPendingRetry, codeRenditionPending, "the rendition is still being made")
		default:
			if !errors.Is(err, media.ErrUnavailable) {
				a.log.Warn("watch rendition failed", "path", abs, "err", err)
			}
			writeRetry(w, renditionErrorRetry, codeRenditionError, "the rendition could not be made now")
		}
		return
	}
	serveCopy(w, r, f, etag, copyName(abs, a.renditions.Ext()), r.URL.Query().Get("download") == "1")
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
	a.renditionsOfBook(w, r, true)
}

// handleRenditionStatus is GET /libraries/{id}/renditions?path=<book>&kind=watch:
// where each audio file's rendition stands, queueing nothing.
func (a *API) handleRenditionStatus(w http.ResponseWriter, r *http.Request) {
	a.renditionsOfBook(w, r, false)
}

// renditionsOfBook is the two renditions routes: the book at ?path= scope-checked
// as /item (403 outside the grant, 404 no book), the kind checked (the body's
// for a prepare, else ?kind=), then each of its audio files, in play order,
// queued (prepare: 202) or only read (200).
func (a *API) renditionsOfBook(w http.ResponseWriter, r *http.Request, prepare bool) {
	lib, path, scope, code, msg := a.authorizedScope(r)
	if code != 0 {
		writeError(w, code, msg)
		return
	}
	kind, status := r.URL.Query().Get("kind"), http.StatusOK
	if prepare {
		var in struct {
			Kind string `json:"kind"`
		}
		if err := decodeJSONOptional(r, &in, 0); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request")
			return
		}
		kind, status = in.Kind, http.StatusAccepted
	}
	if !a.renditionKindAllowed(w, lib, kind) {
		return
	}
	book, ok := a.bookAt(w, r, lib, scope, path, "no book at that path", "could not load book")
	if !ok {
		return
	}
	audio := book.AudioFiles()
	files := make([]renditionFile, 0, len(audio))
	for _, f := range audio {
		out := renditionFile{Path: f.RelPath, State: media.CacheFailed}
		if abs, err := library.SafeJoin(lib.Root, f.RelPath); err == nil {
			if prepare {
				out.State, out.Bytes = a.renditions.Queue(a.baseCtx, a.ffmpeg, abs)
			} else {
				out.State, out.Bytes = a.renditions.Status(abs)
			}
		}
		files = append(files, out)
	}
	writeJSON(w, status, map[string]any{"kind": kind, "files": files})
}
