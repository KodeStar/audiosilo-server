//go:build !unix

package metadata

import (
	"os"
	"path/filepath"
)

// toolInput is ToolInput by path: path resolved, under root resolved, and the
// same file as f. An error when it is not.
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
	now, err := os.Stat(real)
	if err != nil {
		return "", nil, nil, err
	}
	if !os.SameFile(opened, now) {
		return "", nil, nil, &os.PathError{Op: "open", Path: path, Err: ErrReplacedWhileOpening}
	}
	return real, nil, func() {}, nil
}
