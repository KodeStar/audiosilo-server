package media

import (
	"context"
	"math"
	"os/exec"
	"path/filepath"
	"testing"
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
	got, err := DetectSilences(context.Background(), "ffmpeg", path, 2, 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || math.Abs(got[0].Start-4) > 0.1 || math.Abs(got[0].End-6) > 0.1 {
		t.Fatalf("pauses %v, want one about 4-6 s", got)
	}
	if _, err := DetectSilences(context.Background(), "ffmpeg", filepath.Join(t.TempDir(), "none.m4a"), 0, 5); err == nil {
		t.Error("no error for a missing file")
	}
}
