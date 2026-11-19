//go:build unix

package metadata

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
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

// toolInputLooks: toolInput looks at nothing on the share (its link is made
// in the temporary folder).
const toolInputLooks = false

// inheritedInput is the descriptor a tool inherits its input on: the first of
// exec.Cmd.ExtraFiles, as the tool's own /dev/fd names it.
const inheritedInput = "/dev/fd/3"

// inputLink makes a folder of its own (owner only) holding one link, named
// "input" and path's extension, to inheritedInput. A tool opening the link
// opens its own descriptor 3 (/dev/fd/3 resolves in the process that opens
// it), and guesses its format by the name as by path's.
func inputLink(path string) (link string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", inputDirPrefix)
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

// inputDirPrefix starts the name of every folder inputLink makes, in the
// temporary folder.
const inputDirPrefix = "audiosilo-input-"

// inputDirStale is how old a folder of inputLink's is before a start takes it
// for one an earlier run left (killed before its cleanup). A tool opens the
// link once, as it starts, and reads its descriptor from then on, so a folder
// is needed only that long; an hour is far past it, so one another server
// started by the same user (or a test) just made is never taken from under
// its tool, and one a long transcode still runs on (a stream paced by its
// listener runs for hours) is no longer read.
const inputDirStale = time.Hour

// sweepToolInputs removes the folders inputLink left in the temporary folder
// (SweepToolInputs), those of the user the server runs as.
func sweepToolInputs() int {
	return sweepInputDirs(os.TempDir(), uint32(os.Getuid()), time.Now().Add(-inputDirStale)) //nolint:gosec // G115: a uid is never negative on Unix
}

// sweepInputDirs removes from tmp each folder that is inputLink's by every
// sign: named by inputDirPrefix, a folder itself (no link to one), owned by
// uid and its owner's alone (0700, as os.MkdirTemp makes it), last changed
// before the time before, and holding nothing but the one link ("input" and
// an extension) or nothing. The link is unlinked and the folder removed
// empty, never a tree: a folder that is anything else stays as it is.
func sweepInputDirs(tmp string, uid uint32, before time.Time) (removed int) {
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), inputDirPrefix) {
			continue
		}
		dir := filepath.Join(tmp, e.Name())
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || !info.ModTime().Before(before) {
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Uid != uid {
			continue
		}
		held, err := os.ReadDir(dir)
		if err != nil || len(held) > 1 {
			continue
		}
		if len(held) == 1 {
			link := held[0]
			if link.Type() != os.ModeSymlink || !strings.HasPrefix(link.Name(), "input") {
				continue
			}
			if err := os.Remove(filepath.Join(dir, link.Name())); err != nil {
				continue
			}
		}
		if os.Remove(dir) == nil {
			removed++
		}
	}
	return removed
}
