package metadata

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/testwait"
)

// TestHeldPathInput (security): where a tool can be handed no descriptor
// (Windows), its input is the file's resolved path, only while that names the
// file opened through the root: the file in place gives its own path, the
// same file through a link inside the library the link's target; a folder
// swapped for a link out of the library since the open, or another file put
// at the path, gives none. Built on every platform, so it runs here.
func TestHeldPathInput(t *testing.T) {
	t.Parallel()
	root, outside := belowFixture(t)
	file := filepath.Join(root, "Book", "01.mp3")
	open := func(path string) *os.File {
		t.Helper()
		f, _, err := OpenRegularBelow(root, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	f := open(file)
	input, extra, cleanup, err := heldPathInput(f, root, file)
	if err != nil || input != file || extra != nil {
		t.Fatalf("heldPathInput in place = %q, %v, %v; want %s alone", input, extra, err, file)
	}
	cleanup()
	if err := os.Symlink("Book", filepath.Join(root, "Again")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	again := filepath.Join(root, "Again", "01.mp3")
	if input, _, cleanup, err := heldPathInput(open(again), root, again); err != nil || input != file {
		t.Fatalf("heldPathInput through a link inside the library = %q, %v; want %s", input, err, file)
	} else {
		cleanup()
	}

	// Another file put at the path since the open.
	other := filepath.Join(root, "Book", "02.mp3")
	if err := os.WriteFile(other, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	f2 := open(other)
	if err := os.Rename(other, other+".aside"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("swapped"), 0o644); err != nil {
		t.Fatal(err)
	}
	if input, _, _, err := heldPathInput(f2, root, other); !errors.Is(err, ErrReplacedWhileOpening) {
		t.Fatalf("heldPathInput of a file replaced since its open = %q, %v; want ErrReplacedWhileOpening", input, err)
	}

	swapForLink(t, filepath.Join(root, "Book"), outside)
	if input, _, _, err := heldPathInput(f, root, file); !errors.Is(err, errOutsideRoot) {
		t.Fatalf("heldPathInput once the path leads out of the library = %q, %v; want errOutsideRoot", input, err)
	}
}

// TestBoundedToolInputGivenUp: a tool input whose looks on the share don't
// answer in time is given up on (GaveUp, a transcode's 503), and what it
// holds when it answers later (heldPathInput's second handle) is let go then,
// never left open.
func TestBoundedToolInputGivenUp(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var cleaned atomic.Bool
	_, _, _, err := boundedToolInput(context.Background(), t.TempDir(), 20*time.Millisecond, func() (string, []*os.File, func(), error) {
		<-release
		return "x", nil, func() { cleaned.Store(true) }, nil
	})
	if !GaveUp(err) {
		t.Fatalf("a tool input that never answered = %v, want it given up on", err)
	}
	close(release)
	testwait.Eventually(t, "the late input's cleanup", time.Second, cleaned.Load)

	input, _, cleanup, err := boundedToolInput(context.Background(), t.TempDir(), time.Minute, func() (string, []*os.File, func(), error) {
		return "x", nil, func() {}, nil
	})
	if err != nil || input != "x" || cleanup == nil {
		t.Fatalf("a tool input that answered = %q, %v; want it as made", input, err)
	}
}

// TestToolInputBoundedWhereItLooks: ToolInput is the one way a tool is handed
// a library file (the probe's, a silence detection's, a transcode's and a
// copy's), bounded wherever it looks at the share: on Windows, whose input is
// the path looked at again, a share that stopped answering since the open is
// given up on within the call's time (GaveUp), never held for good; on Unix
// it looks at nothing there, so it answers all the same. Not parallel: it
// holds the opens under the library.
func TestToolInputBoundedWhereItLooks(t *testing.T) {
	root, _ := belowFixture(t)
	file := filepath.Join(root, "Book", "01.mp3")
	f, _, err := OpenRegularBelow(root, file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	release := HoldOpensForTests(root)
	defer release()
	input, _, cleanup, err := ToolInput(context.Background(), f, root, file, 50*time.Millisecond)
	if toolInputLooks {
		if !GaveUp(err) {
			t.Fatalf("ToolInput on a share that stopped answering = %q, %v; want it given up on", input, err)
		}
		return
	}
	if err != nil || input == "" {
		t.Fatalf("ToolInput, which looks at nothing on the share here = %q, %v; want its input", input, err)
	}
	cleanup()
}
