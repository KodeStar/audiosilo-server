package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// slotRequest is a GET (or HEAD) carrying cred, as the auth middleware leaves it.
func slotRequest(method string, cred auth.Credential) *http.Request {
	r := httptest.NewRequest(method, "/", nil)
	return r.WithContext(withCredential(r.Context(), &auth.User{}, cred))
}

// slotAPI is an API with n transcode slots, every one but free taken.
func slotAPI(n, free int) *API {
	a := &API{transcodeSem: make(chan struct{}, n), transcodeHolds: make(map[transcodeAsk]*transcodeHold)}
	for range n - free {
		a.transcodeSem <- struct{}{}
	}
	return a
}

// takeSlot is a.takeTranscodeSlot for a request of method by ask's token,
// answered on a recorder.
func takeSlot(a *API, method string, ask transcodeAsk) (*transcodeHold, *http.Request) {
	return a.takeTranscodeSlot(httptest.NewRecorder(), slotRequest(method, auth.Credential{ID: ask.token}), ask)
}

// mustTakeSlot is takeSlot for a GET that must get a slot.
func mustTakeSlot(t *testing.T, a *API, ask transcodeAsk) (*transcodeHold, *http.Request) {
	t.Helper()
	h, r := takeSlot(a, http.MethodGet, ask)
	if h == nil {
		t.Fatalf("%+v got no slot", ask)
	}
	return h, r
}

// endTaken is endTranscode for a hold taken over: it reports whether it aborted.
func endTaken(a *API, h *transcodeHold) (aborted bool) {
	defer func() {
		aborted = recover() == http.ErrAbortHandler
	}()
	a.endTranscode(h)
	return false
}

// TestTranscodeRepeatAskTakesOverSlot: with one slot free, the same token's
// second ask for the same stream (Safari's range probing) takes the first's
// slot, ending its request, rather than being refused; the first's end then
// frees nothing, and the second's frees the slot.
func TestTranscodeRepeatAskTakesOverSlot(t *testing.T) {
	t.Parallel()
	a := slotAPI(2, 1)
	ask := transcodeAsk{token: 7, lib: 1, rel: "Book/01.mka", start: 12.5}
	first, r1 := takeSlot(a, http.MethodGet, ask)
	if first == nil {
		t.Fatal("the first ask got no slot with one free")
	}
	second, r2 := takeSlot(a, http.MethodGet, ask)
	if second == nil {
		t.Fatal("the repeat ask got no slot: it should take the first's over")
	}
	if !errors.Is(r1.Context().Err(), context.Canceled) {
		t.Fatal("the first ask's request was not ended by the takeover")
	}
	if r2.Context().Err() != nil {
		t.Fatal("the repeat ask's request was ended")
	}
	if len(a.transcodeSem) != 2 {
		t.Fatalf("%d slots taken after the takeover, want the same 2", len(a.transcodeSem))
	}
	if !endTaken(a, first) {
		t.Fatal("the request whose slot was taken over was not aborted")
	}
	if len(a.transcodeSem) != 2 {
		t.Fatal("the taken-over request's end freed the slot its successor holds")
	}
	if free, running := a.transcodeSlotFree(ask); !free || running != second {
		t.Fatal("a HEAD of the held ask was told busy, or not of its running GET: a GET would take its slot over")
	}
	if endTaken(a, second) {
		t.Fatal("the slot's holder was aborted at its own end")
	}
	if len(a.transcodeSem) != 1 || len(a.transcodeHolds) != 0 {
		t.Fatalf("after the holder's end: %d slots taken, %d holds, want 1 and 0", len(a.transcodeSem), len(a.transcodeHolds))
	}
}

