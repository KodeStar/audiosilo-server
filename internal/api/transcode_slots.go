package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// headerTranscodeSent is a transcode HEAD's word on the same ask's GET that
// is running: the bytes it has written to its response so far. Absent when no
// GET of the ask runs (none was asked for, it ended, or it was dropped). A
// client whose stream stalls looks twice: a count that grows is a slow
// transcode still producing, to wait for, where asking again would take its
// slot over and end it. Only ever the caller's own ask's (its token's).
const headerTranscodeSent = "X-AudioSilo-Transcode-Sent"

// transcodeTakeoverGrace is the write deadline a takeover gives the response
// it takes the slot from: a write blocked on a client that stopped reading
// then fails, so its handler goes on to reap its ffmpeg and let go of its
// connection rather than waiting for the client to close. Long enough for a
// stream that ran to its end to flush its last bytes to a client reading it.
const transcodeTakeoverGrace = time.Second

// errTranscodeTaken is a write refused to a transcode whose slot was taken over.
var errTranscodeTaken = errors.New("transcode slot taken over")

// transcodeAsk is what a transcode streams and for whom: the credential's
// token, the file, the ?t= start and whether it is a download. Safari asks for
// one stream two or three times (its range probing), each a GET of its own,
// and plays only the last: an ask the same token made before for the same
// thing has its slot taken over (takeTranscodeSlot) rather than the new one
// needing another, so with one slot free the asks don't shut each other out.
// A token is one user's, so a takeover never ends another user's transcode.
// A token holds at most maxTranscodesPerToken slots: its next ask takes over
// its own oldest one's, never another token's.
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
	// rc is the holder's response, for a takeover's write deadline (takeOver).
	rc *http.ResponseController
	// seq orders a token's holds, for its oldest to be taken over.
	seq uint64
	// taken is set by a takeover, before its write deadline: the holder's
	// writer refuses every write after it.
	taken atomic.Bool
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
// for a HEAD's hold of none), refusing every write once the slot is taken
// over. It unwraps to w, so http.ResponseController's flushes and write
// deadlines (media's idle end) reach the connection.
func (h *transcodeHold) writer(w http.ResponseWriter) http.ResponseWriter {
	if h.end == nil {
		return w
	}
	return sentWriter{ResponseWriter: w, hold: h}
}

// sentWriter is a transcodeHold's writer. Its check of taken comes after any
// deadline media set for the write (copyIdle's, which wraps it), so a
// takeover's deadline, set after taken, is the one a write it lets through
// meets.
type sentWriter struct {
	http.ResponseWriter
	hold *transcodeHold
}

func (s sentWriter) Write(p []byte) (int, error) {
	if s.hold.taken.Load() {
		return 0, errTranscodeTaken
	}
	n, err := s.ResponseWriter.Write(p)
	s.hold.sent.Add(int64(n))
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

// takeTranscodeSlot takes a transcode slot for ask, answered on w: the one a
// same ask holds, else, when ask's token holds maxTranscodesPerToken already,
// the one its oldest ask holds (ending that request, whose slot it now is:
// takeOver), else a free one. It returns nil when no slot is free for it,
// else the hold and r with a context a later takeover ends. An ask with no
// token takes a free slot only, and is never taken over. A HEAD, which starts
// no ffmpeg, takes none: it gets a hold of no slot when a GET would get one
// (transcodeSlotFree), naming the same ask's GET that runs, if one does.
func (a *API) takeTranscodeSlot(w http.ResponseWriter, r *http.Request, ask transcodeAsk) (*transcodeHold, *http.Request) {
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
	var prev *transcodeHold
	if keyed {
		prev = a.transcodeHolds[ask]
		if prev == nil {
			prev = a.tokenOldestAtCap(ask.token)
		}
	}
	if prev == nil {
		select {
		case a.transcodeSem <- struct{}{}:
		default:
			return nil, r
		}
	}
	ctx, end := context.WithCancel(r.Context())
	h := &transcodeHold{ask: ask, end: end, rc: http.NewResponseController(w)}
	if keyed {
		if prev != nil {
			delete(a.transcodeHolds, prev.ask)
			prev.takeOver()
		}
		a.transcodeSeq++
		h.seq = a.transcodeSeq
		a.transcodeHolds[ask] = h
	}
	return h, r.WithContext(ctx)
}

// tokenOldestAtCap is the oldest hold of token's when it holds
// maxTranscodesPerToken slots, else nil. The caller holds transcodeMu.
func (a *API) tokenOldestAtCap(token int64) *transcodeHold {
	var oldest *transcodeHold
	n := 0
	for ask, h := range a.transcodeHolds {
		if ask.token != token {
			continue
		}
		n++
		if oldest == nil || h.seq < oldest.seq {
			oldest = h
		}
	}
	if n < maxTranscodesPerToken {
		return nil
	}
	return oldest
}

// takeOver ends h's request, its slot another ask's now: its writer refuses
// every write from here on, and a write already blocked on a client that
// stopped reading fails by transcodeTakeoverGrace (a write deadline, which
// net/http lets another goroutine set), so its handler reaps its ffmpeg and
// lets go of the connection. The caller holds transcodeMu, so endTranscode
// sees the deadline set.
func (h *transcodeHold) takeOver() {
	h.taken.Store(true)
	h.end()
	_ = h.rc.SetWriteDeadline(time.Now().Add(transcodeTakeoverGrace)) // ErrNotSupported: none
}

// transcodeSlotFree reports whether ask would get a slot now (a free one, one
// a same ask holds, running, or its token's oldest at the token's cap),
// taking none: a HEAD's answer, so a client can ask whether its transcode
// would run without starting one.
func (a *API) transcodeSlotFree(ask transcodeAsk) (free bool, running *transcodeHold) {
	a.transcodeMu.Lock()
	defer a.transcodeMu.Unlock()
	if ask.keyed() {
		if h := a.transcodeHolds[ask]; h != nil {
			return true, h
		}
		if a.tokenOldestAtCap(ask.token) != nil {
			return true, nil
		}
	}
	return len(a.transcodeSem) < cap(a.transcodeSem), nil
}

// endTranscode lets go of h's slot, once its request is done with it (nil, or
// a HEAD's hold of none, holds nothing). One a later ask took over is that
// ask's now: the request is aborted instead (http.ErrAbortHandler), so its
// client sees a broken stream, never one that ended (which a player would
// take for the file's end), unless its stream had run to its end (h.ended)
// before the takeover came, which then cut nothing (its connection, kept
// alive, loses the takeover's write deadline). Deferred by the request's
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
	if taken && h.ended {
		_ = h.rc.SetWriteDeadline(time.Time{})
	}
	if taken && !h.ended {
		if p := recover(); p != nil {
			panic(p)
		}
		panic(http.ErrAbortHandler)
	}
}
