package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
	first, r1 := a.takeTranscodeSlot(httptest.NewRecorder(), slotRequest(http.MethodGet, auth.Credential{ID: 7}), ask)
	if first == nil {
		t.Fatal("the first ask got no slot with one free")
	}
	second, r2 := a.takeTranscodeSlot(httptest.NewRecorder(), slotRequest(http.MethodGet, auth.Credential{ID: 7}), ask)
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
	first, _ := a.takeTranscodeSlot(httptest.NewRecorder(), slotRequest(http.MethodGet, auth.Credential{ID: 7}), ask)
	first.ended = true
	second, _ := a.takeTranscodeSlot(httptest.NewRecorder(), slotRequest(http.MethodGet, auth.Credential{ID: 7}), ask)
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
	h, _ := a.takeTranscodeSlot(httptest.NewRecorder(), slotRequest(http.MethodGet, auth.Credential{ID: 7}), ask)
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
	first, _ := a.takeTranscodeSlot(httptest.NewRecorder(), slotRequest(http.MethodGet, auth.Credential{ID: 7}), ask)
	if h, _ := a.takeTranscodeSlot(httptest.NewRecorder(), slotRequest(http.MethodGet, auth.Credential{ID: 7}), ask); h == nil {
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
			h, r := a.takeTranscodeSlot(httptest.NewRecorder(), slotRequest(http.MethodGet, auth.Credential{ID: held.token}), held)
			if h == nil {
				t.Fatal("the held ask got no slot")
			}
			if free, running := a.transcodeSlotFree(other); free || running != nil {
				t.Fatal("a HEAD of another ask was told a slot is free with none, or of the holder's stream")
			}
			if o, _ := a.takeTranscodeSlot(httptest.NewRecorder(), slotRequest(http.MethodGet, auth.Credential{ID: other.token}), other); o != nil {
				t.Fatal("another ask got a slot with none free")
			}
			if r.Context().Err() != nil {
				t.Fatal("another ask ended the holder's transcode")
			}
			if endTaken(a, h) || len(a.transcodeSem) != 1 {
				t.Fatalf("the holder's end: %d slots taken, want 1", len(a.transcodeSem))
			}
			// Now free, the other ask is admitted.
			if o, _ := a.takeTranscodeSlot(httptest.NewRecorder(), slotRequest(http.MethodGet, auth.Credential{ID: other.token}), other); o == nil {
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
	_, r := a.takeTranscodeSlot(httptest.NewRecorder(), slotRequest(http.MethodGet, auth.Credential{ID: 7}), ask)
	head, _ := a.takeTranscodeSlot(httptest.NewRecorder(), slotRequest(http.MethodHead, auth.Credential{ID: 7}), ask)
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
	if h, _ := a.takeTranscodeSlot(httptest.NewRecorder(), slotRequest(http.MethodHead, auth.Credential{ID: 8}), other); h != nil {
		t.Fatal("a HEAD of another ask was told a slot is free with none")
	}
	b := slotAPI(1, 1)
	none := transcodeAsk{lib: 1, rel: "Book/01.mka"}
	if h, _ := b.takeTranscodeSlot(httptest.NewRecorder(), slotRequest(http.MethodGet, auth.Credential{}), none); h == nil {
		t.Fatal("an ask with no token got no slot with one free")
	}
	if h, _ := b.takeTranscodeSlot(httptest.NewRecorder(), slotRequest(http.MethodGet, auth.Credential{}), none); h != nil {
		t.Fatal("a second ask with no token took over the first's slot")
	}
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
	// A stand-in ffmpeg that sends MP3 bytes until it is killed, counting its runs.
	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	ffmpeg := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\necho run >> '" + runs + "'\nexec cat /dev/zero\n"
	// Written under ForkLock: a parallel test's fork must not inherit its open
	// write descriptor, or exec fails with ETXTBSY (golang/go#22315).
	syscall.ForkLock.RLock()
	err := os.WriteFile(ffmpeg, []byte(script), 0o755)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	e.api.ffmpeg = ffmpeg
	countRuns := func() int {
		b, _ := os.ReadFile(runs)
		return len(b) / len("run\n")
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
		h, r := a.takeTranscodeSlot(httptest.NewRecorder(), slotRequest(http.MethodGet, auth.Credential{ID: token}),
			transcodeAsk{token: token, lib: 1, rel: "Book/01.mka", start: start})
		if h == nil {
			t.Fatalf("token %d at t=%v got no slot", token, start)
		}
		return h, r
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
	if n, err := (sentWriter{ResponseWriter: httptest.NewRecorder(), hold: oldest}).Write([]byte("x")); n != 0 || !errors.Is(err, errTranscodeTaken) {
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
		h, r := a.takeTranscodeSlot(httptest.NewRecorder(), slotRequest(http.MethodGet, auth.Credential{ID: token}),
			transcodeAsk{token: token, lib: 1, rel: "Book/01.mka"})
		if h == nil {
			t.Fatalf("token %d got no slot with one free", token)
		}
		held = append(held, r)
	}
	for name, ask := range map[string]transcodeAsk{
		"a fifth token":              {token: 5, lib: 1, rel: "Book/01.mka"},
		"a holder's second-ask seek": {token: 1, lib: 1, rel: "Book/01.mka", start: 30},
	} {
		if h, _ := a.takeTranscodeSlot(httptest.NewRecorder(), slotRequest(http.MethodGet, auth.Credential{ID: ask.token}), ask); h != nil {
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

// TestStreamTranscodeTakeoverStuckWriter: over HTTP, a transcode whose client
// stopped reading (its handler blocked writing) and whose slot a repeat ask
// takes over ends promptly, its ffmpeg reaped, rather than waiting for the
// client to close: the takeover's write deadline fails the blocked write.
func TestStreamTranscodeTakeoverStuckWriter(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	ctx := context.Background()
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	// A stand-in ffmpeg that sends MP3 bytes until it is killed, noting its pid.
	dir := t.TempDir()
	pids := filepath.Join(dir, "pids")
	ffmpeg := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\necho $$ >> '" + pids + "'\nexec cat /dev/zero\n"
	syscall.ForkLock.RLock() // as TestStreamTranscodeRepeatAsk (golang/go#22315)
	err := os.WriteFile(ffmpeg, []byte(script), 0o755)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	e.api.ffmpeg = ffmpeg
	mine, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	at := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10) + "/stream?transcode=1&t=5&path=" +
		url.QueryEscape("Will Wight/Cradle/01 - Unsouled.m4b")
	// A small, fixed receive buffer: the unread stream's writes block for
	// good, not only until the loopback's buffers grow.
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetReadBuffer(16 << 10)
		}
		return c, err
	}
	client := &http.Client{Transport: &http.Transport{DialContext: dial}, Timeout: 30 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	open := func() *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+at, nil)
		req.Header.Set("Authorization", "Bearer "+mine)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("ask = %d, want 200", resp.StatusCode)
		}
		return resp
	}
	sent := func() int64 {
		e.api.transcodeMu.Lock()
		defer e.api.transcodeMu.Unlock()
		for _, h := range e.api.transcodeHolds {
			return h.sent.Load()
		}
		return -1
	}

	stuck := open() // never read: its handler's writes block once the buffers fill
	defer stuck.Body.Close()
	until := time.Now().Add(10 * time.Second)
	for last := int64(-1); ; {
		time.Sleep(200 * time.Millisecond)
		n := sent()
		if n > 0 && n == last {
			break // the handler is blocked writing
		}
		if time.Now().After(until) {
			t.Fatal("the unread stream's writes never blocked")
		}
		last = n
	}
	b, _ := os.ReadFile(pids)
	pid, err := strconv.Atoi(string(b[:len(b)-1]))
	if err != nil {
		t.Fatalf("the stand-in ffmpeg's pid: %v", err)
	}

	start := time.Now()
	repeat := open()
	defer repeat.Body.Close()
	if _, err := io.ReadFull(repeat.Body, make([]byte, 4096)); err != nil {
		t.Fatalf("the repeat ask's stream: %v", err)
	}
	for syscall.Kill(pid, 0) == nil {
		if time.Since(start) > transcodeTakeoverGrace+2*time.Second {
			t.Fatal("the taken-over stream's ffmpeg was never reaped while its client held on")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(e.api.transcodeSem) != 1 {
		t.Fatalf("%d slots taken, want the repeat ask's 1", len(e.api.transcodeSem))
	}
}
