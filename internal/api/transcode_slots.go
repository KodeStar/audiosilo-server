package api

import (
	"context"
	"net/http"
	"strconv"
)

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

// transcodeHold is one transcode slot an ask holds.
type transcodeHold struct {
	ask transcodeAsk
	// end ends the holder's request (and its ffmpeg with it).
	end context.CancelFunc
	// keyed: in transcodeHolds, for a same ask to take over. takenOver: a
	// same ask took the slot. Both guarded by transcodeMu.
	keyed, takenOver bool
}

// transcodeStart is r's ?t= start in seconds (0 when absent or not a number).
func transcodeStart(r *http.Request) float64 {
	start, _ := strconv.ParseFloat(r.URL.Query().Get("t"), 64)
	return start
}

// transcodeAskOf is r's ask for the transcode of rel in library lib.
func transcodeAskOf(r *http.Request, lib int64, rel string) transcodeAsk {
	return transcodeAsk{
		token:    credentialFrom(r.Context()).ID,
		lib:      lib,
		rel:      rel,
		start:    transcodeStart(r),
		download: r.URL.Query().Get("download") == "1",
	}
}

// takeTranscodeSlot takes a transcode slot for ask: the one a same ask holds
// (ending that ask's request, whose slot it now is), else a free one. It
// returns nil when every slot is busy, else the hold and r with a context a
// later takeover ends. A HEAD, which starts no ffmpeg, and an ask with no
// token take a free slot only, and are never taken over.
func (a *API) takeTranscodeSlot(r *http.Request, ask transcodeAsk) (*transcodeHold, *http.Request) {
	keyed := ask.token != 0 && r.Method != http.MethodHead
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
	h := &transcodeHold{ask: ask, end: end, keyed: keyed}
	if keyed {
		if prev != nil {
			prev.takenOver = true
			prev.end()
		}
		if a.transcodeHolds == nil {
			a.transcodeHolds = make(map[transcodeAsk]*transcodeHold)
		}
		a.transcodeHolds[ask] = h
	}
	return h, r.WithContext(ctx)
}

// transcodeSlotFree reports whether ask would get a slot now (a free one, or
// one a same ask holds), taking none: a HEAD's answer, so a client can ask
// whether its transcode would run without starting one.
func (a *API) transcodeSlotFree(ask transcodeAsk) bool {
	a.transcodeMu.Lock()
	defer a.transcodeMu.Unlock()
	if ask.token != 0 && a.transcodeHolds[ask] != nil {
		return true
	}
	return len(a.transcodeSem) < cap(a.transcodeSem)
}

// endTranscode lets go of h's slot, once its request is done with it. One a
// later same ask took over is that ask's now: the request is aborted instead
// (http.ErrAbortHandler), so its client sees a broken stream, never one that
// ended (which a player would take for the file's end).
func (a *API) endTranscode(h *transcodeHold) {
	h.end()
	a.transcodeMu.Lock()
	taken := h.takenOver
	if !taken {
		if h.keyed {
			delete(a.transcodeHolds, h.ask)
		}
		<-a.transcodeSem
	}
	a.transcodeMu.Unlock()
	if taken {
		panic(http.ErrAbortHandler)
	}
}
