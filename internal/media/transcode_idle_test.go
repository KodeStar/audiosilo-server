package media

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// endless is a source that never runs out.
type endless struct{}

func (endless) Read(p []byte) (int, error) { return len(p), nil }

// idleServer serves copyIdle(src, idle) and reports how the copy ended.
func idleServer(t *testing.T, src func() io.Reader, idle time.Duration) (*httptest.Server, <-chan error) {
	t.Helper()
	done := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTranscodeHeader(w)
		done <- copyIdle(w, src(), idle)
	}))
	t.Cleanup(srv.Close)
	return srv, done
}

// TestCopyIdleEndsUnreadStream: a client that stops reading (a paused <audio>
// holding its transcode) has its stream ended once a write has waited idle,
// freeing the transcode's slot, rather than holding it while the page is open.
func TestCopyIdleEndsUnreadStream(t *testing.T) {
	t.Parallel()
	srv, done := idleServer(t, func() io.Reader { return endless{} }, 200*time.Millisecond)
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// Nothing read: the socket's buffers fill, then a write waits out its deadline.
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("copyIdle = %v, want the write deadline", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("an unread stream was never ended")
	}
}

// TestCopyIdleKeepsReadStream: a client that keeps reading, however slowly,
// is never cut off, however long its stream runs past idle.
func TestCopyIdleKeepsReadStream(t *testing.T) {
	t.Parallel()
	// A steady reader's writes still wait on TCP's window updates now and then
	// (some 400 ms seen on loopback under -race): idle leaves room for that.
	const size = 6 << 20
	const idle = time.Second
	srv, done := idleServer(t, func() io.Reader { return io.LimitReader(endless{}, size) }, idle)
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	start := time.Now()
	buf := make([]byte, 64<<10)
	var got int
	for {
		n, err := io.ReadFull(resp.Body, buf)
		got += n
		if err != nil {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if got != size {
		t.Fatalf("read %d bytes, want all %d", got, size)
	}
	if took := time.Since(start); took < 2*idle {
		t.Fatalf("the read took %v: too fast to outlast the idle time", took)
	}
	if err := <-done; err != nil {
		t.Fatalf("copyIdle = %v, want the whole stream", err)
	}
}

// TestTranscodeIdleEndsFFmpeg: a transcode nobody reads ends, its ffmpeg with
// it (blocked writing to its pipe, it would otherwise never exit), so the
// handler returns and lets go of its slot.
func TestTranscodeIdleEndsFFmpeg(t *testing.T) {
	t.Parallel()
	needFFmpeg(t)
	// Half an hour of silence: its MP3 overfills the socket's buffers.
	src := filepath.Join(t.TempDir(), "long.wav")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-loglevel", "error", "-f", "lavfi",
		"-i", "anullsrc=r=8000:cl=mono:d=1800", "-c:a", "pcm_u8", "-y", src).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg could not write a WAV: %v\n%s", err, out)
	}
	returned := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(returned)
		transcodeInput(w, r, inputSpec{url: src}, "ffmpeg", 0, 300*time.Millisecond, nil)
	}))
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	select {
	case <-returned:
	case <-time.After(30 * time.Second):
		t.Fatal("an unread transcode held its handler (and ffmpeg) on")
	}
}

// TestCopyIdleNoneKeepsUnreadStream: with no idle time (every client's but
// the chapter editor's: a web player's track loaded and never played, a Cast
// receiver's paused stream) an unread stream is never ended for it, only by
// the client going away.
func TestCopyIdleNoneKeepsUnreadStream(t *testing.T) {
	t.Parallel()
	srv, done := idleServer(t, func() io.Reader { return endless{} }, 0)
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("copyIdle = %v while the client is there, want it still copying", err)
	case <-time.After(time.Second):
	}
	resp.Body.Close()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the stream outlived its client")
	}
}

// fakeFFmpeg is a stand-in ffmpeg running script (sh), whatever it is asked.
func fakeFFmpeg(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg")
	// Written under ForkLock: a parallel test's fork must not inherit its open
	// write descriptor, or exec fails with ETXTBSY (golang/go#22315).
	syscall.ForkLock.RLock()
	err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// cancelOnWrite ends its request's context at its first write: a later ask
// taking the transcode's slot over while it streams.
type cancelOnWrite struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (c cancelOnWrite) Write(p []byte) (int, error) {
	c.cancel()
	return c.ResponseRecorder.Write(p)
}

// TestTranscodeReportsItsEnd: a transcode whose ffmpeg output came to its end
// with the request live ran to its end, ended after that or not; one whose
// request ended while it streamed (ffmpeg killed, which ends its output too)
// did not, nor did a HEAD.
func TestTranscodeReportsItsEnd(t *testing.T) {
	t.Parallel()
	src := filepath.Join(t.TempDir(), "book.m4b")
	if err := os.WriteFile(src, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	short := fakeFFmpeg(t, "printf 'mp3 bytes'")
	endless := fakeFFmpeg(t, "exec cat /dev/zero")

	rec := httptest.NewRecorder()
	if ended, err := Transcode(rec, httptest.NewRequest(http.MethodGet, "/", nil), src, short, 0, time.Minute, 0, nil); err != nil || !ended {
		t.Fatalf("a whole stream: ended %v, %v, want true", ended, err)
	}
	if rec.Body.String() != "mp3 bytes" {
		t.Fatalf("body = %q, want ffmpeg's output", rec.Body.String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cut := cancelOnWrite{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
	done := make(chan bool, 1)
	go func() {
		ended, _ := Transcode(cut, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx), src, endless, 0, time.Minute, 0, nil)
		done <- ended
	}()
	select {
	case ended := <-done:
		if ended {
			t.Fatal("a stream cut short by its request's end said it ran to its end")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a stream whose request ended went on")
	}

	if ended, _ := Transcode(httptest.NewRecorder(), httptest.NewRequest(http.MethodHead, "/", nil), src, short, 0, time.Minute, 0, nil); ended {
		t.Fatal("a HEAD said it streamed to the end")
	}
}
