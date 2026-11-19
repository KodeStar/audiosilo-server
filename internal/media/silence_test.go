package media

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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
	got := parseSilences(log, 100, 108)
	want := []Silence{{100, 100.5}, {102.25, 104.75}, {107.5, 108}}
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
	got, err := DetectSilences(context.Background(), "ffmpeg", filepath.Dir(path), path, 2, 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || math.Abs(got[0].Start-4) > 0.1 || math.Abs(got[0].End-6) > 0.1 {
		t.Fatalf("pauses %v, want one about 4-6 s", got)
	}
	if _, err := DetectSilences(context.Background(), "ffmpeg", t.TempDir(), filepath.Join(t.TempDir(), "none.m4a"), 0, 5); err == nil {
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
		if got, err := DetectSilences(context.Background(), "ffmpeg", dir, path, 0, 5); err == nil {
			t.Errorf("%s: pauses %v and no error, want it refused", name, got)
		}
	}
}

// ffmpeg is run held to metadata.InputFormats, the list an option of the
// input it applies to (before its -i).
func TestDetectSilencesPassesInputFormats(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the ffmpeg stand-in is a shell script")
	}
	dir := t.TempDir()
	stand, args := filepath.Join(dir, "ffmpeg"), filepath.Join(dir, "args")
	// Held so no exec another parallel test forks meanwhile inherits the file
	// still open for writing (ETXTBSY on Linux, golang/go#22315).
	syscall.ForkLock.RLock()
	err := os.WriteFile(stand, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+args+"'\n"), 0o755) //nolint:gosec // G306: the stand-in must be executable
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "a.mp3")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DetectSilences(context.Background(), stand, dir, file, 0, 5); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	at, in := slices.Index(got, "-format_whitelist"), slices.Index(got, "-i")
	if at < 0 || in != at+2 || got[at+1] != metadata.InputFormats {
		t.Fatalf("ffmpeg's arguments %q, want -format_whitelist %s and then -i", got, metadata.InputFormats)
	}
}
