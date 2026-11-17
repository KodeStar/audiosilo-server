//go:build unix

package metadata

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO named as audio is never waited for nor read (server#360): with no
// writer its open would wait for one, with a writer that never writes its
// reads would. Either way it is no regular file, at once: an OpenErr, its tags
// unread (a stored book it is a part of is kept), no ffprobe run, no
// fingerprint.
func TestFIFOIsNotRead(t *testing.T) {
	t.Parallel()
	for _, writer := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "book.mp3")
		if err := syscall.Mkfifo(path, 0o644); err != nil {
			t.Skipf("no FIFO here: %v", err)
		}
		if writer {
			w, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Skipf("can't hold the FIFO open: %v", err)
			}
			t.Cleanup(func() { _ = w.Close() })
		}
		start := time.Now()
		m, err := ExtractTimeout(context.Background(), path, "/no/such/ffprobe", time.Minute)
		if err != nil || !errors.Is(m.OpenErr, errNotRegular) || !m.TagsUnread() || m.ProbeErr != nil {
			t.Errorf("writer %v: FIFO read = %+v (err %v), want it not a regular file, no ffprobe", writer, m, err)
		}
		if fp := Fingerprint(context.Background(), path, time.Minute); fp != "" {
			t.Errorf("writer %v: FIFO fingerprint = %q, want none", writer, fp)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("writer %v: the FIFO took %v, want it turned away at once", writer, d)
		}
	}
}
