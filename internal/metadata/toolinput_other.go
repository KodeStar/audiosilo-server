//go:build !unix

package metadata

import (
	"os"
	"path/filepath"
)

// toolInput is ToolInput by path: path resolved, under root resolved, opened
// there once more and the same file as f. That second handle is held until
// cleanup: os.Open shares reads and writes but not deletion, so while it is
// open the file can't be renamed away or deleted, and no link can take its
// name before the tool opens it (f, opened through an os.Root, shares
// deletion, so it alone would not hold the name). An error when it is not
// the same file.
func toolInput(f *os.File, root, path string) (string, []*os.File, func(), error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, nil, err
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", nil, nil, err
	}
	if _, ok := relBelow(rootReal, real); !ok {
		return "", nil, nil, &os.PathError{Op: "open", Path: path, Err: errOutsideRoot}
	}
	opened, err := f.Stat()
	if err != nil {
		return "", nil, nil, err
	}
	held, err := os.Open(real)
	if err != nil {
		return "", nil, nil, err
	}
	now, err := held.Stat()
	if err == nil && !os.SameFile(opened, now) {
		err = &os.PathError{Op: "open", Path: path, Err: ErrReplacedWhileOpening}
	}
	if err != nil {
		_ = held.Close()
		return "", nil, nil, err
	}
	return real, nil, func() { _ = held.Close() }, nil
}
