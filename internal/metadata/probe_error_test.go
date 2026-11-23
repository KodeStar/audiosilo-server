package metadata

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

// ffprobe's specific cause comes before its generic last line: both are kept,
// without the input path or the reader's address (server#272).
func TestProbeErrorKeepsTheCause(t *testing.T) {
	err := probeError(&exec.ExitError{Stderr: []byte(
		"[mov,mp4,m4a,3gp,3g2,mj2 @ 0x1] moov atom not found\n" +
			"/srv/books/A/x.m4b: Invalid data found when processing input\n")})
	want := "[mov,mp4,m4a,3gp,3g2,mj2] moov atom not found; Invalid data found when processing input"
	if err.Error() != want {
		t.Fatalf("got %q", err.Error())
	}
}

// A refusal by the format list keeps its words, not the list.
func TestProbeErrorDropsTheList(t *testing.T) {
	t.Parallel()
	err := probeError(&exec.ExitError{Stderr: []byte(
		"[concat @ 0x1] Format not on whitelist '" + InputFormats + "'\n/srv/books/x.mp3: Invalid argument\n")})
	if want := "[concat] Format not on whitelist; Invalid argument"; err.Error() != want {
		t.Fatalf("got %q, want %q", err.Error(), want)
	}
}

// A message ending in ffmpeg's invalid-data words, or in its invalid-argument
// words after a refusal by the format list, is a rejection of the file's
// data (the same ffprobe rejects the same bytes again); any other failure is not,
// even after a line that ends in them.
func TestProbeErrorRejected(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		stderr string
		want   bool
	}{
		{"[mov,mp4,m4a,3gp,3g2,mj2 @ 0x1] moov atom not found\n/srv/x.m4b: Invalid data found when processing input\n", true},
		{"/srv/x.mp3: Invalid data found when processing input", true},
		{"/srv/x.mp3: Input/output error\n", false},
		{"/srv/x.mp3: Permission denied\n", false},
		{"[ogg @ 0x1] Header processing failed: Invalid data found when processing input\n/srv/x.ogg: Input/output error\n", false},
		// A format InputFormats leaves out, with or without a line between.
		{"[concat @ 0x1] Format not on whitelist 'mov,mp3'\n/srv/x.mp3: Invalid argument\n", true},
		{"[concat @ 0x1] Format not on whitelist 'mov,mp3'\n    Last message repeated 1 times\n/srv/x.mp3: Invalid argument\n", true},
		{"/srv/x.mp3: Invalid argument\n", false},
		{"[concat @ 0x1] Format not on whitelist 'mov,mp3'\n/srv/x.mp3: Input/output error\n", false},
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

// A stored read problem is told apart as a time-out by these words at the start
// of its detail (catalog.IsProbeTimeout, the admin wire's probe_timeout), so a
// rewording here loses the rows already stored.
func TestProbeTimeoutWording(t *testing.T) {
	t.Parallel()
	if got := ErrProbeTimeout.Error(); got != "ffprobe timed out" {
		t.Errorf("ErrProbeTimeout = %q: stored rows match \"ffprobe timed out\"", got)
	}
}
