package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/kodestar/audiosilo-server/internal/web/spa"
)

// JSON compression (PERF-SPIKE S5). A 200-book page is ~71 KB of JSON and
// ~10 KB gzipped: away from home that is the difference between several round
// trips and one. Every client sends Accept-Encoding: gzip and decodes it
// transparently (browsers, the apps' fetch, Go's http.Transport in the Bridge
// and the manager), so the wire format is unchanged.
//
// What is compressed is decided twice, and both must say yes:
//
//   - The request (compressible): a GET (or its HEAD, which must carry the
//     GET's Vary and Content-Encoding: server#270) under /api/v1/ that is none
//     of the excluded classes. Every credential the API hands out (sign-in,
//     pairing, exchange, redeem, API keys, cast tokens, member sessions, auth
//     codes, Bridge pairing, recovery codes, the setup wizard, the calendar feed
//     link) is minted by a POST, and /api/v1/auth/ is excluded whole besides, so
//     a compressed body never holds one (BREACH needs a secret and
//     attacker-chosen text in one compressed body). Excluded too: the media
//     (cover, stream, ebook, a backup's download, /meta/cover: isStreamingPath
//     and isLibraryMedia's neighbours, and below /me/upcoming/ the Upcoming
//     covers and the calendar feed, which take their credential in the URL as
//     the media do and answer no JSON worth compressing) and the Bridge's
//     routes, whose claim is a long poll and whose job parameters can carry a
//     private feed URL. A new GET that returns a credential must be added to
//     compressExcluded.
//   - The response (gzipWriter): Content-Type application/json, no
//     Content-Encoding or ETag of the handler's own (a handler that sets a
//     validator compresses its own representation, as /readalong does), a status
//     with a body, and at least gzipMinSize bytes. Smaller answers go out as they
//     are: their gzip saves nothing worth the CPU.
//
// Every JSON answer to a compressible request carries Vary: Accept-Encoding,
// compressed or not, so a cache between never hands one client's coding to
// another. The list cache keeps each answer's gzip beside it (listcache.go), so
// a hit costs no compression.
const (
	gzipMinSize = 1024 // bytes of JSON below which an answer goes out as it is
	gzipLevel   = gzip.BestSpeed
)

// compressExcluded is the GET path prefixes under /api/v1/ never compressed
// (see the rule above): the credential routes, the Bridge's, and the calendar
// feed's and Upcoming covers' (GET /me/upcoming itself is compressed).
var compressExcluded = []string{
	"/api/v1/auth/",
	"/api/v1/bridge/",
	"/api/v1/me/upcoming/",
}

// compressible reports whether r may have its JSON answer gzipped (the request
// half of the rule above), whatever the client accepts.
func compressible(r *http.Request) bool {
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || r.Header.Get("Range") != "" {
		return false
	}
	p := r.URL.Path
	if !strings.HasPrefix(p, "/api/v1/") || isStreamingPath(p) || isMetaCover(p) {
		return false
	}
	for _, x := range compressExcluded {
		if strings.HasPrefix(p, x) {
			return false
		}
	}
	return true
}

// isMetaCover reports whether p is GET /libraries/{id}/meta/cover (a community
// cover image).
func isMetaCover(p string) bool {
	leaf, ok := libraryLeaf(p)
	return ok && leaf == "meta/cover"
}

// gzipAccepted reports whether the compress middleware found r compressible
// and its client accepting gzip: the list cache's cue to send its stored gzip.
func gzipAccepted(r *http.Request) bool {
	ok, _ := r.Context().Value(gzipKey).(bool)
	return ok
}

// compress wraps the API's handlers (inside the timeout, so its writes land in
// the timeout's buffer) with gzipWriter for a compressible request. It must
// wrap the mux itself: the list cache reads its verdict from the context.
func compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !compressible(r) {
			next.ServeHTTP(w, r)
			return
		}
		gz := spa.AcceptsGzip(r.Header.Get("Accept-Encoding"))
		if gz {
			r = r.WithContext(context.WithValue(r.Context(), gzipKey, true))
		}
		cw := &gzipWriter{ResponseWriter: w, accept: gz}
		next.ServeHTTP(cw, r)
		cw.finish()
	})
}

// gzipWriters keeps the compressors between requests (each holds ~800 KB of
// state at BestSpeed; allocating one per answer would cost more than the
// compression).
var gzipWriters = sync.Pool{New: func() any {
	zw, _ := gzip.NewWriterLevel(nil, gzipLevel)
	return zw
}}

// gzipBytes is body gzipped at gzipLevel.
func gzipBytes(body []byte) []byte {
	var buf bytes.Buffer
	buf.Grow(len(body) / 4)
	zw := gzipWriters.Get().(*gzip.Writer)
	zw.Reset(&buf)
	_, _ = zw.Write(body)
	_ = zw.Close()
	zw.Reset(nil) // let go of buf
	gzipWriters.Put(zw)
	return buf.Bytes()
}

// gzipWriter holds back a JSON answer until it is known to be at least
// gzipMinSize, then gzips it; anything else passes straight through. It
// deliberately has no Flush or Unwrap: a route that streams is excluded by
// compressible, and a flush reaching past the held bytes would reorder them.
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
	if !w.accept || !bodyAllowed(code) || w.Header().Get("Content-Encoding") != "" || w.Header().Get("ETag") != "" {
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

// bodyAllowed reports whether a status carries a body (RFC 9110: not 204 or 304).
func bodyAllowed(code int) bool {
	return code != http.StatusNoContent && code != http.StatusNotModified
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
