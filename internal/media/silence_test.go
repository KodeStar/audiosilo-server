package media

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/metadata"
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
	syscall.ForkLock.RLock()
	err := os.WriteFile(stand, []byte(script), 0o755) //nolint:gosec // executable test helper
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "a.mp3")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = DetectSilences(t.Context(), stand, dir, file, []metadata.TimeRange{{Start: 0, End: 5}, {Start: 11, End: 20}, {Start: 30, End: 32}})
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
