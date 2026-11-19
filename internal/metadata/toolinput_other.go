//go:build !unix

package metadata

import (
	"os"
	"path/filepath"
)

// toolInput is ToolInput by path, resolved and checked (resolvedInput).
func toolInput(f *os.File, root, path string) (string, []*os.File, func(), error) {
	input, err := resolvedInput(f, root, path)
	if err != nil {
		return "", nil, nil, err
	}
	return input, nil, func() {}, nil
}

// resolvedInput is ToolInput's input by path: path resolved, under root
// resolved, and the same file as f. An error when it is not.
func resolvedInput(f *os.File, root, path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if _, ok := relBelow(rootReal, real); !ok {
		return "", &os.PathError{Op: "open", Path: path, Err: errOutsideRoot}
	}
	opened, err := f.Stat()
	if err != nil {
		return "", err
	}
	now, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !os.SameFile(opened, now) {
		return "", &os.PathError{Op: "open", Path: path, Err: ErrReplacedWhileOpening}
	}
	return real, nil
}