// TestTranscodeTakeoverAfterItsEnd: a stream that ran to its end before a
// repeat ask took its slot over (between its last byte and its handler's end)
// is not aborted: the takeover cut nothing. Its end lets go of no slot, the
// successor's being the same one.
func TestTranscodeTakeoverAfterItsEnd(t *testing.T) {
	t.Parallel()
	a := slotAPI(2, 1)
	ask := transcodeAsk{token: 7, lib: 1, rel: "Book/01.mka"}
	first, _ := takeSlot(a, http.MethodGet, ask)
	first.ended = true
	second, _ := takeSlot(a, http.MethodGet, ask)
	if second == nil {
		t.Fatal("the repeat ask got no slot")
	}
	if endTaken(a, first) {
		t.Fatal("a stream that ran to its end was aborted by a takeover after it")
	}
	if len(a.transcodeSem) != 2 || a.transcodeHolds[ask] != second {
		t.Fatal("the ended stream's end let go of the slot its successor holds")
	}
	if endTaken(a, second) || len(a.transcodeSem) != 1 || len(a.transcodeHolds) != 0 {
		t.Fatal("the successor's end did not let go of the slot")
	}
}

// TestTranscodeHeadSaysWhatItsAskSent: a HEAD of an ask whose GET runs says
// how much that GET has written (headerTranscodeSent), through the writer its
// handler is given; a HEAD of the same file and start by another token, or by
// none, is told only whether a slot is free, never of another's stream.
func TestTranscodeHeadSaysWhatItsAskSent(t *testing.T) {
	t.Parallel()
	a := slotAPI(2, 2)
	ask := transcodeAsk{token: 7, lib: 1, rel: "Book/01.mka", start: 12.5}
	h, _ := takeSlot(a, http.MethodGet, ask)
	rec := httptest.NewRecorder()
	w := h.writer(rec)
	if _, err := w.Write(make([]byte, 1500)); err != nil {
		t.Fatal(err)
	}
	// The connection's flushes and deadlines reach it (media's idle end).
	if err := http.NewResponseController(w).Flush(); err != nil || !rec.Flushed {
		t.Fatalf("flush through the counting writer = %v (flushed %v)", err, rec.Flushed)
	}

	look := func(cred auth.Credential, ask transcodeAsk) http.Header {
		t.Helper()
		head, _ := a.takeTranscodeSlot(httptest.NewRecorder(), slotRequest(http.MethodHead, cred), ask)
		if head == nil {
			t.Fatal("a HEAD was told busy with a slot free")
		}
		rec := httptest.NewRecorder()
		head.describe(rec)
		if head.writer(rec) != http.ResponseWriter(rec) {
			t.Fatal("a HEAD's writer counts")
		}
		return rec.Header()
	}
	if got := look(auth.Credential{ID: 7}, ask).Get(headerTranscodeSent); got != "1500" {
		t.Fatalf("HEAD of the running ask: %s %q, want 1500", headerTranscodeSent, got)
	}
	other := ask
	other.token = 8
	if got := look(auth.Credential{ID: 8}, other).Values(headerTranscodeSent); got != nil {
		t.Fatalf("another token's HEAD of the same stream was told of it: %q", got)
	}
	none := ask
	none.token = 0
	if got := look(auth.Credential{}, none).Values(headerTranscodeSent); got != nil {
		t.Fatalf("a tokenless HEAD of the same stream was told of it: %q", got)
	}
	if endTaken(a, h) {
		t.Fatal("the holder was aborted")
	}
	if got := look(auth.Credential{ID: 7}, ask).Values(headerTranscodeSent); got != nil {
		t.Fatalf("HEAD after the GET ended still told of it: %q", got)
	}
}

// TestTranscodeAskStartNoNumber: a ?t= that is no positive number (NaN, a
// negative) is the ask from 0, as the transcode plays it: a NaN key would never
// be found again, so its end would neither let go of its slot nor stay
// unaborted, and a few such asks would take every slot for good.
func TestTranscodeAskStartNoNumber(t *testing.T) {
	t.Parallel()
	for _, start := range []string{"NaN", "-5", "0", ""} {
		r := slotRequest(http.MethodGet, auth.Credential{ID: 7})
		r.URL.RawQuery = url.Values{"t": {start}}.Encode()
		ask := transcodeAskOf(r, 1, "Book/01.mka")
		if ask.start != 0 {
			t.Fatalf("t=%q: start %v, want 0", start, ask.start)
		}
		a := slotAPI(1, 1)
		h, _ := a.takeTranscodeSlot(httptest.NewRecorder(), r, ask)
		if h == nil {
			t.Fatalf("t=%q: no slot with one free", start)
		}
		if endTaken(a, h) || len(a.transcodeSem) != 0 || len(a.transcodeHolds) != 0 {
			t.Fatalf("t=%q: its end left %d slots taken, %d holds", start, len(a.transcodeSem), len(a.transcodeHolds))
		}
	}
}

