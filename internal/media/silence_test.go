package media

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/testwait"
)

func TestParseSilences(t *testing.T) {
	t.Parallel()
	log := `[silencedetect @ 0x1] silence_end: 0.5 | silence_duration: 0.5
[silencedetect @ 0x1] silence_start: 2.25
[silencedetect @ 0x1] silence_end: 4.75 | silence_duration: 2.5
[silencedetect @ 0x1] silence_start: 7.5`
	var pauses silenceLog
	for line := range strings.SplitSeq(log, "\n") {
		pauses.add(line)
	}
	got := pauses.finish(8)
	want := []metadata.TimeRange{{Start: 0, End: 0.5}, {Start: 2.25, End: 4.75}, {Start: 7.5, End: 8}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pause %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestDetectSilences(t *testing.T) {
	t.Parallel()
	if !HasFFmpeg("ffmpeg") {
		t.Skip("ffmpeg not available")
	}
	// 4 s of tone, 2 s of silence, 4 s of tone.
	path := filepath.Join(t.TempDir(), "gap.m4a")
	cmd := exec.Command("ffmpeg", "-nostdin", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=4",
		"-f", "lavfi", "-i", "anullsrc=r=44100:cl=mono:d=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=4",
		"-filter_complex", "[0][1][2]concat=n=3:v=0:a=1",
		"-c:a", "aac", "-y", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg could not build the fixture: %v\n%s", err, out)
	}
	got, err := DetectSilences(context.Background(), "ffmpeg", filepath.Dir(path), path, []metadata.TimeRange{{Start: 2, End: 5}, {Start: 5, End: 8}, {Start: 2, End: 9}})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []metadata.TimeRange{{Start: 4, End: 5}, {Start: 5, End: 6}, {Start: 4, End: 6}} {
		if len(got[i]) != 1 || math.Abs(got[i][0].Start-want.Start) > 0.1 || math.Abs(got[i][0].End-want.End) > 0.1 {
			t.Errorf("window %d pauses %v, want about %v", i, got[i], want)
		}
	}
	if _, err := DetectSilences(context.Background(), "ffmpeg", t.TempDir(), filepath.Join(t.TempDir(), "none.m4a"), []metadata.TimeRange{{Start: 0, End: 5}}); err == nil {
		t.Error("no error for a missing file")
	}
}

// ffmpeg reads the file as an audio container only: a file shaped like a
// playlist under an audio name is an error, the file it lists never read for
// its pauses.
func TestDetectSilencesReadsNoPlaylist(t *testing.T) {
	t.Parallel()
	if !HasFFmpeg("ffmpeg") {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()
	for name, body := range map[string]string{
		"concat.mp3": "ffconcat version 1.0\nfile secret.mp3\n",
		"hls.mp3":    "#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:10,\nsecret.mp3\n#EXT-X-ENDLIST\n",
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := DetectSilences(context.Background(), "ffmpeg", dir, path, []metadata.TimeRange{{Start: 0, End: 5}}); err == nil {
			t.Errorf("%s: pauses %v and no error, want it refused", name, got)
		}
	}
}

// All three windows go to one process. Each input in the generated seek list
// carries the demuxer allowlist.
func TestDetectSilencesBatchesWindows(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the ffmpeg stand-in is a shell script")
	}
	dir := t.TempDir()
	stand, record := filepath.Join(dir, "ffmpeg"), filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo '  Duration: 00:01:00, start: 2.000000, bitrate: N/A' >&2
{
  echo invocation
  printf '%s\n' "$@"
  cat
} >> '` + record + `'
`
	file := writeStandIn(t, stand, script)
	_, err := DetectSilences(t.Context(), stand, dir, file, []metadata.TimeRange{{Start: 0, End: 5}, {Start: 11, End: 20}, {Start: 30, End: 32}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if strings.Count(log, "invocation\n") != 1 ||
		!strings.Contains(log, "-format_whitelist\n"+metadata.InputFormats+"\n-i\n") ||
		strings.Count(log, "option format_whitelist "+metadata.InputFormats+"\n") != 3 ||
		!strings.Contains(log, "inpoint 13.000000\noutpoint 22.000000") {
		t.Fatalf("ffmpeg calls and seek list:\n%s", log)
	}
}

// writeStandIn writes an executable ffmpeg stand-in at stand and returns a
// one-byte audio file beside it. The write is held so no exec another parallel
// test forks meanwhile inherits the script still open for writing (ETXTBSY on
// Linux, golang/go#22315).
func writeStandIn(t *testing.T, stand, script string) string {
	t.Helper()
	syscall.ForkLock.RLock()
	err := os.WriteFile(stand, []byte(script), 0o755) //nolint:gosec // executable test helper
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(filepath.Dir(stand), "a.mp3")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// When the budget runs out mid-list, a window read whole keeps its pauses and
// the window being read gets none, rather than the pauses of its first part.
func TestDetectSilencesBudgetKeepsWholeWindows(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the ffmpeg stand-in is a shell script")
	}
	dir := t.TempDir()
	stand := filepath.Join(dir, "ffmpeg")
	file := writeStandIn(t, stand, `#!/bin/sh
echo '  Duration: 00:10:00, start: 0.000000, bitrate: N/A' >&2
cat >/dev/null
echo '[silencedetect @ 0x1] silence_start: 2' >&2
echo '[silencedetect @ 0x1] silence_end: 3 | silence_duration: 1' >&2
echo '[silencedetect @ 0x1] silence_start: 12' >&2
echo '[silencedetect @ 0x1] silence_end: 13 | silence_duration: 1' >&2
echo '[silencedetect @ 0x1] silence_start: 14' >&2
exec sleep 30
`)
	ctx, cancel := context.WithTimeout(t.Context(), testwait.Scaled(time.Second))
	defer cancel()
	got, err := DetectSilences(ctx, stand, dir, file, []metadata.TimeRange{{Start: 0, End: 10}, {Start: 100, End: 110}, {Start: 200, End: 210}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || len(got[0]) != 1 || got[0][0] != (metadata.TimeRange{Start: 2, End: 3}) || got[1] != nil || got[2] != nil {
		t.Fatalf("pauses %v, want 2-3 in the first window and none in the window cut off", got)
	}
}

// An ffmpeg older than 5.0 refuses the list's per-file options: each window
// then runs on its own, seeking with -ss, still held to the audio demuxers.
func TestDetectSilencesOldFFmpegRunsEachWindow(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the ffmpeg stand-in is a shell script")
	}
	dir := t.TempDir()
	stand, record := filepath.Join(dir, "ffmpeg"), filepath.Join(dir, "calls")
	file := writeStandIn(t, stand, `#!/bin/sh
case " $* " in
*" concat "*)
  echo '  Duration: 00:10:00, start: 0.000000, bitrate: N/A' >&2
  cat >/dev/null
  echo "[concat @ 0x1] Line 2: unknown keyword 'option'" >&2
  echo 'pipe:0: Invalid data found when processing input' >&2
  exit 1;;
esac
{ echo invocation; printf '%s\n' "$@"; } >> '`+record+`'
echo '[silencedetect @ 0x1] silence_start: 1' >&2
echo '[silencedetect @ 0x1] silence_end: 2 | silence_duration: 1' >&2
`)
	got, err := DetectSilences(t.Context(), stand, dir, file, []metadata.TimeRange{{Start: 10, End: 20}, {Start: 30, End: 35}})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]metadata.TimeRange{{{Start: 11, End: 12}}, {{Start: 31, End: 32}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pauses %v, want %v", got, want)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if strings.Count(log, "invocation\n") != 2 || !strings.Contains(log, "-ss\n30.000\n-t\n5.000\n") ||
		strings.Count(log, "-format_whitelist\n"+metadata.InputFormats+"\n-i\n") != 2 {
		t.Fatalf("per-window runs:\n%s", log)
	}
}

// An ffmpeg built without the concat demuxer runs each window too, from the
// first file on: later files don't try the list again. A window whose run
// fails is left unanswered while the others keep their pauses.
func TestDetectSilencesWithoutConcatRunsEachWindowOnce(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the ffmpeg stand-in is a shell script")
	}
	dir := t.TempDir()
	stand, record := filepath.Join(dir, "ffmpeg"), filepath.Join(dir, "calls")
	file := writeStandIn(t, stand, `#!/bin/sh
case " $* " in
*" concat "*)
  echo concat >> '`+record+`'
  echo '  Duration: 00:10:00, start: 0.000000, bitrate: N/A' >&2
  echo "[in#1 @ 0x1] Unknown input format: 'concat'" >&2
  exit 1;;
*" -ss 30.000 "*)
  echo 'cannot read this window' >&2
  exit 1;;
esac
echo '[silencedetect @ 0x1] silence_start: 1' >&2
echo '[silencedetect @ 0x1] silence_end: 2 | silence_duration: 1' >&2
`)
	want := [][]metadata.TimeRange{{{Start: 11, End: 12}}, nil}
	for range 2 {
		got, err := DetectSilences(t.Context(), stand, dir, file, []metadata.TimeRange{{Start: 10, End: 20}, {Start: 30, End: 35}})
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("pauses %v, %v; want %v", got, err, want)
		}
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "concat\n"); n != 1 {
		t.Fatalf("the list was tried %d times, want once", n)
	}
}
