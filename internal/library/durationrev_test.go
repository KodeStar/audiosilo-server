package library

import (
	"bytes"
	"encoding/binary"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// headerlessVBR is n MPEG-1 layer III frames at 44.1 kHz whose bitrate varies,
// with no Xing or VBRI header (as lame -t writes a VBR file), and their length
// in seconds.
func headerlessVBR(n int) ([]byte, float64) {
	var out bytes.Buffer
	for i := range n {
		bi := []uint32{9, 11, 10}[i%3]                         // 128, 192, 160 kbit/s
		size := 144 * []int{128, 192, 160}[i%3] * 1000 / 44100 // no padding
		f := make([]byte, size)
		binary.BigEndian.PutUint32(f, 0xFFFB0000|bi<<12)
		out.Write(f)
	}
	return out.Bytes(), float64(n) * 1152 / 44100
}

// A book holding an MP3 that an older server indexed (duration_rev 0, its VBR
// file's length a bitrate's estimate) is indexed once more by the next scan,
// its files unchanged, so the frames are counted (SRV-16); the scan after
// leaves it be.
func TestScanRereadsLengthsOfAnOlderRevision(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	data, secs := headerlessVBR(2000)
	path := filepath.Join(root, "Author", "Book.mp3")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	duration := func() float64 {
		t.Helper()
		var d float64
		if err := db.QueryRowContext(ctx, `SELECT duration FROM books`).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	if res, err := s.Scan(ctx, *lib); err != nil || res.Added != 1 {
		t.Fatalf("first scan = %+v, %v", res, err)
	}
	if d := duration(); d < secs-0.001 || d > secs+0.001 {
		t.Fatalf("duration %v, want the frames counted, %v", d, secs)
	}

	// As an older server left it: the estimate, and no revision.
	if _, err := db.ExecContext(ctx, `UPDATE books SET duration = 46.4, duration_rev = 0`); err != nil {
		t.Fatal(err)
	}
	if res, err := s.Scan(ctx, *lib); err != nil || res.Updated != 1 {
		t.Fatalf("rescan = %+v, %v; want the book read again", res, err)
	}
	if d := duration(); d < secs-0.001 || d > secs+0.001 {
		t.Errorf("after the rescan: duration %v, want %v", d, secs)
	}
	if res, err := s.Scan(ctx, *lib); err != nil || res.Updated != 0 {
		t.Errorf("third scan = %+v, %v; want the book left be", res, err)
	}
}
