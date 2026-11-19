package server

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"
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
// StartingAnswer, so the listener is up (and the healthcheck says "starting")
// while the database opens and migrates, which can take minutes on a large
// library; after Open every request goes to that handler.
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

func (g *StartGate) serveStarting(w http.ResponseWriter, r *http.Request) {
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Retry-After", startingRetry)
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
