//go:build !unix

package media

import (
	"context"
	"os"
	"time"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// descriptorInput is fileInput's for an ffmpeg without the fd: protocol (older
// than 6.1) where there is no /dev/fd to hand it a descriptor it can seek
// (Windows): the file's resolved path, re-checked as the file opened
// (recheckedInput). The re-check looks at the share again (a stat, the path's
// links), so it is a metadata.Bounded look as the open before it was (ctx, at
// most wait): on a share that stopped answering since, or failing as one does
// that a retry may get past, it is given up on (metadata.GaveUp: the stream's
// 503 share_not_answering), never a handler and its transcode slot held.
func descriptorInput(ctx context.Context, f *os.File, wait time.Duration) (inputSpec, error) {
	in, err := metadata.LookBounded(ctx, f.Name(), wait, func() (inputSpec, error) { return recheckedInput(f) })
	return in, transientGaveUp(err)
}
