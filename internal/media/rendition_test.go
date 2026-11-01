package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

func TestRenditionArgs(t *testing.T) {
	t.Parallel()
	got := renditionArgs(fdInput, "/cache/k.1.part")
	want := []string{"-nostdin", "-loglevel", "error", "-protocol_whitelist", "fd", "-i", "fd:",
		"-map", "0:a:0", "-vn", "-sn", "-dn", "-map_metadata", "-1", "-map_chapters", "-1",
		"-c:a", "aac", "-b:a", "48k", "-ac", "1", "-ar", "44100",
		"-fflags", "+bitexact", "-flags:a", "+bitexact",
		"-movflags", "+faststart", "-f", "mp4", "-y", "file:/cache/k.1.part"}
	if !slices.Equal(got, want) {
		t.Fatalf("renditionArgs(fd) = %q\nwant %q", got, want)
	}
	if got := renditionArgs(pipeInput, "x"); !slices.Contains(got, "pipe:0") || got[slices.Index(got, "-protocol_whitelist")+1] != "pipe" {
		t.Fatalf("renditionArgs(pipe) = %q, want the pipe whitelist and pipe:0", got)
	}
	if slices.Contains(got, "-xerror") {
		t.Fatal("a rendition must not stop on a decode error")
	}
}

// TestCacheKeys: the remux's keys are the ones it always had (its cached copies
// and ETags survive the upgrade); a rendition's key adds its kind and version,
// so the two never share a key and a new encode version is a new key.
func TestCacheKeys(t *testing.T) {
	t.Parallel()
	src := filepath.Join(t.TempDir(), "book.mp3")
	if err := os.WriteFile(src, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	remux, err := NewRemuxer(filepath.Join(t.TempDir(), "remux"), nil)
	if err != nil {
		t.Fatal(err)
	}
	watch, err := NewRenditionCache(filepath.Join(t.TempDir(), "watch"), nil)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(src + "\x00" + strconv.FormatInt(info.Size(), 10) + "\x00" +
		strconv.FormatInt(info.ModTime().UnixNano(), 10)))
	old := hex.EncodeToString(sum[:])[:32]
	if got := remux.key(src, info); got != old {
		t.Fatalf("remux key = %s, want the 2.5 key %s", got, old)
	}
	if got := remux.spec.etag(old, 99); got != `"remux-`+old+`"` {
		t.Fatalf("remux etag = %s", got)
	}
	sum = sha256.Sum256([]byte(src + "\x00" + strconv.FormatInt(info.Size(), 10) + "\x00" +
		strconv.FormatInt(info.ModTime().UnixNano(), 10) + "\x00watch-v1"))
	if got, want := watch.key(src, info), hex.EncodeToString(sum[:])[:32]; got != want || got == old {
		t.Fatalf("watch key = %s, want %s (not the remux's)", got, want)
	}
	if got := watch.spec.etag("k", 123); got != `"rw1-k-123"` {
		t.Fatalf("watch etag = %s", got)
	}
	if watch.cachePath("k") != filepath.Join(watch.dir, "k.m4a") {
		t.Fatalf("watch copy = %s, want k.m4a", watch.cachePath("k"))
	}
}

// renditionProbe is the parts of ffprobe's report on a rendition the tests read.
type renditionProbe struct {
	Streams []struct {
		CodecName  string `json:"codec_name"`
		Profile    string `json:"profile"`
		Channels   int    `json:"channels"`
		SampleRate string `json:"sample_rate"`
		BitRate    string `json:"bit_rate"`
	} `json:"streams"`
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
	} `json:"format"`
}

func probeRendition(t *testing.T, ffprobe, path string) (renditionProbe, float64) {
	t.Helper()
	out, err := exec.Command(ffprobe, "-v", "error", "-print_format", "json", "-show_format", "-show_streams", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var p renditionProbe
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	d, err := strconv.ParseFloat(p.Format.Duration, 64)
	if err != nil {
		t.Fatalf("duration %q: %v", p.Format.Duration, err)
	}
	return p, d
}

// writeAudio writes seconds of a stereo tone with cover-like metadata at path
// in the format its extension names, or skips.
func writeAudio(t *testing.T, path string, seconds int, enc ...string) {
	t.Helper()
	needFFmpeg(t)
	args := append([]string{"-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", "sine=f=440:r=22050",
		"-t", strconv.Itoa(seconds), "-ac", "2", "-metadata", "title=Some Title"}, enc...)
	if out, err := exec.Command("ffmpeg", append(args, "-y", path)...).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg could not write %s: %v\n%s", path, err, out)
	}
}

