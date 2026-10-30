package metadata

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A file the in-process probe leaves to ffprobe is logged at debug with its
// path and the reason (server#269), a probe limit's included, and nothing
// above debug. Not parallel: it sets the package's logger.
func TestProbeFallbackLoggedAtDebug(t *testing.T) {
	var logs syncBuffer
	SetLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { SetLogger(nil) })
	dir := t.TempDir()
	wav := filepath.Join(dir, "a.wav")
	mp3 := filepath.Join(dir, "b.mp3")
	for _, p := range []string{wav, mp3} {
		if err := os.WriteFile(p, bytes.Repeat([]byte{0x55}, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if p, err := readFile(context.Background(), &Metadata{Format: "wav"}, wav, ff8, time.Second); err != nil || p != nil {
		t.Fatalf("wav: %v %v", p, err)
	}
	done, cancel := context.WithCancel(context.Background())
	cancel() // the caller's context ended: a probe limit
	if p, err := readFile(done, &Metadata{Format: "mp3"}, mp3, ff8, time.Second); err != nil || p != nil {
		t.Fatalf("mp3: %v %v", p, err)
	}
	out := logs.String()
	for _, want := range []string{
		`level=DEBUG msg="in-process probe: ffprobe reads this file" path=` + wav + ` reason="format wav"`,
		`path=` + mp3 + ` reason="limit: context canceled"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "level=INFO") || strings.Contains(out, "level=WARN") {
		t.Errorf("a fallback logged above debug:\n%s", out)
	}
}
