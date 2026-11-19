//go:build unix

package metadata

import (
	"os"
	"path/filepath"
	"strings"
)

// toolInput is ToolInput by descriptor (inputLink). With no link made (no
// temporary folder the server may write to: a read-only container), the
// descriptor's own name: the tool still reads f, its format guessed from its
// bytes alone, never every probe failing for want of a folder.
func toolInput(f *os.File, _, path string) (string, []*os.File, func(), error) {
	link, cleanup, err := inputLink(path)
	if err != nil {
		return inheritedInput, []*os.File{f}, func() {}, nil
	}
	return link, []*os.File{f}, cleanup, nil
}

// inheritedInput is the descriptor a tool inherits its input on: the first of
// exec.Cmd.ExtraFiles, as the tool's own /dev/fd names it.
const inheritedInput = "/dev/fd/3"

// inputLink makes a folder of its own (owner only) holding one link, named
// "input" and path's extension, to inheritedInput. A tool opening the link
// opens its own descriptor 3 (/dev/fd/3 resolves in the process that opens
// it), and guesses its format by the name as by path's.
func inputLink(path string) (link string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "audiosilo-input-")
	if err != nil {
		return "", nil, err
	}
	ext := strings.ToLower(filepath.Ext(path))
	if len(ext) > 16 {
		ext = "" // no extension ffmpeg knows
	}
	link = filepath.Join(dir, "input"+ext)
	if err := os.Symlink(inheritedInput, link); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	return link, func() { _ = os.RemoveAll(dir) }, nil
}
