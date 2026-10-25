package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// needFFmpeg skips without ffmpeg and ffprobe and returns ffprobe's path.
func needFFmpeg(t *testing.T) string {
	t.Helper()
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil || !HasFFmpeg("ffmpeg") {
		t.Skip("ffmpeg/ffprobe not available")
	}
	return ffprobe
}

// writeMKA writes a 4-second Matroska audio file of codec ("mp3" or "aac")
// with two chapters (Opening 0-2 s, Closing 2-4 s) at path, or skips.
func writeMKA(t *testing.T, path, codec string) {
	t.Helper()
	needFFmpeg(t)
	meta := filepath.Join(t.TempDir(), "meta.txt")
	chapters := ";FFMETADATA1\ntitle=Matroska Book\n" +
		"[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=2000\ntitle=Opening\n" +
		"[CHAPTER]\nTIMEBASE=1/1000\nSTART=2000\nEND=4000\ntitle=Closing\n"
	if err := os.WriteFile(meta, []byte(chapters), 0o644); err != nil {
		t.Fatal(err)
	}
	enc := []string{"-c:a", "libmp3lame", "-q:a", "9"}
	if codec == "aac" {
		enc = []string{"-c:a", "aac", "-b:a", "32k"}
	}
	args := append([]string{"-nostdin", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=f=440:r=22050", "-i", meta, "-t", "4",
		"-map", "0:a", "-map_metadata", "1", "-map_chapters", "1"}, enc...)
	if out, err := exec.Command("ffmpeg", append(args, "-y", path)...).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg could not write an .mka: %v\n%s", err, out)
	}
}

// probe is the parts of ffprobe's report the tests read.
type probe struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
	} `json:"format"`
	Chapters []struct {
		Tags struct {
			Title string `json:"title"`
		} `json:"tags"`
	} `json:"chapters"`
}