// TestTranscodeTakenOverKeepsItsPanic: a request whose slot was taken over
// that panics of its own goes on panicking with its own value, which net/http
// logs, not with the abort, which it does not.
func TestTranscodeTakenOverKeepsItsPanic(t *testing.T) {
	t.Parallel()
	a := slotAPI(1, 1)
	ask := transcodeAsk{token: 7, lib: 1, rel: "Book/01.mka"}
	first, _ := takeSlot(a, http.MethodGet, ask)
	if h, _ := takeSlot(a, http.MethodGet, ask); h == nil {
		t.Fatal("the repeat ask got no slot")
	}
	got := func() (p any) {
		defer func() { p = recover() }()
		defer a.endTranscode(first)
		panic("its own")
	}()
	if got != "its own" {
		t.Fatalf("panicked with %v, want its own panic", got)
	}
	if len(a.transcodeSem) != 1 {
		t.Fatal("the taken-over request's end let go of its successor's slot")
	}
}

// TestTranscodeOtherAsksCountSeparately: an ask that differs in token (another
// user's, or another device's), start, file or download takes a slot of its
// own, refused with none free, and never ends the holder's transcode.
func TestTranscodeOtherAsksCountSeparately(t *testing.T) {
	t.Parallel()
	held := transcodeAsk{token: 7, lib: 1, rel: "Book/01.mka", start: 12.5}
	for name, other := range map[string]transcodeAsk{
		"another token": {token: 8, lib: 1, rel: "Book/01.mka", start: 12.5},
		"another start": {token: 7, lib: 1, rel: "Book/01.mka", start: 30},
		"another file":  {token: 7, lib: 1, rel: "Book/02.mka", start: 12.5},
		"a download":    {token: 7, lib: 1, rel: "Book/01.mka", start: 12.5, download: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := slotAPI(2, 1)
			h, r := takeSlot(a, http.MethodGet, held)
			if h == nil {
				t.Fatal("the held ask got no slot")
			}
			if free, running := a.transcodeSlotFree(other); free || running != nil {
				t.Fatal("a HEAD of another ask was told a slot is free with none, or of the holder's stream")
			}
			if o, _ := takeSlot(a, http.MethodGet, other); o != nil {
				t.Fatal("another ask got a slot with none free")
			}
			if r.Context().Err() != nil {
				t.Fatal("another ask ended the holder's transcode")
			}
			if endTaken(a, h) || len(a.transcodeSem) != 1 {
				t.Fatalf("the holder's end: %d slots taken, want 1", len(a.transcodeSem))
			}
			// Now free, the other ask is admitted.
			if o, _ := takeSlot(a, http.MethodGet, other); o == nil {
				t.Fatal("another ask got no slot with one free")
			}
		})
	}
}

