package media

import (
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	const size = 4 << 20
	srv, done := idleServer(t, func() io.Reader { return io.LimitReader(endless{}, size) }, 300*time.Millisecond)
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
	if took := time.Since(start); took < time.Second {
		t.Fatalf("the read took %v: too fast to outlast the idle time several times", took)
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
	if !HasFFmpeg("ffmpeg") {
		t.Skip("ffmpeg not available; transcoding requires it")
	}
	src := longWAV(t, 30*60)
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

// longWAV writes seconds of silence as an 8 kHz mono 8-bit WAV: long enough
// that its MP3 overfills the socket's buffers, quick to write.
func longWAV(t *testing.T, seconds int) string {
	t.Helper()
	const rate = 8000
	n := uint32(rate * seconds)
	path := filepath.Join(t.TempDir(), "long.wav")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	head := []any{
		[4]byte{'R', 'I', 'F', 'F'}, 36 + n, [4]byte{'W', 'A', 'V', 'E'},
		[4]byte{'f', 'm', 't', ' '}, uint32(16), uint16(1), uint16(1), uint32(rate), uint32(rate), uint16(1), uint16(8),
		[4]byte{'d', 'a', 't', 'a'}, n,
	}
	for _, v := range head {
		if err := binary.Write(f, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	silence := make([]byte, rate)
	for i := range silence {
		silence[i] = 128 // 8-bit PCM is unsigned
	}
	for range seconds {
		if _, err := f.Write(silence); err != nil {
			t.Fatal(err)
		}
	}
	return path
}
