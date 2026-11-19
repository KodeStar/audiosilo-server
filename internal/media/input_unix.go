//go:build unix

package media

import "os"

// descriptorInput is fileInput's for an ffmpeg without the fd: protocol (older
// than 6.1): f handed over as the child's descriptor 3 (setInput: ExtraFiles)
// and read by the file protocol as /dev/fd/3, which opens that descriptor's
// file, never a path in the library, and seeks it (an input -ss; an MP4 whose
// index sits at its end demuxes, which pipe:0 can't). The file protocol is the
// only one ffmpeg may open (-protocol_whitelist): a crafted file that probes
// as a playlist reaches no network, as before when ffmpeg was handed the path.
func descriptorInput(f *os.File) (inputSpec, error) {
	return inputSpec{proto: "file", url: "/dev/fd/3", file: f, extra: true}, nil
}
