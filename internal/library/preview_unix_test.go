//go:build unix

package library

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A file under the root that can't be opened, as on a share mounted below it
// that stopped answering (here a FIFO with no writer: its open waits for one,
// uninterruptibly), holds the preview only to its time and previewGrace:
// ErrDirsTimedOut then, never the request's time-out with its goroutine leaked
// for good. The preview keeps its mount's slot until the open returns, so with
// previewsPerMount of them hung another on that mount times out waiting for
// one, and a preview on another mount still runs.
func TestPreviewHungFileIsBounded(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Author", "Book"), 0o755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "Author", "Book", "01.mp3")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("no FIFO here: %v", err)
	}
	t.Cleanup(func() { unblockFIFO(t, fifo, previewsPerMount) })
	other := previewFixture(t)
	s := NewScanner(nil, "", slog.Default())
	s.previews.Mount = func(p string) string {
		if p == root {
			return "hung"
		}
		return "other"
	}
	lim := PreviewLimits{MaxTime: 100 * time.Millisecond}
	for range previewsPerMount {
		start := time.Now()
		if _, err := s.Preview(t.Context(), root, nil, lim); !errors.Is(err, ErrDirsTimedOut) {
			t.Fatalf("a preview whose sample read hangs: %v, want ErrDirsTimedOut", err)
		}
		if took := time.Since(start); took > lim.MaxTime+previewGrace+5*time.Second {
			t.Errorf("the hung preview answered after %v", took)
		}
	}
	if _, err := s.Preview(t.Context(), root, nil, lim); !errors.Is(err, ErrDirsTimedOut) {
		t.Errorf("with the mount's previews hung: %v, want ErrDirsTimedOut", err)
	}
	p, err := s.Preview(t.Context(), other, nil, PreviewLimits{})
	if err != nil || p.Counts.AudioFiles == 0 {
		t.Errorf("a preview on another mount: %+v, %v, want it read", p, err)
	}
}

// unblockFIFO opens fifo for writing, n times, so each open waiting for a
// writer returns; it gives up after a few seconds (no reader is left then).
func unblockFIFO(t *testing.T, fifo string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for opened := 0; opened < n && time.Now().Before(deadline); {
		f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		_ = f.Close()
		opened++
	}
}