func ffprobeOf(t *testing.T, ffprobe, path string) probe {
	t.Helper()
	out, err := exec.Command(ffprobe, "-v", "error", "-print_format", "json", "-show_format", "-show_chapters", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var p probe
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func (p probe) duration(t *testing.T) float64 {
	t.Helper()
	d, err := strconv.ParseFloat(p.Format.Duration, 64)
	if err != nil {
		t.Fatalf("duration %q: %v", p.Format.Duration, err)
	}
	return d
}

// countingRemuxer is a Remuxer over a fresh cache dir whose ffmpeg runs are
// counted; gate, when set, holds each run until it is closed.
func countingRemuxer(t *testing.T, gate chan struct{}) (*Remuxer, *atomic.Int32) {
	t.Helper()
	m, err := NewRemuxer(filepath.Join(t.TempDir(), "cache", "remux"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var runs atomic.Int32
	m.run = func(ctx context.Context, ffmpegPath string, args []string, stdin *os.File) error {
		runs.Add(1)
		if gate != nil {
			<-gate
		}
		return runFFmpeg(ctx, ffmpegPath, args, stdin)
	}
	return m, &runs
}

func TestRemuxArgs(t *testing.T) {
	t.Parallel()
	got := remuxArgs(fdInput, "/cache/k.1.part")
	want := []string{"-nostdin", "-loglevel", "error", "-xerror", "-protocol_whitelist", "fd",
		"-f", "matroska", "-i", "fd:", "-map", "0:a:0", "-c:a", "copy", "-map_chapters", "0",
		"-fflags", "+bitexact", "-id3v2_version", "3", "-f", "mp3", "-y", "file:/cache/k.1.part"}
	if !slices.Equal(got, want) {
		t.Fatalf("remuxArgs(fd) = %q\nwant %q", got, want)
	}
	if got := remuxArgs(pipeInput, "x"); !slices.Contains(got, "pipe:0") || got[slices.Index(got, "-protocol_whitelist")+1] != "pipe" {
		t.Fatalf("remuxArgs(pipe) = %q, want the pipe whitelist and pipe:0", got)
	}
}

func TestRemuxable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		format, codec string
		want          bool
	}{
		{"mka", "mp3", true}, {"MKA", "MP3", true},
		{"mka", "aac", false}, {"mka", "", false}, {"mp3", "mp3", false}, {"m4b", "mp3", false},
	} {
		if got := Remuxable(tc.format, tc.codec); got != tc.want {
			t.Errorf("Remuxable(%q, %q) = %v, want %v", tc.format, tc.codec, got, tc.want)
		}
	}
}

// TestRemuxerOpen: an MP3-in-MKA becomes a plain MP3 with the chapters and the
// duration; the second Open is a cache hit, and a changed mtime a new remux.
func TestRemuxerOpen(t *testing.T) {
	t.Parallel()
	ffprobe := needFFmpeg(t)
	src := filepath.Join(t.TempDir(), "book.mka")
	writeMKA(t, src, "mp3")
	m, runs := countingRemuxer(t, nil)
	ctx := context.Background()

	f, etag, err := m.Open(ctx, ctx, "ffmpeg", src)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"remux-` + strings.TrimSuffix(filepath.Base(f.Name()), ".mp3") + `"`; etag != want {
		t.Fatalf("etag = %s, want the strong %s", etag, want)
	}
	if ct := sniffAudioType(f); ct != "audio/mpeg" {
		t.Fatalf("remuxed file sniffs %q, want audio/mpeg", ct)
	}
	first, _ := io.ReadAll(f)
	f.Close()
	p := ffprobeOf(t, ffprobe, f.Name())
	if p.Format.FormatName != "mp3" || len(p.Chapters) != 2 || p.Chapters[0].Tags.Title != "Opening" || p.Chapters[1].Tags.Title != "Closing" {
		t.Fatalf("remuxed probe = %+v, want mp3 with Opening and Closing", p)
	}
	if d := p.duration(t); d < 3.9 || d > 4.1 {
		t.Fatalf("remuxed duration = %v, want about 4", d)
	}
	if fi, _ := os.Stat(m.dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("cache dir mode = %v, want 0700", fi.Mode().Perm())
	}

	f2, etag2, err := m.Open(ctx, ctx, "ffmpeg", src)
	if err != nil {
		t.Fatal(err)
	}
	f2.Close()
	if n := runs.Load(); n != 1 || f2.Name() != f.Name() || etag2 != etag {
		t.Fatalf("second open: %d runs, %s vs %s; want a hit", n, f2.Name(), f.Name())
	}

	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(src, later, later); err != nil {
		t.Fatal(err)
	}
	f3, etag3, err := m.Open(ctx, ctx, "ffmpeg", src)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := io.ReadAll(f3)
	f3.Close()
	if n := runs.Load(); n != 2 || f3.Name() == f.Name() || etag3 == etag {
		t.Fatalf("after touching the source: %d runs, same file %v; want a new remux", n, f3.Name() == f.Name())
	}
	// Bitexact: the same audio remuxes to the same bytes, so the ETag of a key
	// re-remuxed after an eviction still names its bytes.
	if !bytes.Equal(first, again) {
		t.Fatal("re-remux of the same audio differs")
	}
}

// TestRemuxerSingleFlight: requests for one cold key at once share one remux.
func TestRemuxerSingleFlight(t *testing.T) {
	t.Parallel()
	needFFmpeg(t)
	src := filepath.Join(t.TempDir(), "book.mka")
	writeMKA(t, src, "mp3")
	gate := make(chan struct{})
	m, runs := countingRemuxer(t, gate)
	ctx := context.Background()
	var wg sync.WaitGroup
	names := make([]string, 8)
	errs := make([]error, 8)
	for i := range names {
		wg.Go(func() {
			f, _, err := m.Open(ctx, ctx, "ffmpeg", src)
			errs[i] = err
			if err == nil {
				names[i] = f.Name()
				f.Close()
			}
		})
	}
	time.Sleep(100 * time.Millisecond) // let them all reach the flight
	close(gate)
	wg.Wait()
	for i, err := range errs {
		if err != nil || names[i] != names[0] {
			t.Fatalf("open %d = %q %v, want %q", i, names[i], err, names[0])
		}
	}
	if n := runs.Load(); n != 1 {
		t.Fatalf("%d remuxes, want 1", n)
	}
}

// TestRemuxerWaiterLeaves: a waiter whose request goes returns at once, and the
// remux it started still finishes and is cached.
func TestRemuxerWaiterLeaves(t *testing.T) {
	t.Parallel()
	needFFmpeg(t)
	src := filepath.Join(t.TempDir(), "book.mka")
	writeMKA(t, src, "mp3")
	gate := make(chan struct{})
	m, runs := countingRemuxer(t, gate)
	reqCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := m.Open(reqCtx, context.Background(), "ffmpeg", src)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter = %v, want context.Canceled", err)
	}
	close(gate)
	f, _, err := m.Open(context.Background(), context.Background(), "ffmpeg", src)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if n := runs.Load(); n != 1 {
		t.Fatalf("%d remuxes, want the first one to have finished and been reused", n)
	}
}

// TestRemuxerRefusesOtherCodecs: AAC in Matroska is not remuxable, and the
// refusal is remembered for the file (no second ffmpeg run) until it changes.
func TestRemuxerRefusesOtherCodecs(t *testing.T) {
	t.Parallel()
	needFFmpeg(t)
	src := filepath.Join(t.TempDir(), "aac.mka")
	writeMKA(t, src, "aac")
	m, runs := countingRemuxer(t, nil)
	ctx := context.Background()
	for range 2 {
		if _, _, err := m.Open(ctx, ctx, "ffmpeg", src); !errors.Is(err, ErrNotRemuxable) {
			t.Fatalf("open = %v, want ErrNotRemuxable", err)
		}
	}
	if n := runs.Load(); n != 1 {
		t.Fatalf("%d runs, want the refusal remembered", n)
	}
	if entries, _ := os.ReadDir(m.dir); len(entries) != 0 {
		t.Fatalf("cache holds %d files after a refusal, want none", len(entries))
	}
	later := time.Now().Add(time.Hour)
	_ = os.Chtimes(src, later, later)
	if _, _, err := m.Open(ctx, ctx, "ffmpeg", src); !errors.Is(err, ErrNotRemuxable) || runs.Load() != 2 {
		t.Fatalf("changed file: %v after %d runs, want a retry", err, runs.Load())
	}
}

// TestRemuxerDiskGuard: too little free space skips the remux.
func TestRemuxerDiskGuard(t *testing.T) {
	t.Parallel()
	src := filepath.Join(t.TempDir(), "book.mka")
	if err := os.WriteFile(src, []byte("not really matroska"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, runs := countingRemuxer(t, nil)
	m.freeSpace = func(string) (uint64, bool) { return remuxMinFree, true }
	ctx := context.Background()
	if _, _, err := m.Open(ctx, ctx, "ffmpeg", src); !errors.Is(err, ErrRemuxUnavailable) || runs.Load() != 0 {
		t.Fatalf("open = %v after %d runs, want ErrRemuxUnavailable and no run", err, runs.Load())
	}
	// No regular file at the path: not remuxable, so the stored file's answer
	// (a 404) is given.
	folder := filepath.Join(t.TempDir(), "folder.mka")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(t.TempDir(), "missing.mka"), folder} {
		if _, _, err := m.Open(ctx, ctx, "ffmpeg", p); !errors.Is(err, ErrNotRemuxable) || runs.Load() != 0 {
			t.Fatalf("open %s = %v after %d runs, want ErrNotRemuxable and no run", p, err, runs.Load())
		}
	}
}

// fakeRemuxer is a Remuxer over a fresh cache dir whose ffmpeg run is run (given
// the source and the copy's path), with a source file of a few bytes at src.
func fakeRemuxer(t *testing.T, run func(src, out string) error) (m *Remuxer, src string) {
	t.Helper()
	src = filepath.Join(t.TempDir(), "book.mka")
	if err := os.WriteFile(src, []byte("matroska bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := NewRemuxer(filepath.Join(t.TempDir(), "remux"), nil)
	if err != nil {
		t.Fatal(err)
	}
	m.run = func(_ context.Context, _ string, args []string, _ *os.File) error {
		return run(src, strings.TrimPrefix(args[len(args)-1], "file:"))
	}
	return m, src
}

// TestRemuxerFailurePartwayIsNotARefusal: a run that wrote some of the copy
// and then exited with an error (a full disk, a read error) is not the file's
// refusal: it isn't remembered, so the next request tries again.
func TestRemuxerFailurePartwayIsNotARefusal(t *testing.T) {
	t.Parallel()
	// A real exit status: this test binary run with a flag it doesn't know.
	status := exec.Command(os.Args[0], "-test.run=^$", "-not-a-flag").Run()
	if exit := (*exec.ExitError)(nil); !errors.As(status, &exit) || exit.ExitCode() <= 0 {
		t.Fatalf("want an exit status, got %v", status)
	}
	var runs atomic.Int32
	m, src := fakeRemuxer(t, func(_, out string) error {
		runs.Add(1)
		if err := os.WriteFile(out, []byte("ID3 partial"), 0o600); err != nil {
			return err
		}
		return status
	})
	ctx := context.Background()
	for range 2 {
		if _, _, err := m.Open(ctx, ctx, "ffmpeg", src); err == nil || errors.Is(err, ErrNotRemuxable) {
			t.Fatalf("open = %v, want a failure that is not a refusal", err)
		}
	}
	if n := runs.Load(); n != 2 {
		t.Fatalf("%d runs, want the failure not remembered", n)
	}
	if entries, _ := os.ReadDir(m.dir); len(entries) != 0 {
		t.Fatalf("cache holds %d files after failed runs, want none", len(entries))
	}
}

// TestRemuxerRecreatesItsDir: a cache folder removed while the server runs is
// made again by the next remux.
func TestRemuxerRecreatesItsDir(t *testing.T) {
	t.Parallel()
	m, src := fakeRemuxer(t, func(_, out string) error { return os.WriteFile(out, []byte("ID3 copy"), 0o600) })
	if err := os.RemoveAll(m.dir); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	f, _, err := m.Open(ctx, ctx, "ffmpeg", src)
	if err != nil {
		t.Fatalf("open after the cache folder went = %v", err)
	}
	f.Close()
	if fi, err := os.Stat(m.dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("cache folder = %v %v, want it made again 0700", fi, err)
	}
}

// TestRemuxerSourceChangedWhileRead: a source rewritten while ffmpeg read it
// is not cached under the key made from its old size and mtime.
func TestRemuxerSourceChangedWhileRead(t *testing.T) {
	t.Parallel()
	m, src := fakeRemuxer(t, func(src, out string) error {
		later := time.Now().Add(time.Hour)
		if err := os.Chtimes(src, later, later); err != nil {
			return err
		}
		return os.WriteFile(out, []byte("ID3 copy"), 0o600)
	})
	ctx := context.Background()
	if _, _, err := m.Open(ctx, ctx, "ffmpeg", src); !errors.Is(err, ErrRemuxUnavailable) {
		t.Fatalf("open = %v, want ErrRemuxUnavailable", err)
	}
	if entries, _ := os.ReadDir(m.dir); len(entries) != 0 {
		t.Fatalf("cache holds %d files, want none", len(entries))
	}
}

// TestRemuxerEvicts: the sweep removes the least recently used copies until the
// cache fits, never the one just written; NewRemuxer clears .part leftovers.
func TestRemuxerEvicts(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "remux")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour)
	for i, name := range []string{"a", "b", "c", "d"} {
		path := filepath.Join(dir, name+".mp3")
		if err := os.WriteFile(path, make([]byte, 100), 0o600); err != nil {
			t.Fatal(err)
		}
		used := base.Add(time.Duration(i) * time.Minute) // a oldest, d newest
		if err := os.Chtimes(path, used, used); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "e.123.part"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewRemuxer(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "e.123.part")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("leftover .part = %v, want removed", err)
	}
	m.maxBytes = 200
	m.evict("a") // a is the oldest but was just written
	var left []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if want := []string{"a.mp3", "d.mp3"}; !slices.Equal(left, want) {
		t.Fatalf("after the sweep: %q, want %q", left, want)
	}
}

// TestRemuxerWarmAll: the warm remuxes a book's files in the background, one
// at a time, skipping the cached and the refused, and a request after it is a
// hit.
func TestRemuxerWarmAll(t *testing.T) {
	t.Parallel()
	needFFmpeg(t)
	dir := t.TempDir()
	one, two, aac := filepath.Join(dir, "1.mka"), filepath.Join(dir, "2.mka"), filepath.Join(dir, "aac.mka")
	writeMKA(t, one, "mp3")
	writeMKA(t, two, "mp3")
	writeMKA(t, aac, "aac")
	m, runs := countingRemuxer(t, nil)
	var most, now atomic.Int32
	run := m.run
	m.run = func(ctx context.Context, ffmpegPath string, args []string, stdin *os.File) error {
		n := now.Add(1)
		defer now.Add(-1)
		for {
			if cur := most.Load(); n <= cur || most.CompareAndSwap(cur, n) {
				break
			}
		}
		return run(ctx, ffmpegPath, args, stdin)
	}
	ctx := context.Background()
	m.WarmAll(ctx, "ffmpeg", []string{one, aac, two})
	m.Wait()
	if n := runs.Load(); n != 3 || most.Load() != 1 {
		t.Fatalf("%d runs, at most %d at once; want 3, one at a time", n, most.Load())
	}
	m.WarmAll(ctx, "ffmpeg", []string{one, aac, two, filepath.Join(dir, "missing.mka")})
	m.Wait()
	if n := runs.Load(); n != 3 {
		t.Fatalf("second warm ran ffmpeg %d more times, want none", n-3)
	}
	f, _, err := m.Open(ctx, ctx, "ffmpeg", two)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if n := runs.Load(); n != 3 {
		t.Fatalf("open after the warm ran ffmpeg again (%d runs)", n)
	}
}

// TestRemuxerHeldWaiterTimesOut: a waiter that stops waiting (its bounded
// wait) leaves the held remux running, and it caches once let go.
func TestRemuxerHeldWaiterTimesOut(t *testing.T) {
	t.Parallel()
	needFFmpeg(t)
	src := filepath.Join(t.TempDir(), "book.mka")
	writeMKA(t, src, "mp3")
	m, runs := countingRemuxer(t, nil)
	gate := make(chan struct{})
	m.HoldRemuxesForTests(gate)
	wait, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := m.Open(wait, context.Background(), "ffmpeg", src); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded wait = %v, want DeadlineExceeded", err)
	}
	close(gate)
	m.Wait()
	ctx := context.Background()
	f, _, err := m.Open(ctx, ctx, "ffmpeg", src)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if n := runs.Load(); n != 1 {
		t.Fatalf("%d runs, want the held one only", n)
	}
}
