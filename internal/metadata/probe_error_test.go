package metadata

import (
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
