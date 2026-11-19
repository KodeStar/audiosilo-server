package server

import (
	"net/http"
	"sync/atomic"
)

// StartingStatus is the "status" a server answers every request with while it
// starts (StartGate): `audiosilo healthcheck` reads a 503 carrying it as
// healthy, so a long migration is never restarted mid-way (server#217).
const StartingStatus = "starting"

// startingBody is StartGate's answer: the API's error envelope ("error", and a
// "code" to branch on) with the healthcheck's "status".
const startingBody = `{"status":"` + StartingStatus + `","error":"the server is starting","code":"server_starting"}` + "\n"

// startingRetry is the Retry-After StartGate sends, in seconds.
const startingRetry = "5"

// StartGate is the server's handler from the moment it listens: until Open
// hands it the app's handler, every request is answered 503 with
// startingBody, so the listener is up (and the healthcheck says "starting")
// while the database opens and migrates, which can take minutes on a large
// library; after Open every request goes to that handler.
type StartGate struct {
	h atomic.Pointer[http.Handler]
}

// Open sends every request from now on to h.
func (g *StartGate) Open(h http.Handler) { g.h.Store(&h) }

func (g *StartGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := g.h.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Retry-After", startingRetry)
	w.WriteHeader(http.StatusServiceUnavailable)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(startingBody))
	}
}
