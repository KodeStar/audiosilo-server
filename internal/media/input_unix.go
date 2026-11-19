//go:build unix

package media

import (
	"context"
	"os"
	"time"
)

// descriptorInput is fileInput's for an ffmpeg without the fd: protocol (older
// than 6.1): f handed over as the child's descriptor 3 (setInput: ExtraFiles)
// and read by the file protocol as /dev/fd/3, which opens that descriptor's
// file, never a path in the library, and seeks it (an input -ss; an MP4 whose
// index sits at its end demuxes, which pipe:0 can't). The file protocol is the
// only one ffmpeg may open (-protocol_whitelist), so a crafted file reaches no
// network; and since that protocol opens any local file, the input may only
// be an audio container (transcodeArgs: -format_whitelist, audioDemuxers),
// never a manifest or a playlist that names other files. fd: and pipe:0 open
// none whatever the demuxer, which is why a podcast episode, publisher bytes,
// never takes /dev/fd/3.
// Nothing on disk is looked at for it: ctx and the wait bound the other
// platforms' re-check.
func descriptorInput(_ context.Context, f *os.File, _ time.Duration) (inputSpec, error) {
	return inputSpec{proto: "file", url: "/dev/fd/3", file: f}, nil
}
