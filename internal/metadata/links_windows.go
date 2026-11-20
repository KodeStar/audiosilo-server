//go:build windows

package metadata

import (
	"os"
	"path/filepath"
)

// resolveLinks is filepath.EvalSymlinks of path with a junction followed as a
// link (walkLinks): since Go 1.23 EvalSymlinks leaves a junction as a plain
// name, and fails a path through one (ENOTDIR), so a junction inside a
// library, which the join follows (LinkKind), would refuse every file below
// it. The path walked is resolved once more by EvalSymlinks, which then only
// spells its names as the disk does (no link is left to follow), so a root
// and a path under it resolved here compare (relBelow) whatever case they
// were given in.
func resolveLinks(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	walked, err := walkLinks(abs, os.Lstat, os.Readlink)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(walked)
}
