//go:build !windows

package metadata

import "path/filepath"

// resolveLinks is filepath.EvalSymlinks of path: no junction is made here
// (walkLinks is Windows' resolve).
func resolveLinks(path string) (string, error) { return filepath.EvalSymlinks(path) }