// TestTranscodeUnkeyedAsksNeverTakeOver: a HEAD (it starts no ffmpeg) takes
// no slot, told only whether a GET would get one, and an ask with no token
// takes a free slot only: neither ends a same ask's transcode, and the
// tokenless ask is refused with none free.
func TestTranscodeUnkeyedAsksNeverTakeOver(t *testing.T) {
	t.Parallel()
	ask := transcodeAsk{token: 7, lib: 1, rel: "Book/01.mka"}
	a := slotAPI(1, 1)
	_, r := takeSlot(a, http.MethodGet, ask)
	head, _ := takeSlot(a, http.MethodHead, ask)
	if head == nil {
		t.Fatal("a HEAD of the held ask was told busy: a GET would take its slot over")
	}
	if r.Context().Err() != nil {
		t.Fatal("a HEAD ended the holder's transcode")
	}
	if endTaken(a, head) || len(a.transcodeSem) != 1 || len(a.transcodeHolds) != 1 {
		t.Fatal("a HEAD's end let go of a slot it never held")
	}
	other := transcodeAsk{token: 8, lib: 1, rel: "Book/01.mka"}
	if h, _ := takeSlot(a, http.MethodHead, other); h != nil {
		t.Fatal("a HEAD of another ask was told a slot is free with none")
	}
	b := slotAPI(1, 1)
	none := transcodeAsk{lib: 1, rel: "Book/01.mka"}
	if h, _ := takeSlot(b, http.MethodGet, none); h == nil {
		t.Fatal("an ask with no token got no slot with one free")
	}
	if h, _ := takeSlot(b, http.MethodGet, none); h != nil {
		t.Fatal("a second ask with no token took over the first's slot")
	}
}

// standInFFmpeg writes a stand-in ffmpeg that sends bytes until it is killed,
// each run noting its pid on a line of pids.
func standInFFmpeg(t *testing.T) (ffmpeg, pids string) {
	t.Helper()
	dir := t.TempDir()
	pids = filepath.Join(dir, "pids")
	ffmpeg = filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\necho $$ >> '" + pids + "'\nexec cat /dev/zero\n"
	// Written under ForkLock: a parallel test's fork must not inherit its open
	// write descriptor, or exec fails with ETXTBSY (golang/go#22315).
	syscall.ForkLock.RLock()
	err := os.WriteFile(ffmpeg, []byte(script), 0o755)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	return ffmpeg, pids
}

