package meta

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"
)

// Mirror is a local copy of the metadata service (internal/metamirror in
// mirror mode): metaserve's own JSON API handler over a downloaded copy of its
// database, so the copy answers exactly what the remote service would.
type Mirror interface {
	// Handler answers metaserve's API (/api/v1/..., /healthz) from the copy.
	Handler() http.Handler
	// Ready reports whether a usable copy is loaded. Until it is, every request
	// goes to the remote service.
	Ready() bool
}

// localTimeout bounds one request answered by the local copy. It is longer than
// clientTimeout because the copy runs on the server's own hardware (a small home
// box may need longer for a match than meta.audiosilo.app does), and a local
// query that runs out of time still falls back to the remote service.
const localTimeout = 15 * time.Second

// maxLocalBody bounds what one local answer may buffer in memory (the in-memory
// response writer below). No metaserve answer comes near it; one that did would
// be a fault, answered by the remote service instead.
const maxLocalBody = 32 << 20

// fallbackLogEvery spaces the warnings about requests the copy failed: one
// tells the admin, a busy hour of them is noise.
const fallbackLogEvery = 10 * time.Minute

// SetMirror puts the local copy m in front of the remote service (mirror mode):
// every request the client makes is answered by m while it is Ready, and goes to
// the remote service, unchanged, while it is not or when m fails it (a 5xx, e.g.
// a query this code can't run on an artifact schema newer than it knows; an
// answer too large; a timeout; a panic). The requests, their URLs and the
// answers' decoding are exactly remote mode's, so nothing above the client knows
// which one answered - Ping included: /healthz is answered by the copy when it
// is ready, else by the remote service. log receives a warning (at most every
// ten minutes) when a request the copy failed goes to the remote service.
//
// Mirror mode also changes one thing above the client: a "no match" never
// replaces a stored positive answer (see Enrich and Work). Call it once, right
// after NewService and before the Service is used.
func (s *Service) SetMirror(m Mirror, log *slog.Logger) {
	s.mirror = m
	// The remote leg is remote mode's own client, whose Timeout (clientTimeout)
	// covers the body too; only the local leg has its own bound (localTimeout),
	// so the outer client has none.
	s.client.http = &http.Client{Transport: &fallbackTransport{
		local: m.Handler(), ready: m.Ready, remote: s.client.http, log: log,
	}}
}

// fallbackTransport answers from the local copy when it can and from the remote
// service otherwise (no copy or a failed answer -> the remote service; a 404 is
// the copy's answer).
type fallbackTransport struct {
	local  http.Handler
	ready  func() bool
	remote *http.Client
	log    *slog.Logger
	// logged is when (unix nanoseconds) the last fallback was logged.
	logged atomic.Int64
}

func (t *fallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.ready() {
		resp, failed := serveLocal(t.local, req)
		if resp != nil {
			return resp, nil
		}
		t.logFallback(req.URL.Path, failed)
	}
	resp, err := t.remote.Do(req)
	var ue *url.Error
	if errors.As(err, &ue) {
		// The outer client wraps it again: unwrapped, the error reads as remote
		// mode's does.
		return nil, ue.Err
	}
	return resp, err
}

// logFallback warns that a request the copy failed went to the remote service,
// at most once every fallbackLogEvery.
func (t *fallbackTransport) logFallback(path, reason string) {
	if t.log == nil {
		return
	}
	now := time.Now().UnixNano()
	last := t.logged.Load()
	if now-last < int64(fallbackLogEvery) || !t.logged.CompareAndSwap(last, now) {
		return
	}
	t.log.Warn("metadata mirror: the local copy couldn't answer; asked the online service instead",
		"path", path, "reason", reason)
}

// serveLocal answers req from the local copy. When the copy failed it (a 5xx,
// an answer too large to buffer, a timeout, a panic) it returns a nil response
// and why, so the caller asks the remote service instead.
func serveLocal(h http.Handler, req *http.Request) (resp *http.Response, failed string) {
	ctx, cancel := context.WithTimeout(req.Context(), localTimeout)
	defer cancel()
	lr := req.WithContext(ctx)
	lr.RequestURI = req.URL.RequestURI()
	w := &memWriter{header: http.Header{}}
	defer func() {
		// A panic in a query is the copy failing this request, not the server.
		if recover() != nil {
			resp, failed = nil, "panic"
		}
	}()
	h.ServeHTTP(w, lr)
	code := w.status()
	switch {
	case w.overflow:
		return nil, "answer too large"
	case ctx.Err() != nil:
		return nil, "timed out"
	case code >= 500:
		return nil, "HTTP " + strconv.Itoa(code)
	}
	body := w.body.Bytes()
	return &http.Response{
		Status:        strconv.Itoa(code) + " " + http.StatusText(code),
		StatusCode:    code,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        w.header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, ""
}

// memWriter is an http.ResponseWriter that keeps the answer in memory, so a
// handler can answer a client's request in-process (no listener, no httptest).
type memWriter struct {
	header   http.Header
	code     int
	body     bytes.Buffer
	overflow bool
}

func (w *memWriter) Header() http.Header { return w.header }

func (w *memWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}

func (w *memWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	if w.body.Len()+len(p) > maxLocalBody {
		w.overflow = true
		return 0, errLocalBodyTooLarge
	}
	return w.body.Write(p)
}

// status is the answer's status code: 200 when the handler wrote nothing else.
func (w *memWriter) status() int {
	if w.code == 0 {
		return http.StatusOK
	}
	return w.code
}

var errLocalBodyTooLarge = errors.New("meta: local answer too large")
