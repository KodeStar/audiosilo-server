package media

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// libraryWithOutside is a library root holding Book/a.m4a (inside bytes) and a
// folder out of the library holding a.m4a too (outside bytes), both resolved.
func libraryWithOutside(t *testing.T) (root, outside string) {
	t.Helper()
	var err error
	if root, err = filepath.EvalSymlinks(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if outside, err = filepath.EvalSymlinks(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Book"), 0o755); err != nil {
		t.Fatal(err)
	}
	for dir, body := range map[string]string{filepath.Join(root, "Book"): "inside", outside: "outside"} {
		if err := os.WriteFile(filepath.Join(dir, "a.m4a"), []byte(body+" bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, outside
}

// swapForLink replaces root's Book folder with a link to outside, as whoever
// can write to the library (or a share serving one) could between a path's
// check and its open; it skips where links can't be made.
func swapForLink(t *testing.T, root, outside string) {
	t.Helper()
	book := filepath.Join(root, "Book")
	if err := os.Rename(book, book+".aside"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, book); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
}

// echoFFmpeg is a stand-in ffmpeg that copies its input to its output: its
// descriptor 3 (the /dev/fd/3 fallback) when it has one, else its stdin (fd:,
// pipe:0); it answers the protocols probe listing none (answerNoProtocols), so
// Transcode hands it the descriptor as /dev/fd/3.
func echoFFmpeg(t *testing.T) string {
	t.Helper()
	return fakeFFmpeg(t, `if [ -e /dev/fd/3 ]; then exec cat <&3; fi; exec cat`)
}

// TestTranscodeOpensThroughTheRoot (security): a transcode's file is opened
// through the library's root and ffmpeg reads that descriptor, never the path,
// so a folder swapped for a link out of the library after the caller's check
// is a 404 and ffmpeg never reads the file it leads to (a stat of the path and
// ffmpeg's own open of it followed the link). Allowed: the file in place is
// transcoded from its own bytes, through a link that stays in the library
// too.
func TestTranscodeOpensThroughTheRoot(t *testing.T) {
	t.Parallel()
	ffmpeg := echoFFmpeg(t)
	root, outside := libraryWithOutside(t)
	abs := filepath.Join(root, "Book", "a.m4a")
	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		if _, err := Transcode(rec, httptest.NewRequest(http.MethodGet, "/", nil), root, abs, ffmpeg, 0, false, time.Minute, 0, nil); err != nil {
			t.Fatalf("Transcode = %v, want it answered here", err)
		}
		return rec
	}
	if rec := get(); rec.Code != http.StatusOK || rec.Body.String() != "inside bytes" {
		t.Fatalf("transcode in place = %d %q, want the library file's bytes", rec.Code, rec.Body)
	}
	if err := os.Symlink("Book", filepath.Join(root, "Again")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	rec := httptest.NewRecorder()
	if _, err := Transcode(rec, httptest.NewRequest(http.MethodGet, "/", nil), root, filepath.Join(root, "Again", "a.m4a"), ffmpeg, 0, false, time.Minute, 0, nil); err != nil ||
		rec.Code != http.StatusOK || rec.Body.String() != "inside bytes" {
		t.Fatalf("transcode through a link inside the library = %d %q (%v), want the file's bytes", rec.Code, rec.Body, err)
	}
	swapForLink(t, root, outside)
	if rec := get(); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "outside") {
		t.Fatalf("transcode after its folder became a link out of the library = %d %q, want a 404 and none of that file", rec.Code, rec.Body)
	}
}

// TestTranscodeSeeksTheDescriptor: ?t= stays an input seek on the descriptor
// ffmpeg is handed, through fd: and through the /dev/fd/3 fallback of an
// ffmpeg without it (pipe:0 could neither seek nor demux an m4a whose index
// sits at its end): from 8 s of 10 the output is the last 2 s.
func TestTranscodeSeeksTheDescriptor(t *testing.T) {
	t.Parallel()
	ffprobe := needFFmpeg(t)
	src := writeNonFaststartM4A(t)
	// Without fd: (an ffmpeg older than 6.1) the descriptor is /dev/fd/3 on
	// Unix, the re-checked path elsewhere: both seekable.
	ffmpegs := map[string]string{"no fd:": pipeOnlyFFmpeg(t)}
	if inputFor("ffmpeg") == fdInput {
		ffmpegs["fd:"] = "ffmpeg"
	}
	for name, ffmpeg := range ffmpegs {
		for _, tc := range []struct{ start, want float64 }{{0, 10}, {8, 2}} {
			rec := httptest.NewRecorder()
			if _, err := Transcode(rec, httptest.NewRequest(http.MethodGet, "/", nil), filepath.Dir(src), src, ffmpeg, tc.start, false, time.Minute, 0, nil); err != nil || rec.Code != http.StatusOK || rec.Body.Len() == 0 {
				t.Fatalf("%s from %v s: %d with %d bytes (%v)", name, tc.start, rec.Code, rec.Body.Len(), err)
			}
			out := filepath.Join(t.TempDir(), "out.mp3")
			if err := os.WriteFile(out, rec.Body.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			if d := ffprobeOf(t, ffprobe, out).duration(t); d < tc.want-0.5 || d > tc.want+0.5 {
				t.Errorf("%s: transcoded duration from %v s = %v, want about %v", name, tc.start, d, tc.want)
			}
		}
	}
}

// TestRecheckedInput: the fallback where ffmpeg can be handed no descriptor
// (Windows without fd:) is the file's resolved path, only while it names the
// file opened: the same file through a link gives the link's target, a path
// that leads elsewhere since the open none.
func TestRecheckedInput(t *testing.T) {
	t.Parallel()
	root, outside := libraryWithOutside(t)
	f, _, err := metadata.OpenRegularBelow(root, filepath.Join(root, "Book", "a.m4a"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	in, err := recheckedInput(f)
	if want := "file:" + filepath.Join(root, "Book", "a.m4a"); err != nil || in.url != want || in.proto != "file" || in.file != nil {
		t.Fatalf("recheckedInput in place = %+v, %v, want %s alone", in, err, want)
	}
	swapForLink(t, root, outside)
	if in, err := recheckedInput(f); !errors.Is(err, errNotOpened) {
		t.Fatalf("recheckedInput once the path leads out of the library = %+v, %v, want errNotOpened", in, err)
	}
}

// failOpens makes every open of a library file (openBelow) fail with errno,
// until the test ends. Not for a parallel test: it swaps the package's open.
func failOpens(t *testing.T, errno syscall.Errno) {
	t.Helper()
	was := openBelow
	t.Cleanup(func() { openBelow = was })
	openBelow = func(_ context.Context, _, path string, _ time.Duration) (*os.File, os.FileInfo, error) {
		return nil, nil, &fs.PathError{Op: "open", Path: path, Err: errno}
	}
}

// TestOpenFaultsAnswerByKind: a stream's or a transcode's open that fails as a
// share does that a retry may get past (EIO, ESTALE) is returned with nothing
// written, the caller's 503 share_not_answering, never the 404 of a file gone;
// a lasting refusal (a permission) and the file gone are the 404. Not
// parallel: it swaps openBelow.
func TestOpenFaultsAnswerByKind(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "a.mp3")
	for _, c := range []struct {
		errno     syscall.Errno
		transient bool
	}{{syscall.EIO, true}, {syscall.ESTALE, true}, {syscall.EACCES, false}, {syscall.ENOENT, false}} {
		failOpens(t, c.errno)
		serve := httptest.NewRecorder()
		serveErr := ServeFile(serve, httptest.NewRequest(http.MethodGet, "/", nil), root, abs, false, time.Minute)
		trans := httptest.NewRecorder()
		_, transErr := Transcode(trans, httptest.NewRequest(http.MethodGet, "/", nil), root, abs, "ffmpeg", 0, false, time.Minute, 0, nil)
		for what, got := range map[string]struct {
			err error
			rec *httptest.ResponseRecorder
		}{"ServeFile": {serveErr, serve}, "Transcode": {transErr, trans}} {
			switch {
			case c.transient && (!errors.Is(got.err, c.errno) || got.rec.Body.Len() != 0):
				t.Errorf("%s with its open failing %v = %v (%d written), want the fault returned with nothing written", what, c.errno, got.err, got.rec.Body.Len())
			case !c.transient && (got.err != nil || got.rec.Code != http.StatusNotFound):
				t.Errorf("%s with its open failing %v = %v, %d, want a 404", what, c.errno, got.err, got.rec.Code)
			}
		}
	}
}

// TestFileCacheSourceThroughTheRoot (security): a copy's source is looked at
// and opened through the library's root, so a folder swapped for a link out
// of the library between the look (Source) and the run's open is refused
// there, ffmpeg never run on what it leads to (a plain open of the path
// followed the link), and a source already behind such a link is no source.
// Allowed: in place the run reads the library file's own descriptor. Not
// parallel: it swaps statBelow.
func TestFileCacheSourceThroughTheRoot(t *testing.T) {
	root, outside := libraryWithOutside(t)
	abs := filepath.Join(root, "Book", "a.m4a")
	m, _ := fakeRenditions(t, nil, func(out string) error { return os.WriteFile(out, []byte("m4a"), 0o600) })
	var read atomic.Value
	run := m.run
	m.run = func(ctx context.Context, ffmpegPath string, args []string, stdin *os.File) error {
		b, _ := io.ReadAll(stdin)
		read.Store(string(b))
		return run(ctx, ffmpegPath, args, stdin)
	}
	ctx := context.Background()
	f, _, err := m.Open(ctx, ctx, "ffmpeg", root, abs)
	if err != nil || read.Load() != "inside bytes" {
		t.Fatalf("a copy in place = %v, its run read %q, want the library file's bytes", err, read.Load())
	}
	f.Close()

	// The look answers, then the folder becomes a link out of the library
	// before the run opens the source. The file out of it has the size and
	// mtime the look read, so the run's own check (unchanged) can't tell.
	if err := os.WriteFile(abs, []byte("inside bytes, changed"), 0o644); err != nil {
		t.Fatal(err) // a new key: the copy is made again
	}
	twin := filepath.Join(outside, "a.m4a")
	if err := os.WriteFile(twin, []byte("outside bytes changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(abs); err != nil {
		t.Fatal(err)
	} else if err := os.Chtimes(twin, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	was := statBelow
	t.Cleanup(func() { statBelow = was })
	var swap sync.Once
	statBelow = func(ctx context.Context, root, abs string, wait time.Duration) (fs.FileInfo, error) {
		info, err := was(ctx, root, abs, wait)
		swap.Do(func() { swapForLink(t, root, outside) })
		return info, err
	}
	read.Store("")
	if _, _, err := m.Open(ctx, ctx, "ffmpeg", root, abs); err == nil || read.Load() != "" {
		t.Fatalf("a copy whose folder became a link out of the library after its look = %v, its run read %q, want it refused unread", err, read.Load())
	}
	statBelow = was
	if _, err := Source(ctx, root, abs, time.Minute); !errors.Is(err, ErrNoSource) {
		t.Fatalf("Source through a link out of the library = %v, want ErrNoSource", err)
	}
	if st, _, _ := m.Status(root, abs); st != CacheFailed {
		t.Fatalf("Status through a link out of the library = %s, want failed", st)
	}
}

// TestSourceTransientFaultIsUnanswered: a copy's look at its source failing as
// a share does that a retry may get past is a look given up on (the stream's
// 503 share_not_answering), never the file gone (ErrNoSource); a lasting
// refusal is no source. Not parallel: it swaps statBelow.
func TestSourceTransientFaultIsUnanswered(t *testing.T) {
	was := statBelow
	t.Cleanup(func() { statBelow = was })
	for _, c := range []struct {
		errno     syscall.Errno
		transient bool
	}{{syscall.EIO, true}, {syscall.ESTALE, true}, {syscall.EACCES, false}} {
		statBelow = func(_ context.Context, _, abs string, _ time.Duration) (fs.FileInfo, error) {
			return nil, &fs.PathError{Op: "stat", Path: abs, Err: c.errno}
		}
		_, err := Source(context.Background(), "/lib", "/lib/a.mka", time.Minute)
		if gaveUp := metadata.GaveUp(err); gaveUp != c.transient || gaveUp == errors.Is(err, ErrNoSource) {
			t.Errorf("Source with its stat failing %v = %v, want given up on %v", c.errno, err, c.transient)
		}
	}
}