// TestStreamTranscodeRepeatAsk: over HTTP, with one slot free, a repeat GET of
// the same transcode URL with the same token plays and the earlier one's
// stream breaks off (an error, not an end); the same URL with another user's
// token, and the same token from another start, are 503 busy and leave the
// stream playing. A HEAD answers whether a GET would run (200 for the held
// URL, 503 for another) and starts no ffmpeg; the slot is free again once
// the stream is closed.
func TestStreamTranscodeRepeatAsk(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	ctx := context.Background()
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	ffmpeg, runs := standInFFmpeg(t)
	e.api.ffmpeg = ffmpeg
	countRuns := func() int {
		b, _ := os.ReadFile(runs)
		return bytes.Count(b, []byte("\n"))
	}
	for range cap(e.api.transcodeSem) - 1 {
		e.api.transcodeSem <- struct{}{}
	}

	mine, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	other, _ := e.auth.CreateUser(ctx, "other", "other-password", auth.RoleAdmin)
	theirs, _ := e.auth.IssueToken(ctx, other.ID, auth.KindSession, "t", 0)
	stream := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10) + "/stream?transcode=1&path=" +
		url.QueryEscape("Will Wight/Cradle/01 - Unsouled.m4b")
	at := stream + "&t=12.5"
	client := &http.Client{Transport: &http.Transport{}, Timeout: 30 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	open := func(method, path, token string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, e.srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	status := func(method, path, token string) int {
		t.Helper()
		return e.status(t, method, path, token, "")
	}
	readSome := func(name string, resp *http.Response) {
		t.Helper()
		if _, err := io.ReadFull(resp.Body, make([]byte, 4096)); err != nil {
			t.Fatalf("%s: reading its stream: %v", name, err)
		}
	}

	first := open("GET", at, mine)
	defer first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first ask = %d, want 200 with one slot free", first.StatusCode)
	}
	readSome("first ask", first)

	second := open("GET", at, mine)
	defer second.Body.Close()
	if second.StatusCode != http.StatusOK {
		t.Fatalf("repeat ask = %d, want 200: it takes the first's slot over", second.StatusCode)
	}
	readSome("repeat ask", second)
	if _, err := io.Copy(io.Discard, first.Body); err == nil {
		t.Fatal("the taken-over stream ended cleanly: a player would take it for the file's end")
	}

	// Opened, not read: a 200 would stream until the test timed out.
	refused := func(name, path, token string) {
		t.Helper()
		resp := open("GET", path, token)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s = %d, want 503", name, resp.StatusCode)
		}
	}
	refused("another user's same ask", at, theirs)
	refused("another start", stream+"&t=40", mine)
	ran := countRuns()
	// A HEAD says what the same ask's running GET has sent (its client read
	// 4 KiB of it by now), and never of another user's stream.
	head := func(token string) (int, string) {
		t.Helper()
		resp := open("HEAD", at, token)
		_ = resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("X-AudioSilo-Transcode-Sent")
	}
	code, sent := head(mine)
	if n, err := strconv.Atoi(sent); code != http.StatusOK || err != nil || n < 4096 {
		t.Fatalf("HEAD of the held ask = %d with %q sent, want 200 and at least what was read", code, sent)
	}
	if code, sent := head(theirs); code != http.StatusServiceUnavailable || sent != "" {
		t.Fatalf("HEAD of another user's ask = %d with %q sent, want 503 and none", code, sent)
	}
	if countRuns() != ran {
		t.Fatal("a HEAD ran ffmpeg")
	}
	readSome("repeat ask after the refusals", second)

	_ = second.Body.Close()
	deadline := time.Now().Add(10 * time.Second)
	for len(e.api.transcodeSem) == cap(e.api.transcodeSem) {
		if time.Now().After(deadline) {
			t.Fatal("the closed stream never let go of its slot")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := status("HEAD", at, theirs); got != http.StatusOK {
		t.Fatalf("HEAD with the slot free = %d, want 200", got)
	}
	if code, sent := head(mine); code != http.StatusOK || sent != "" {
		t.Fatalf("HEAD of the closed ask = %d with %q sent, want 200 and none running", code, sent)
	}
	if len(e.api.transcodeSem) != cap(e.api.transcodeSem)-1 || len(e.api.transcodeHolds) != 0 {
		t.Fatal("a HEAD with the slot free took it")
	}
}

// TestTranscodeTokenCapTakesOverItsOldest: a token holding
// maxTranscodesPerToken slots that asks for another stream (asks differing
// only in ?t=, the web player's overlapping seeks) takes over its own oldest
// one's slot, ending that request, rather than a slot of its own: the global
// count is unchanged, and another token's slots, older ones included, are
// never touched.
func TestTranscodeTokenCapTakesOverItsOldest(t *testing.T) {
	t.Parallel()
	a := slotAPI(maxConcurrentTranscodes, maxConcurrentTranscodes)
	take := func(token int64, start float64) (*transcodeHold, *http.Request) {
		t.Helper()
		return mustTakeSlot(t, a, transcodeAsk{token: token, lib: 1, rel: "Book/01.mka", start: start})
	}
	// Another token's two slots, taken first: the oldest of all.
	_, theirs1 := take(8, 0)
	_, theirs2 := take(8, 5)
	oldest, r1 := take(7, 0)
	_, r2 := take(7, 10)
	if len(a.transcodeSem) != 4 {
		t.Fatalf("%d slots taken, want 4", len(a.transcodeSem))
	}
	if free, _ := a.transcodeSlotFree(transcodeAsk{token: 7, lib: 1, rel: "Book/01.mka", start: 20}); !free {
		t.Fatal("a HEAD of the token's third ask was told busy: a GET would take its oldest over")
	}
	third, r3 := take(7, 20)
	if r1.Context().Err() == nil || !oldest.taken.Load() {
		t.Fatal("the token's oldest transcode was not ended by its third ask")
	}
	if r2.Context().Err() != nil || r3.Context().Err() != nil {
		t.Fatal("the token's newer transcodes were ended")
	}
	if theirs1.Context().Err() != nil || theirs2.Context().Err() != nil {
		t.Fatal("another token's transcode was ended")
	}
	if len(a.transcodeSem) != 4 || len(a.transcodeHolds) != 4 {
		t.Fatalf("after the takeover: %d slots taken, %d holds, want 4 and 4", len(a.transcodeSem), len(a.transcodeHolds))
	}
	if !endTaken(a, oldest) || len(a.transcodeSem) != 4 {
		t.Fatal("the taken-over oldest was not aborted, or let go of the slot it no longer holds")
	}
	if n, err := oldest.writer(httptest.NewRecorder()).Write([]byte("x")); n != 0 || !errors.Is(err, errTranscodeTaken) {
		t.Fatalf("a write to the taken-over stream = %d, %v; want refused", n, err)
	}
	if endTaken(a, third) || len(a.transcodeSem) != 3 {
		t.Fatalf("the third ask's end: %d slots taken, want 3", len(a.transcodeSem))
	}
}

// TestTranscodeGlobalCapAcrossTokens: with every slot held by a different
// token, a GET from a fifth token and a second ask from one of them are both
// refused (each is under its own cap, so takes over nothing), a HEAD of either
// is told busy, and no holder's transcode is ended.
func TestTranscodeGlobalCapAcrossTokens(t *testing.T) {
	t.Parallel()
	a := slotAPI(maxConcurrentTranscodes, maxConcurrentTranscodes)
	var held []*http.Request
	for token := int64(1); token <= maxConcurrentTranscodes; token++ {
		h, r := takeSlot(a, http.MethodGet, transcodeAsk{token: token, lib: 1, rel: "Book/01.mka"})
		if h == nil {
			t.Fatalf("token %d got no slot with one free", token)
		}
		held = append(held, r)
	}
	for name, ask := range map[string]transcodeAsk{
		"a fifth token":              {token: 5, lib: 1, rel: "Book/01.mka"},
		"a holder's second-ask seek": {token: 1, lib: 1, rel: "Book/01.mka", start: 30},
	} {
		if h, _ := takeSlot(a, http.MethodGet, ask); h != nil {
			t.Fatalf("%s got a slot with every one held", name)
		}
		if free, _ := a.transcodeSlotFree(ask); free {
			t.Fatalf("a HEAD of %s was told a slot is free", name)
		}
	}
	for i, r := range held {
		if r.Context().Err() != nil {
			t.Fatalf("token %d's transcode was ended", i+1)
		}
	}
	if len(a.transcodeSem) != maxConcurrentTranscodes || len(a.transcodeHolds) != maxConcurrentTranscodes {
		t.Fatal("a refused ask changed the slots held")
	}
}

// TestTranscodeTokenCapSparesItsDownload: a token at its cap whose oldest
// hold is a running download takes over its stream instead (a download is
// never ended by another ask: its saved file would be cut short); a token at
// its cap with only downloads is busy (503 with Retry-After, its HEAD told
// busy) even with slots free, and a repeat of a download's own ask still takes
// that download over.
func TestTranscodeTokenCapSparesItsDownload(t *testing.T) {
	t.Parallel()
	a := slotAPI(maxConcurrentTranscodes, maxConcurrentTranscodes)
	dl := transcodeAsk{token: 7, lib: 1, rel: "Book/01.mka", download: true}
	_, dlReq := mustTakeSlot(t, a, dl)
	stream, streamReq := mustTakeSlot(t, a, transcodeAsk{token: 7, lib: 1, rel: "Book/02.mka", start: 10})
	seek := transcodeAsk{token: 7, lib: 1, rel: "Book/03.mka", start: 20}
	if free, _ := a.transcodeSlotFree(seek); !free {
		t.Fatal("a HEAD of the token's third ask was told busy: a GET would take its stream over")
	}
	third, _ := mustTakeSlot(t, a, seek)
	if dlReq.Context().Err() != nil {
		t.Fatal("the token's third ask ended its running download")
	}
	if streamReq.Context().Err() == nil || !stream.taken.Load() {
		t.Fatal("the token's third ask did not take its stream over")
	}
	if !endTaken(a, stream) || len(a.transcodeSem) != 2 {
		t.Fatalf("after the takeover: %d slots taken, want 2", len(a.transcodeSem))
	}
	if endTaken(a, third) {
		t.Fatal("the third ask was aborted at its own end")
	}

	// Two downloads: the token's next ask is busy though slots are free.
	dl2 := transcodeAsk{token: 7, lib: 1, rel: "Book/02.mka", download: true}
	_, dl2Req := mustTakeSlot(t, a, dl2)
	if free, running := a.transcodeSlotFree(seek); free || running != nil {
		t.Fatal("a HEAD of a token holding only downloads at its cap was told a slot is free")
	}
	a.ffmpeg = "ffmpeg" // beginTranscode's check only: nothing runs
	rec := httptest.NewRecorder()
	if h, _ := a.beginTranscode(rec, slotRequest(http.MethodGet, auth.Credential{ID: 7}), 1, "Book/03.mka"); h != nil {
		t.Fatal("a token holding only downloads at its cap got a slot")
	}
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "5" {
		t.Fatalf("the refused ask = %d with Retry-After %q, want 503 and 5", rec.Code, rec.Header().Get("Retry-After"))
	}
	if dlReq.Context().Err() != nil || dl2Req.Context().Err() != nil {
		t.Fatal("a refused ask ended a download")
	}
	if len(a.transcodeSem) != 2 || len(a.transcodeHolds) != 2 {
		t.Fatal("a refused ask changed the slots held")
	}

	// A repeat of a download's own ask (a client retrying it) takes it over.
	again, _ := mustTakeSlot(t, a, dl)
	if dlReq.Context().Err() == nil {
		t.Fatal("a repeat of the download's own ask did not take its slot over")
	}
	if a.transcodeHolds[dl] != again || len(a.transcodeSem) != 2 {
		t.Fatal("the repeat download ask does not hold the download's slot")
	}
}

// TestTranscodeTokenCapPrefersSameFile: a token at its cap asking for a file
// it streams already (a seek in one tab) takes over that file's earlier ask,
// not its oldest overall (another tab's stream on the same token, which plays
// on); a HEAD of the ask says the same (holdToTake), and with no hold on the
// file the oldest is taken.
func TestTranscodeTokenCapPrefersSameFile(t *testing.T) {
	t.Parallel()
	a := slotAPI(maxConcurrentTranscodes, maxConcurrentTranscodes)
	take := func(rel string, start float64) (*transcodeHold, *http.Request) {
		t.Helper()
		return mustTakeSlot(t, a, transcodeAsk{token: 7, lib: 1, rel: rel, start: start})
	}
	otherTab, otherReq := take("Book/01.mka", 0)
	thisTab, thisReq := take("Book/05.mka", 0)
	seek := transcodeAsk{token: 7, lib: 1, rel: "Book/05.mka", start: 30}
	a.transcodeMu.Lock()
	picked, refused := a.holdToTake(seek)
	a.transcodeMu.Unlock()
	if picked != thisTab || refused {
		t.Fatal("holdToTake (a HEAD's answer) did not pick the same file's earlier ask")
	}
	if free, running := a.transcodeSlotFree(seek); !free || running != nil {
		t.Fatal("a HEAD of the seek was told busy, or of a running GET of another ask")
	}
	seeked, _ := take("Book/05.mka", 30)
	if thisReq.Context().Err() == nil || !thisTab.taken.Load() {
		t.Fatal("the seek did not take over the same file's earlier ask")
	}
	if otherReq.Context().Err() != nil || otherTab.taken.Load() {
		t.Fatal("the seek ended the token's oldest stream, another file's")
	}
	if !endTaken(a, thisTab) {
		t.Fatal("the taken-over stream was not aborted")
	}

	// Neither of the token's holds is on this file: its oldest is taken.
	_, _ = take("Book/09.mka", 0)
	if otherReq.Context().Err() == nil {
		t.Fatal("an ask for a file the token streams nowhere did not take over its oldest")
	}
	if seeked.taken.Load() {
		t.Fatal("an ask for another file took over the newer hold")
	}
}

// TestStreamTranscodeDownloadNamed: ?transcode=1&download=1 is an attachment
// named for the file with the transcode's extension, as a plain download is
// named for the file (a GET and a HEAD); without download=1 it is none.
func TestStreamTranscodeDownloadNamed(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	ctx := context.Background()
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	e.api.ffmpeg, _ = standInFFmpeg(t)
	tok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	stream := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10) + "/stream?transcode=1&path=" +
		url.QueryEscape("Will Wight/Cradle/01 - Unsouled.m4b")
	client := &http.Client{Transport: &http.Transport{}, Timeout: 30 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	disposition := func(method, path string) string {
		t.Helper()
		req, _ := http.NewRequest(method, e.srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close() // opened, not read: the stand-in streams until killed
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s = %d, want 200", method, path, resp.StatusCode)
		}
		return resp.Header.Get("Content-Disposition")
	}
	want := `attachment; filename="01 - Unsouled.mp3"`
	for _, method := range []string{"GET", "HEAD"} {
		if got := disposition(method, stream+"&download=1"); got != want {
			t.Fatalf("%s of a transcoded download: Content-Disposition %q, want %q", method, got, want)
		}
	}
	if got := disposition("HEAD", stream); got != "" {
		t.Fatalf("a transcoded stream (no download): Content-Disposition %q, want none", got)
	}
}

// deadlineRecorder is a recorder that notes the write deadlines set on it
// (http.ResponseController's SetWriteDeadline) and its flushes.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	mu        sync.Mutex
	deadlines []time.Time
	flushes   int
}

