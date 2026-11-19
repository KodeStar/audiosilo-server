//go:build !unix

package media

import "os"

// descriptorInput is fileInput's for an ffmpeg without the fd: protocol (older
// than 6.1) where there is no /dev/fd to hand it a descriptor it can seek
// (Windows): the file's resolved path, re-checked as the file opened
// (recheckedInput).
func descriptorInput(f *os.File) (inputSpec, error) { return recheckedInput(f) }
