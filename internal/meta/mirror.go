package meta

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Mirror is a local copy of the metadata service (internal/metamirror in
// mirror mode): metaserve's own JSON API handler over a downloaded copy of its
// database, so the copy answers exactly what the remote service would.
type Mirror interface {
	// ServeHTTP answers a metaserve API request (/api/v1/..., /healthz) from the
	// copy.
	http.Handler
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

// SetMirror puts the local copy m in front of the remote service (mirror mode):
// every request the client makes is answered by m while it is Ready, and goes to
// the remote service, unchanged, while it is not or when m answers 5xx (a query
// this code can't run, e.g. on an artifact schema newer than it knows). The
// requests, their URLs and the answers' decoding are exactly remote mode's, so
// nothing above the client knows which one answered.
//
// Mirror mode also changes two things above the client: a "no match" never
// replaces a stored positive answer (see Enrich and Work), and Ping reports the
// local copy rather than asking the remote service. Call it once, right after
// NewService and before the Service is used.
func (s *Service) SetMirror(m Mirror) {
	s.mirror = m
	// No whole-request deadline on the client: each path sets its own (the
	// transport's), so the local copy gets localTimeout and the remote service
	// keeps clientTimeout.
	s.client.http = &http.Client{Transport: &fallbackTransport{local: m, remote: http.DefaultTransport}}
}

// fallbackTransport answers from the local copy when it can and from the remote
// service otherwise (decision 3 of the mirror design: no copy or a 5xx -> the
// remote service; a 404 is the copy's answer).
type fallbackTransport struct {
	local  Mirror
	remote http.RoundTripper
}

func (t *fallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.local.Ready() {
		if resp := serveLocal(t.local, req); resp != nil {
			return resp, nil
		}
	}
	ctx, cancel := context.WithTimeout(req.Context(), clientTimeout)
	resp, err := t.remote.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	// The deadline covers the body too (as http.Client.Timeout does): released
	// when the caller closes it.
	resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// serveLocal answers req from the local copy, or returns nil when the copy
// failed it (a 5xx, an answer too large to buffer, a panic) so the caller asks
// the remote service instead.
func serveLocal(h http.Handler, req *http.Request) (resp *http.Response) {
	ctx, cancel := context.WithTimeout(req.Context(), localTimeout)
	defer cancel()
	lr := req.Clone(ctx)
	lr.RequestURI = req.URL.RequestURI()
	w := &memWriter{header: http.Header{}}
	defer func() {
		// A panic in a query is the copy failing this request, not the server.
		if recover() != nil {
			resp = nil
		}
	}()
	h.ServeHTTP(w, lr)
	code := w.status()
	if code >= 500 || w.overflow || ctx.Err() != nil {
		return nil
	}
	body := w.body.Bytes()
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", code, http.StatusText(code)),
		StatusCode:    code,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        w.header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
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

// cancelBody releases a request's deadline when its body is closed.
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// mirrorHealth is Ping in mirror mode: the local copy's state, with no outbound
// call (the remote service is only the fallback there).
func (s *Service) mirrorHealth() Health {
	h := Health{CheckedAt: s.now(), Reachable: s.mirror.Ready()}
	if !h.Reachable {
		h.Error = "no local copy yet"
	}
	return h
}