// TestRenditionEncodes: an MP3 and an M4B become AAC-LC, mono, 44.1 kHz,
// about 48 kbps, in MP4 with its index first, the source's duration, with no
// metadata; the copy is served with Range and its strong ETag, and an If-Range
// with that ETag resumes it while an old one gets the whole copy.
func TestRenditionEncodes(t *testing.T) {
	t.Parallel()
	ffprobe := needFFmpeg(t)
	dir := t.TempDir()
	srcs := map[string][]string{
		"part.mp3": {"-c:a", "libmp3lame", "-b:a", "64k"},
		"book.m4b": {"-c:a", "aac", "-b:a", "64k"},
	}
	m, err := NewRenditionCache(filepath.Join(t.TempDir(), "renditions", "watch"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, enc := range srcs {
		src := filepath.Join(dir, name)
		writeAudio(t, src, 20, enc...)
		_, want := probeRendition(t, ffprobe, src)
		f, etag, err := m.Open(ctx, ctx, "ffmpeg", src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		info, _ := f.Stat()
		if !strings.HasPrefix(etag, `"rw1-`) || !strings.HasSuffix(etag, "-"+strconv.FormatInt(info.Size(), 10)+`"`) {
			t.Fatalf("%s: etag %s, want rw1-<key>-<size>", name, etag)
		}
		p, got := probeRendition(t, ffprobe, f.Name())
		s := p.Streams
		if len(s) != 1 || s[0].CodecName != "aac" || s[0].Profile != "LC" || s[0].Channels != 1 || s[0].SampleRate != "44100" {
			t.Fatalf("%s: rendition streams %+v, want one AAC-LC mono 44.1 kHz", name, s)
		}
		if br, _ := strconv.Atoi(s[0].BitRate); br < 40000 || br > 56000 {
			t.Fatalf("%s: bit rate %s, want about 48k", name, s[0].BitRate)
		}
		if got < want-0.1 || got > want+0.1 {
			t.Fatalf("%s: rendition lasts %v, source %v", name, got, want)
		}
		whole, _ := io.ReadAll(f)
		if moov, mdat := strings.Index(string(whole), "moov"), strings.Index(string(whole), "mdat"); string(whole[4:8]) != "ftyp" ||
			moov < 0 || mdat < 0 || moov > mdat {
			t.Fatalf("%s: moov at %d, mdat at %d; want an MP4 with its index first", name, moov, mdat)
		}
		if out, _ := exec.Command(ffprobe, "-v", "error", "-show_entries", "format_tags", "-of", "json", f.Name()).Output(); strings.Contains(string(out), "Some Title") {
			t.Fatalf("%s: rendition keeps the source's tags: %s", name, out)
		}

		serve := func(h http.Header) *httptest.ResponseRecorder {
			r := httptest.NewRequest("GET", "/x", nil)
			r.Header = h
			w := httptest.NewRecorder()
			w.Header().Set("ETag", etag)
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			ServeOpenedAs(w, r, f, "part.m4a", info.ModTime(), true)
			return w
		}
		if w := serve(http.Header{"Range": {"bytes=1000-"}}); w.Code != http.StatusPartialContent ||
			w.Header().Get("Content-Type") != "audio/mp4" || w.Body.Len() != int(info.Size())-1000 {
			t.Fatalf("%s: Range = %d %q %d bytes", name, w.Code, w.Header().Get("Content-Type"), w.Body.Len())
		}
		if w := serve(http.Header{"Range": {"bytes=1000-"}, "If-Range": {etag}}); w.Code != http.StatusPartialContent {
			t.Fatalf("%s: If-Range with the ETag = %d, want 206", name, w.Code)
		}
		if w := serve(http.Header{"Range": {"bytes=1000-"}, "If-Range": {`"rw1-other-1"`}}); w.Code != http.StatusOK ||
			w.Body.Len() != int(info.Size()) {
			t.Fatalf("%s: If-Range with another ETag = %d %d bytes, want the whole copy", name, w.Code, w.Body.Len())
		}
		f.Close()
	}
}

// fakeRenditions is a rendition cache over a fresh dir whose ffmpeg run is run
// (given the copy's path), held until gate closes when gate is set (or the test
// ends, before the cache is waited for, so a failed test never hangs). Its
// copies are not MP4s, so the spec's check is off.
func fakeRenditions(t *testing.T, gate chan struct{}, run func(out string) error) (*FileCache, *atomic.Int32) {
	t.Helper()
	m, err := NewRenditionCache(filepath.Join(t.TempDir(), "watch"), nil)
	if err != nil {
		t.Fatal(err)
	}
	m.spec.check = nil
	var runs atomic.Int32
	ended := make(chan struct{})
	m.run = func(ctx context.Context, _ string, args []string, _ *os.File) error {
		runs.Add(1)
		if gate != nil {
			select {
			case <-gate:
			case <-ended:
				return context.Canceled
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return run(strings.TrimPrefix(args[len(args)-1], "file:"))
	}
	t.Cleanup(m.Wait)
	t.Cleanup(func() { close(ended) }) // runs first
	return m, &runs
}

func srcFile(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("audio "+name), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRenditionQueueAndStatus: Status starts nothing; Queue starts a missing
// file's encode and answers pending, then ready with its size; a refused file
// is failed, remembered, and not queued again; a run that failed for another
// reason leaves the file missing for the next Queue.
func TestRenditionQueueAndStatus(t *testing.T) {
	t.Parallel()
	gate := make(chan struct{})
	m, runs := fakeRenditions(t, gate, func(out string) error { return os.WriteFile(out, []byte("m4a copy"), 0o600) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := srcFile(t, "a.mp3")
	if st, _ := m.Status(src); st != CacheMissing || runs.Load() != 0 {
		t.Fatalf("status = %s after %d runs, want missing and none", st, runs.Load())
	}
	if st, _ := m.Queue(ctx, "ffmpeg", src); st != CachePending {
		t.Fatalf("queue = %s, want pending", st)
	}
	if st, _ := m.Status(src); st != CachePending {
		t.Fatalf("status while encoding = %s, want pending", st)
	}
	if st, _ := m.Queue(ctx, "ffmpeg", src); st != CachePending {
		t.Fatalf("queue again = %s, want pending (one flight)", st)
	}
	close(gate)
	m.Wait()
	if st, n := m.Status(src); st != CacheReady || n != int64(len("m4a copy")) {
		t.Fatalf("status after = %s %d, want ready with its size", st, n)
	}
	if st, n := m.Queue(ctx, "ffmpeg", src); st != CacheReady || n != int64(len("m4a copy")) || runs.Load() != 1 {
		t.Fatalf("queue of a ready file = %s %d after %d runs, want ready and one run", st, n, runs.Load())
	}
	if st, _ := m.Status(filepath.Join(t.TempDir(), "gone.mp3")); st != CacheFailed {
		t.Fatalf("status of no file = %s, want failed", st)
	}

	// A refusal: an exit status before anything was written.
	status := exec.Command(os.Args[0], "-test.run=^$", "-not-a-flag").Run()
	refusing, refusedRuns := fakeRenditions(t, nil, func(string) error { return status })
	bad := srcFile(t, "bad.mp3")
	refusing.Queue(ctx, "ffmpeg", bad)
	refusing.Wait()
	if st, _ := refusing.Status(bad); st != CacheFailed {
		t.Fatalf("status of a refused file = %s, want failed", st)
	}
	if _, _, err := refusing.Open(ctx, ctx, "ffmpeg", bad); !errors.Is(err, ErrRefused) {
		t.Fatalf("open of a refused file = %v, want ErrRefused", err)
	}
	if st, _ := refusing.Queue(ctx, "ffmpeg", bad); st != CacheFailed || refusedRuns.Load() != 1 {
		t.Fatalf("queue of a refused file = %s after %d runs, want failed and no new run", st, refusedRuns.Load())
	}

	// A failure partway (not the file's): missing again, tried again.
	partway, partRuns := fakeRenditions(t, nil, func(out string) error {
		_ = os.WriteFile(out, []byte("half"), 0o600)
		return status
	})
	flaky := srcFile(t, "flaky.mp3")
	partway.Queue(ctx, "ffmpeg", flaky)
	partway.Wait()
	if st, _ := partway.Status(flaky); st != CacheMissing {
		t.Fatalf("status after a failure partway = %s, want missing", st)
	}
	partway.Queue(ctx, "ffmpeg", flaky)
	partway.Wait()
	if n := partRuns.Load(); n != 2 {
		t.Fatalf("%d runs, want the failure tried again", n)
	}
}

// TestRenditionQueueBounded: past renditionQueueMax files in flight neither a
// Queue nor a stream's Open starts a run: Queue answers missing, Open
// ErrUnavailable (abandoned stream requests can't pile up encodes).
func TestRenditionQueueBounded(t *testing.T) {
	t.Parallel()
	gate := make(chan struct{})
	m, _ := fakeRenditions(t, gate, func(out string) error { return os.WriteFile(out, []byte("x"), 0o600) })
	defer close(gate)
	m.spec.queueMax = 2
	ctx := context.Background()
	var states []CacheState
	for _, name := range []string{"1.mp3", "2.mp3", "3.mp3"} {
		st, _ := m.Queue(ctx, "ffmpeg", srcFile(t, name))
		states = append(states, st)
	}
	if want := []CacheState{CachePending, CachePending, CacheMissing}; !slices.Equal(states, want) {
		t.Fatalf("queued %v, want %v", states, want)
	}
	if _, _, err := m.Open(ctx, ctx, "ffmpeg", srcFile(t, "4.mp3")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("open past the bound = %v, want ErrUnavailable", err)
	}
}

// TestRenditionNoAudioRefused: an M4B whose index sits at its end, read
// through pipe:0 (an ffmpeg without the fd protocol), makes ffmpeg exit 0 with
// an MP4 holding no track. That copy is never cached: the file is refused, and
// remembered, as an exit status would be.
func TestRenditionNoAudioRefused(t *testing.T) {
	t.Parallel()
	src := nonFaststartM4A(t)
	ffmpeg := pipeOnlyFFmpeg(t)
	dir := filepath.Join(t.TempDir(), "watch")
	m, err := NewRenditionCache(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Wait)
	ctx := context.Background()
	if _, _, err := m.Open(ctx, ctx, ffmpeg, src); !errors.Is(err, ErrRefused) || errors.Is(err, ErrNoSource) {
		t.Fatalf("open = %v, want ffmpeg's refusal", err)
	}
	if st, _ := m.Status(src); st != CacheFailed {
		t.Fatalf("status = %s, want failed (remembered)", st)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("cache holds %d files, want none", len(entries))
	}
}

// pipeOnlyFFmpeg is the installed ffmpeg under a path of its own whose fd
// probe answered no, so inputFor gives it pipe:0, as for an ffmpeg older than
// 6.1, without touching the answer other tests read for "ffmpeg".
func pipeOnlyFFmpeg(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available")
	}
	link := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("no symlink: %v", err)
	}
	p := &fdProbe{ran: true}
	p.once.Do(func() {})
	fdSupport.Store(link, p)
	t.Cleanup(func() { fdSupport.Delete(link) })
	return link
}

// TestRenditionOpenWaitsThenPending: a request that stops waiting for a cold
// encode gets its context's error while the encode goes on under the run
// context, and the next request is a hit.
func TestRenditionOpenWaitsThenPending(t *testing.T) {
	t.Parallel()
	gate := make(chan struct{})
	m, runs := fakeRenditions(t, gate, func(out string) error { return os.WriteFile(out, []byte("m4a"), 0o600) })
	src := srcFile(t, "a.mp3")
	wait, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := m.Open(wait, context.Background(), "ffmpeg", src); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cold open = %v, want the wait's deadline", err)
	}
	close(gate)
	m.Wait()
	ctx := context.Background()
	f, _, err := m.Open(ctx, ctx, "ffmpeg", src)
	if err != nil || runs.Load() != 1 {
		t.Fatalf("open after = %v after %d runs, want a hit", err, runs.Load())
	}
	f.Close()
}

// TestCachesShareDiskGuard: caches on one disk share one reservation
// (ShareDiskGuard), so a rendition still writing counts against a remux.
func TestCachesShareDiskGuard(t *testing.T) {
	t.Parallel()
	src := filepath.Join(t.TempDir(), "book.mka")
	if err := os.WriteFile(src, []byte("not really matroska"), 0o644); err != nil {
		t.Fatal(err)
	}
	remux, runs := countingRemuxer(t, nil)
	watch, err := NewRenditionCache(filepath.Join(t.TempDir(), "watch"), nil)
	if err != nil {
		t.Fatal(err)
	}
	remux.ShareDiskGuard(watch)
	// Room for this remux alone, but a rendition still writing has taken it.
	remux.freeSpace = func(string) (uint64, bool) { return 2 * cacheMinFree, true }
	watch.reserved.Store(cacheMinFree)
	ctx := context.Background()
	if _, _, err := remux.Open(ctx, ctx, "ffmpeg", src); !errors.Is(err, ErrUnavailable) || runs.Load() != 0 {
		t.Fatalf("open beside a rendition writing = %v after %d runs, want ErrUnavailable and no run", err, runs.Load())
	}
}

// TestStderrLogBounded: a run's stderr keeps its start and its end, whatever
// ffmpeg writes between them.
func TestStderrLogBounded(t *testing.T) {
	t.Parallel()
	var l stderrLog
	if _, err := l.Write([]byte("first line\n")); err != nil {
		t.Fatal(err)
	}
	if got := l.String(); got != "first line\n" {
		t.Fatalf("short log = %q", got)
	}
	for i := range 20000 {
		_, _ = l.Write([]byte("[mp3float @ 0x1] Header missing " + strconv.Itoa(i) + "\n"))
	}
	_, _ = l.Write([]byte("last line\n"))
	got := l.String()
	if len(got) > 2*stderrKeep+100 || !strings.HasPrefix(got, "first line\n") || !strings.HasSuffix(got, "last line\n") ||
		!strings.Contains(got, "left out") {
		t.Fatalf("long log = %d bytes, %q...%q", len(got), got[:40], got[len(got)-40:])
	}
}

// TestCacheEvictsOwnFilesOnly: each cache's sweep counts and removes its own
// copies (by extension), never another cache's files beside them.
func TestCacheEvictsOwnFilesOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)
	for i, name := range []string{"a.mp3", "b.m4a", "c.m4a", "d.mp3"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, make([]byte, 100), 0o600); err != nil {
			t.Fatal(err)
		}
		used := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, used, used); err != nil {
			t.Fatal(err)
		}
	}
	m, err := NewRenditionCache(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.spec.maxBytes = 100
	m.evict("")
	var left []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if want := []string{"a.mp3", "c.m4a", "d.mp3"}; !slices.Equal(left, want) {
		t.Fatalf("after the rendition sweep: %q, want %q", left, want)
	}
}

// TestRenditionShortNotCached: an encode that stops early and still exits 0
// (a read error partway, with no -xerror) leaves a copy shorter than its
// source, which is not cached and not remembered: the request fails as one to
// retry, the file stays missing, and the next request encodes it in full.
func TestRenditionShortNotCached(t *testing.T) {
	t.Parallel()
	needFFmpeg(t)
	src := filepath.Join(t.TempDir(), "part.mp3")
	writeAudio(t, src, 20, "-c:a", "libmp3lame", "-b:a", "64k")
	sf, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := sf.Stat()
	want, ok := metadata.ExactDuration(context.Background(), sf, info.Size(), "mp3")
	sf.Close()
	if !ok || want < 19 || want > 21 {
		t.Fatalf("source's exact duration = %v %v, want about 20 s", want, ok)
	}
	dir := filepath.Join(t.TempDir(), "watch")
	m, err := NewRenditionCache(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Wait)
	var cut atomic.Bool
	cut.Store(true)
	run := m.run
	m.run = func(ctx context.Context, ffmpegPath string, args []string, stdin *os.File) error {
		if cut.Load() { // the encode ends 15 s early and exits 0
			args = append(append(slices.Clone(args[:len(args)-1]), "-t", "5"), args[len(args)-1])
		}
		return run(ctx, ffmpegPath, args, stdin)
	}
	ctx := context.Background()
	if _, _, err := m.Open(ctx, ctx, "ffmpeg", src); !errors.Is(err, errShortCopy) || errors.Is(err, ErrRefused) {
		t.Fatalf("open = %v, want a short copy, not a refusal", err)
	}
	if st, _ := m.Status(src); st != CacheMissing {
		t.Fatalf("status = %s, want missing (not remembered)", st)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("cache holds %d files, want none", len(entries))
	}
	cut.Store(false)
	f, _, err := m.Open(ctx, ctx, "ffmpeg", src)
	if err != nil {
		t.Fatalf("open again = %v, want the full copy", err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	if got, ok := metadata.ExactDuration(ctx, f, fi.Size(), "m4a"); !ok || got < want-renditionShortMax {
		t.Fatalf("copy lasts %v %v, source %v", got, ok, want)
	}
}