func (d *deadlineRecorder) SetWriteDeadline(at time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deadlines = append(d.deadlines, at)
	return nil
}

func (d *deadlineRecorder) FlushError() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.flushes++
	return nil
}

// last is the write deadline set last (zero when none was).
func (d *deadlineRecorder) last() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.deadlines) == 0 {
		return time.Time{}
	}
	return d.deadlines[len(d.deadlines)-1]
}

// TestTranscodeTakeoverWriteDeadline: a takeover gives the response it takes
// the slot from a write deadline transcodeTakeoverGrace on. That response's
// abort puts the deadline back after media's idle copy cleared it (copyIdle,
// at its end), so net/http's flush as it closes the connection meets it; one
// whose stream had run to its end flushes under it, then leaves its kept-alive
// connection with none.
func TestTranscodeTakeoverWriteDeadline(t *testing.T) {
	t.Parallel()
	for _, ended := range []bool{false, true} {
		a := slotAPI(1, 1)
		ask := transcodeAsk{token: 7, lib: 1, rel: "Book/01.mka"}
		w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		first, _ := a.takeTranscodeSlot(w, slotRequest(http.MethodGet, auth.Credential{ID: 7}), ask)
		before := time.Now()
		if h, _ := takeSlot(a, http.MethodGet, ask); h == nil {
			t.Fatal("the repeat ask got no slot")
		}
		set := w.last()
		if set.Before(before.Add(transcodeTakeoverGrace)) || set.After(time.Now().Add(transcodeTakeoverGrace)) {
			t.Fatalf("ended %v: the takeover's write deadline is %v, want %v on", ended, set, transcodeTakeoverGrace)
		}
		if !ended {
			// The chapter editor's idle copy clears its deadline as it ends
			// (copyIdle); the web player's has none to clear.
			_ = http.NewResponseController(first.writer(w)).SetWriteDeadline(time.Time{})
		}
		first.ended = ended
		if aborted := endTaken(a, first); aborted == ended {
			t.Fatalf("ended %v: aborted %v", ended, aborted)
		}
		want, flushes := set, 0
		if ended {
			want, flushes = time.Time{}, 1
		}
		if got := w.last(); !got.Equal(want) || w.flushes != flushes {
			t.Fatalf("ended %v: left with write deadline %v after %d flushes, want %v after %d", ended, got, w.flushes, want, flushes)
		}
	}
}
