package api

import (
	"compress/gzip"
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/web/spa"
)

const (
	gzipMinSize = 1024 // bytes of JSON below which an answer goes out as it is
	gzipLevel   = gzip.BestSpeed
)

// gzipAccepted reports whether the compress middleware found r compressible
// and its client accepting gzip: the list cache's cue to send its stored gzip.
func gzipAccepted(r *http.Request) bool {
	ok, _ := r.Context().Value(gzipKey).(bool)
	return ok
}

// compressJSON opts a reviewed JSON read into gzip at route registration.
// Other handlers retain their original writer, including its streaming interfaces.
// Range requests and protocol upgrades always get the original writer.
func compressJSON(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if (r.Method != http.MethodGet && r.Method != http.MethodHead) || r.Header.Get("Range") != "" || r.Header.Get("Upgrade") != "" {
			next(w, r)
			return
		}
		gz := spa.AcceptsGzip(r.Header.Get("Accept-Encoding"))
		if gz {
			r = r.WithContext(context.WithValue(r.Context(), gzipKey, true))
		}
		cw := &gzipWriter{ResponseWriter: w, accept: gz}
		next(cw, r)
		cw.finish()
	}
}

// gzipWriters keeps the streaming gzipWriter's compressors between requests,
// the same pool spa.Gzip draws on for a whole answer in memory.
var gzipWriters = spa.GzipWriters(gzipLevel)

// gzipWriter holds back a JSON answer until it is known to be at least
// gzipMinSize, then gzips it; anything else passes straight through. It
// deliberately has no Flush or Unwrap: a route that streams is registered without
// compressJSON, and a flush reaching past the held bytes would reorder them.
type gzipWriter struct {
	http.ResponseWriter
	accept bool // the client takes gzip

	status int // the status the handler wrote (0 none yet)
	mode   int // gzUndecided, gzIdentity or gzGzip
	held   []byte
	zw     *gzip.Writer
}

const (
	gzUndecided = iota
	gzIdentity
	gzGzip
)

func (w *gzipWriter) WriteHeader(code int) {
	if code < 200 { // informational: not the answer
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if w.status != 0 {
		return // superfluous, as net/http treats it
	}
	w.status = code
	if !w.isJSON() {
		w.mode = gzIdentity
		w.ResponseWriter.WriteHeader(code)
		return
	}
	addVary(w.Header(), "Accept-Encoding")
	h := w.Header()
	if !w.accept || code == http.StatusNoContent || code == http.StatusNotModified || code == http.StatusPartialContent ||
		h.Get("Content-Range") != "" || h.Get("Content-Encoding") != "" || h.Get("ETag") != "" {
		w.mode = gzIdentity
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *gzipWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	switch w.mode {
	case gzIdentity:
		return w.ResponseWriter.Write(p)
	case gzGzip:
		return w.zw.Write(p)
	}
	if len(w.held) == 0 && len(p) >= gzipMinSize { // writeJSON's one write: no copy
		w.startGzip()
		return w.zw.Write(p)
	}
	w.held = append(w.held, p...)
	if len(w.held) >= gzipMinSize {
		w.startGzip()
		if _, err := w.zw.Write(w.held); err != nil {
			return 0, err
		}
		w.held = nil
	}
	return len(p), nil
}

func (w *gzipWriter) startGzip() {
	w.mode = gzGzip
	h := w.Header()
	h.Set("Content-Encoding", "gzip")
	h.Del("Content-Length")
	w.ResponseWriter.WriteHeader(w.status)
	w.zw = gzipWriters.Get().(*gzip.Writer)
	w.zw.Reset(w.ResponseWriter)
}

// finish ends the answer once the handler has returned: a held answer below
// gzipMinSize goes out as it is, a gzipped one gets its trailer.
func (w *gzipWriter) finish() {
	switch w.mode {
	case gzUndecided:
		if w.status == 0 {
			return // nothing written: net/http's empty 200
		}
		if w.Header().Get("Content-Length") == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(w.held)))
		}
		w.ResponseWriter.WriteHeader(w.status)
		_, _ = w.ResponseWriter.Write(w.held)
	case gzGzip:
		_ = w.zw.Close()
		w.zw.Reset(nil)
		gzipWriters.Put(w.zw)
	}
}

// isJSON reports whether the answer's Content-Type is application/json.
func (w *gzipWriter) isJSON() bool {
	mt, _, _ := strings.Cut(w.Header().Get("Content-Type"), ";")
	return strings.EqualFold(strings.TrimSpace(mt), "application/json")
}

// addVary adds token to h's Vary unless one of its values already names it.
func addVary(h http.Header, token string) {
	if !hasVary(h, token) {
		h.Add("Vary", token)
	}
}

// hasVary reports whether one of h's Vary values names token.
func hasVary(h http.Header, token string) bool {
	for _, v := range h.Values("Vary") {
		for t := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}
