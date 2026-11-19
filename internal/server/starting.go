package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/kodestar/audiosilo-server/internal/web/spa"
)

// StartingStatus is the "status" a server answers every request with while it
// starts (StartGate): `audiosilo healthcheck` reads a 503 carrying it as
// healthy, so a long migration is never restarted mid-way (server#217).
const StartingStatus = "starting"

// StartingAnswer is StartGate's answer: the API's error envelope ("error", and
// a "code" to branch on) with the healthcheck's "status" and how long the start
// has run, by which the healthcheck tells a long start from a stuck one.
type StartingAnswer struct {
	Status             string `json:"status"`
	Error              string `json:"error"`
	Code               string `json:"code"`
	StartingForSeconds int64  `json:"starting_for_seconds"`
}

// startingRetry is the Retry-After StartGate sends, in seconds.
const startingRetry = "5"

// StartGate is the server's handler from the moment it listens: until Open
// hands it the app's handler, every request is answered 503 with a
// StartingAnswer (a browser opening a page gets startingPage instead), so the
// listener is up (and the healthcheck says "starting") while the database opens
// and migrates, which can take minutes on a large library; after Open every
// request goes to that handler.
type StartGate struct {
	h        atomic.Pointer[http.Handler]
	started  time.Time
	starting http.Handler
}

// NewStartGate is the gate of a start that begins now. wrap, when given, goes
// around the starting answer only, never the handler Open is given: the
// launcher passes the API's CORS policy (api.CORS), so a player on an allowed
// origin can read the answer and its preflight passes.
func NewStartGate(wrap func(http.Handler) http.Handler) *StartGate {
	g := &StartGate{started: time.Now()}
	g.starting = http.HandlerFunc(g.serveStarting)
	if wrap != nil {
		g.starting = wrap(g.starting)
	}
	return g
}

// Open sends every request from now on to h.
func (g *StartGate) Open(h http.Handler) { g.h.Store(&h) }

func (g *StartGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := g.h.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return
	}
	g.starting.ServeHTTP(w, r)
}

// serveStarting answers a request while the start runs: a StartingAnswer, or
// for a browser opening a page (Accept prefers text/html: /admin, /web, /) the
// same 503 and Retry-After with startingPage, which reloads itself, rather than
// raw JSON. API clients (a JSON Accept, */* or none) always get the JSON.
func (g *StartGate) serveStarting(w http.ResponseWriter, r *http.Request) {
	hdr := w.Header()
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Retry-After", startingRetry)
	hdr.Add("Vary", "Accept")
	if prefersHTML(r.Header.Values("Accept")) {
		hdr.Set("Content-Type", "text/html; charset=utf-8")
		hdr.Set("Content-Security-Policy", startingPageCSP)
		w.WriteHeader(http.StatusServiceUnavailable)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, startingPage)
		}
		return
	}
	hdr.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	if r.Method != http.MethodHead {
		_ = json.NewEncoder(w).Encode(StartingAnswer{
			Status: StartingStatus,
			Error:  "the server is starting",
			Code:   "server_starting",
			// time.Since reads the monotonic clock: a wall clock set mid-start
			// (a board with no battery, at its first NTP answer) changes nothing.
			StartingForSeconds: int64(time.Since(g.started) / time.Second),
		})
	}
}

// startingStyle is startingPage's one style sheet, allowed by its hash in
// startingPageCSP (no inline script, nothing loaded from anywhere).
const startingStyle = `body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;` +
	`font-family:system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;background:#f6f5f2;color:#1f1d1a}` +
	`main{max-width:28rem;padding:2rem;text-align:center}h1{font-size:1.5rem;margin:0 0 1rem}` +
	`p{margin:.5rem 0;line-height:1.5;color:#55514b}` +
	`@media (prefers-color-scheme:dark){body{background:#171614;color:#f2efe9}p{color:#b8b2a8}}`

// startingPage is what a browser opening a page sees while the server starts:
// self-contained, reloaded by its meta refresh after startingRetry seconds.
const startingPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="` + startingRetry + `">
<title>AudioSilo is starting</title>
<style>` + startingStyle + `</style>
</head>
<body>
<main>
<h1>AudioSilo is starting</h1>
<p>After an upgrade the server updates its database before it answers, which can take a while on a large library.</p>
<p>This page reloads by itself every few seconds.</p>
</main>
</body>
</html>
`

// startingPageCSP lets startingPage apply its own style sheet and nothing
// else: no script, no fetch, no frame, no form.
var startingPageCSP = func() string {
	sum := sha256.Sum256([]byte(startingStyle))
	return "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) +
		"'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}()

// prefersHTML reports whether an Accept header (its values, as Header.Values
// gives them) asks for an HTML page before JSON: text/html or
// application/xhtml+xml listed with a quality above zero and above
// application/json's. A browser's navigation does; fetch's default */*, a JSON
// Accept and no Accept at all do not.
func prefersHTML(accept []string) bool {
	html, jsonQ := 0.0, 0.0
	for _, value := range accept {
		for mediaType, q := range spa.Qualities(value) {
			switch mediaType {
			case "text/html", "application/xhtml+xml":
				html = max(html, q)
			case "application/json":
				jsonQ = max(jsonQ, q)
			}
		}
	}
	return html > 0 && html > jsonQ
}
