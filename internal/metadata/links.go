package metadata

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// LinkKind is how a name is read by its mode (an Lstat's, or a listing's
// entry type): a symbolic link (link), or a name that may be one (link and
// maybe): Lstat's irregular entry that is no folder, which on Windows is a
// junction (a mount point: since Go 1.23 neither ModeSymlink nor ModeDir, so
// walking through it as a folder would read its target's names, out of the
// root as it may be) or another reparse point (a cloud file's placeholder).
// Such a name is read (os.Readlink reads a junction's target) and followed as
// a link when it reads as one (NotALink says it does not), else taken as a
// plain name, as it always was. The library's join and scan read names so,
// and so does resolveLinks.
func LinkKind(m fs.FileMode) (link, maybe bool) {
	switch {
	case m&fs.ModeSymlink != 0:
		return true, false
	case m&fs.ModeIrregular != 0 && !m.IsDir():
		return true, true
	}
	return false, false
}

// NotALink reports whether err is a read of a link's (os.Readlink's) saying
// the name is no link: EINVAL, or Windows' ERROR_NOT_A_REPARSE_POINT (4390).
func NotALink(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.Errno(4390))
}

// MaxLinkHops is how many links one path's resolve follows (walkLinks, the
// library's resolveBelow), as filepath.EvalSymlinks allows: past it, a loop,
// which resolves to nothing.
const MaxLinkHops = 255

// errLinkLoop is walkLinks' for a path through more than MaxLinkHops links.
var errLinkLoop = errors.New("too many links")

// walkLinks is filepath.EvalSymlinks of path, an absolute one, a name that may
// be a link (LinkKind: a Windows junction, which EvalSymlinks has not followed
// since Go 1.23, and fails a path through with ENOTDIR) read with readlink and
// followed as a link when it reads as one. Each name is looked at with lstat,
// from the volume's root down; a link's target is walked in its place, from
// the link's folder (a relative one) or its volume's root (an absolute one, a
// junction's always). An error for a name not there, a name past one that is
// no folder (ENOTDIR), a link that can't be read, and a loop. The answer
// holds no link and no junction; where it leads, inside a root or out of it,
// is for the caller to check (relBelow). Built on every platform, with the
// looks handed in, so its tests run on every one; resolveLinks uses it on
// Windows alone.
func walkLinks(path string, lstat func(string) (fs.FileInfo, error), readlink func(string) (string, error)) (string, error) {
	vol := filepath.VolumeName(path)
	sep := string(filepath.Separator)
	dest := vol + sep
	rest := splitNames(path[len(vol):])
	for hops := 0; len(rest) > 0; {
		name := rest[0]
		rest = rest[1:]
		switch name {
		case ".":
			continue
		case "..":
			dest = filepath.Dir(dest)
			continue
		}
		next := filepath.Join(dest, name)
		info, err := lstat(next)
		if err != nil {
			return "", err
		}
		link, maybe := LinkKind(info.Mode())
		if !link {
			if !info.IsDir() && len(rest) > 0 {
				return "", &fs.PathError{Op: "lstat", Path: next, Err: syscall.ENOTDIR}
			}
			dest = next
			continue
		}
		to, err := readlink(next)
		if maybe && err != nil && NotALink(err) {
			// Another reparse point: a plain name, as it always was.
			dest = next
			continue
		}
		if err != nil {
			return "", err
		}
		if hops++; hops > MaxLinkHops {
			return "", &fs.PathError{Op: "lstat", Path: path, Err: errLinkLoop}
		}
		if v := filepath.VolumeName(to); v != "" || strings.HasPrefix(to, sep) || strings.HasPrefix(to, "/") {
			// Absolute: walked from its volume's root (the current one for a
			// target with none, "\foo" on Windows).
			if v != "" {
				vol = v
			}
			dest = vol + sep
			to = to[len(v):]
		}
		rest = append(splitNames(to), rest...)
	}
	return filepath.Clean(dest), nil
}

// splitNames is the names of p, a path or a link's target, in order.
func splitNames(p string) []string {
	return strings.FieldsFunc(p, func(r rune) bool { return r < 0x80 && os.IsPathSeparator(uint8(r)) })
}
