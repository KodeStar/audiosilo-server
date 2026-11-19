package api

import (
	"context"
	"net/http"
	"strconv"
	"sync/atomic"
)

// headerTranscodeSent is a transcode HEAD's word on the same ask's GET that
// is running: the bytes it has written to its response so far. Absent when no
// GET of the ask runs (none was asked for, it ended, or it was dropped). A
// client whose stream stalls looks twice: a count that grows is a slow
// transcode still producing, to wait for, where asking again would take its
// slot over and end it. Only ever the caller's own ask's (its token's).
const headerTranscodeSent = "X-AudioSilo-Transcode-Sent"

// transcodeAsk is what a transcode streams and for whom: the credential's
// token, the file, the ?t= start and whether it is a download. Safari asks for
// one stream two or three times (its range probing), each a GET of its own,
// and plays only the last: an ask the same token made before for the same
// thing has its slot taken over (takeTranscodeSlot) rather than the new one
// needing another, so with one slot free the asks don't shut each other out.
// A token is one user's, so a takeover never ends another user's transcode.
type transcodeAsk struct {
	token    int64
	lib      int64
	rel      string
	start    float64
	download bool
}

// keyed reports whether a GET of the ask is in transcodeHolds, for a same ask
// to take over: one with a token (the slot is another's once the map holds
// another hold for its ask).
func (k transcodeAsk) keyed() bool { return k.token != 0 }

// transcodeHold is one transcode slot an ask holds, or a HEAD's answer that
// one is free (end nil: it holds none).
type transcodeHold struct {
	ask transcodeAsk
	// end ends the holder's request (and its ffmpeg with it).
	end context.CancelFunc
	// sent counts the bytes the holder has written to its response (writer).
	sent atomic.Int64
	// ended is set by the holder once its stream ran to its end
	// (media.Transcode), before endTranscode: a takeover then ended nothing.
	ended bool
	// running is, for a HEAD's hold of none, the GET of the same ask holding
	// a slot then (nil when none does), whose sent it reports.
	running *transcodeHold
}

// writer is w counting what the holder writes to it into h.sent (w itself
// for a HEAD's hold of none). It unwraps to w, so http.ResponseController's
// flushes and write deadlines (media's idle end) reach the connection.
func (h *transcodeHold) writer(w http.ResponseWriter) http.ResponseWriter {
	if h.end == nil {
		return w
	}
	return sentWriter{ResponseWriter: w, sent: &h.sent}
}

// sentWriter is a transcodeHold's writer.
type sentWriter struct {
	http.ResponseWriter
	sent *atomic.Int64
}

func (s sentWriter) Write(p []byte) (int, error) {
	n, err := s.ResponseWriter.Write(p)
	s.sent.Add(int64(n))
	return n, err
}

func (s sentWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// describe sets h's word on the same ask's running GET on a HEAD's answer
// (headerTranscodeSent), when h is a HEAD's and one runs.
func (h *transcodeHold) describe(w http.ResponseWriter) {
	if h.running != nil {
		w.Header().Set(headerTranscodeSent, strconv.FormatInt(h.running.sent.Load(), 10))
	}
}

// transcodeAskOf is r's ask for the transcode of rel in library lib. A start
// that is no positive number (none, a negative, NaN) is 0, as the transcode
// plays it (media's transcodeArgs): a NaN key never equals itself, so its hold
// would never be found again, its slot never let go.
func transcodeAskOf(r *http.Request, lib int64, rel string) transcodeAsk {
	q := r.URL.Query()
	start, _ := strconv.ParseFloat(q.Get("t"), 64)
	if !(start > 0) {
		start = 0
	}
	return transcodeAsk{
		token:    credentialFrom(r.Context()).ID,
		lib:      lib,
		rel:      rel,
		start:    start,
		download: q.Get("download") == "1",
	}
}

// takeTranscodeSlot takes a transcode slot for ask: the one a same ask holds
// (ending that ask's request, whose slot it now is), else a free one. It
// returns nil when no slot is free for it, else the hold and r with a context
// a later takeover ends. An ask with no token takes a free slot only, and is
// never taken over. A HEAD, which starts no ffmpeg, takes none: it gets a
// hold of no slot when a GET would get one (transcodeSlotFree), naming the
// same ask's GET that runs, if one does.
func (a *API) takeTranscodeSlot(r *http.Request, ask transcodeAsk) (*transcodeHold, *http.Request) {
	if r.Method == http.MethodHead {
		free, running := a.transcodeSlotFree(ask)
		if !free {
			return nil, r
		}
		return &transcodeHold{ask: ask, running: running}, r
	}
	keyed := ask.keyed()
	a.transcodeMu.Lock()
	defer a.transcodeMu.Unlock()
	prev := a.transcodeHolds[ask]
	if !keyed || prev == nil {
		select {
		case a.transcodeSem <- struct{}{}:
		default:
			return nil, r
		}
	}
	ctx, end := context.WithCancel(r.Context())
	h := &transcodeHold{ask: ask, end: end}
	if keyed {
		if prev != nil {
			prev.end()
		}
		a.transcodeHolds[ask] = h
	}
	return h, r.WithContext(ctx)
}

// transcodeSlotFree reports whether ask would get a slot now (a free one, or
// one a same ask holds, running), taking none: a HEAD's answer, so a client
// can ask whether its transcode would run without starting one.
func (a *API) transcodeSlotFree(ask transcodeAsk) (free bool, running *transcodeHold) {
	a.transcodeMu.Lock()
	defer a.transcodeMu.Unlock()
	if ask.keyed() {
		if h := a.transcodeHolds[ask]; h != nil {
			return true, h
		}
	}
	return len(a.transcodeSem) < cap(a.transcodeSem), nil
}

// endTranscode lets go of h's slot, once its request is done with it (nil, or
// a HEAD's hold of none, holds nothing). One a later same ask took over is
// that ask's now: the request is aborted instead (http.ErrAbortHandler), so
// its client sees a broken stream, never one that ended (which a player would
// take for the file's end), unless its stream had run to its end (h.ended)
// before the takeover came, which then cut nothing. Deferred by the request's
// handler: a panic of its own goes on as it was, never hidden behind the abort
// (which net/http does not log).
func (a *API) endTranscode(h *transcodeHold) {
	if h == nil || h.end == nil {
		return
	}
	h.end()
	keyed := h.ask.keyed()
	a.transcodeMu.Lock()
	taken := keyed && a.transcodeHolds[h.ask] != h
	if !taken {
		if keyed {
			delete(a.transcodeHolds, h.ask)
		}
		<-a.transcodeSem
	}
	a.transcodeMu.Unlock()
	if taken && !h.ended {
		if p := recover(); p != nil {
			panic(p)
		}
		panic(http.ErrAbortHandler)
	}
}
