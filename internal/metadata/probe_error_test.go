package metadata

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

// ffprobe's specific cause comes before its generic last line: both are kept,
// without the input path.
func TestProbeErrorKeepsTheCause(t *testing.T) {
	err := probeError(&exec.ExitError{Stderr: []byte(
		"[mov,mp4,m4a,3gp,3g2,mj2 @ 0x1] moov atom not found\n" +
			"/srv/books/A/x.m4b: Invalid data found when processing input\n")})
	want := "[mov,mp4,m4a,3gp,3g2,mj2 @ 0x1] moov atom not found; Invalid data found when processing input"
	if err.Error() != want {
		t.Fatalf("got %q", err.Error())
	}
}

// A message ending in ffmpeg's invalid-data words is a rejection of the file's
// data (the same ffprobe rejects the same bytes again); any other failure is not.
func TestProbeErrorRejected(t *testing.T) {
	for _, tc := range []struct {
		stderr string
		want   bool
	}{
		{"[mov,mp4,m4a,3gp,3g2,mj2 @ 0x1] moov atom not found\n/srv/x.m4b: Invalid data found when processing input\n", true},
		{"/srv/x.mp3: Invalid data found when processing input", true},
		{"/srv/x.mp3: Input/output error\n", false},
		{"/srv/x.mp3: Permission denied\n", false},
	} {
		err := probeError(&exec.ExitError{Stderr: []byte(tc.stderr)})
		if got := errors.Is(err, ErrProbeRejected); got != tc.want {
			t.Errorf("%q: rejected = %v, want %v", tc.stderr, got, tc.want)
		}
		if errors.Is(err, ErrProbeTimeout) {
			t.Errorf("%q: reads as a time-out", tc.stderr)
		}
	}
	if errors.Is(probeError(&exec.ExitError{}), ErrProbeRejected) {
		t.Error("a failure without a message reads as a rejection")
	}
	if errors.Is(fmt.Errorf("%w after 30s", ErrProbeTimeout), ErrProbeRejected) {
		t.Error("a time-out reads as a rejection")
	}
}
