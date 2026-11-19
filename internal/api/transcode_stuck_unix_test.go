//go:build !windows

package api

import (
	"context"
	"io"
	"net"
	"net/http"
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

// Not built for Windows: the test asks whether the stand-in ffmpeg (a shell
// script) still runs with syscall.Kill, which Windows has not.

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
	ffmpeg, pids := standInFFmpeg(t)
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
