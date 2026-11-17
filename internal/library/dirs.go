package library

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/mounts"
)

// The add-library folder picker (GET /admin/fs/dirs). An admin choosing a
// library root needs to see the server's folders, not just the configured
// roots, so this lists any absolute directory. Its bounds:
//
//   - admin-only at the API (the same admin can already point a library at any
//     folder and browse it, so this reveals nothing new to that role);
//   - directory names only: never a file, size, owner or timestamp;
//   - hidden (dot) folders are skipped, like the scanner and /fs skip them;
//   - absolute paths only, cleaned, and at most `limit` entries per listing;
//   - a listing behind a deadline and a few slots per mount (DirLister), so a
//     mount that stops answering can't hold the request, or pile up readers,
//     for good, nor stop the other mounts from listing.

// DirEntry is one folder in a DirListing.
type DirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"` // absolute, in the server's own path syntax
}

// DirListing is one server folder's subfolders.
type DirListing struct {
	Path string `json:"path"`
	// Parent is the folder above Path; empty at a filesystem root.
	Parent    string     `json:"parent,omitempty"`
	Dirs      []DirEntry `json:"dirs"`
	Truncated bool       `json:"truncated,omitempty"`
}

// ErrNotAbsolute rejects a relative path: the picker always names a folder from
// the filesystem root, never relative to wherever the server was started.
var ErrNotAbsolute = errors.New("path must be absolute")

// ListDirs lists the subfolders of dir (the filesystem root when dir is empty),
// sorted case-insensitively, at most limit of them. Symlinks that resolve to
// folders count as folders. An unreadable or missing dir returns the os error.
func ListDirs(dir string, limit int) (*DirListing, error) {
	if dir == "" {
		dir = filesystemRoot()
	}
	if !filepath.IsAbs(dir) {
		return nil, ErrNotAbsolute
	}
	dir = filepath.Clean(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := &DirListing{Path: dir, Dirs: []DirEntry{}}
	if parent := filepath.Dir(dir); parent != dir {
		out.Parent = parent
	}
	type dirName struct{ name, key string } // key: the lowercased sort key, computed once
	var names []dirName
	for _, e := range entries {
		if isHidden(e.Name()) {
			continue
		}
		isDir := e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			info, err := os.Stat(filepath.Join(dir, e.Name())) // follows the link
			isDir = err == nil && info.IsDir()
		}
		if isDir {
			names = append(names, dirName{e.Name(), strings.ToLower(e.Name())})
		}
	}
	sort.Slice(names, func(i, j int) bool { return names[i].key < names[j].key })
	if limit > 0 && len(names) > limit {
		names, out.Truncated = names[:limit], true
	}
	for _, n := range names {
		out.Dirs = append(out.Dirs, DirEntry{Name: n.name, Path: filepath.Join(dir, n.name)})
	}
	return out, nil
}

// ErrDirsTimedOut is a folder read (a listing, a root check, a setup preview)
// that didn't answer within its time-out, or found every slot of its mount held
// by reads that haven't returned.
var ErrDirsTimedOut = errors.New("folder listing timed out")

// DirLister runs the folder reads an admin's typed path asks for (the picker's
// listings, the setup preview's root check) each on a goroutine of its own,
// waited for at most its time-out: a readdir on a network share that stops
// answering can't be interrupted (server#224). At most its slots run at once
// on one mount (newMountSlots), a hung one keeping its slot until it returns, so
// hung reads can't pile up: with every slot of a mount held a read there waits
// for one only as long as its time-out, and the other mounts list as ever.
type DirLister struct {
	list    func(dir string, limit int) (*DirListing, error)
	check   func(root string) (string, error)
	slots   *mounts.PerMount[chan struct{}]
	timeout time.Duration
	log     *slog.Logger // nil: slog.Default(), read when it is needed
}

// NewDirLister is a DirLister of list (ListDirs; a test's stand-in), slots at
// once per mount, each waited for at most timeout.
func NewDirLister(list func(dir string, limit int) (*DirListing, error), slots int, timeout time.Duration) *DirLister {
	return &DirLister{list: list, check: CheckRoot, slots: newMountSlots(slots), timeout: timeout}
}

// List is list(dir, limit), or ErrDirsTimedOut once the time-out (or ctx) ends
// first, the listing left to finish on its own goroutine.
func (l *DirLister) List(ctx context.Context, dir string, limit int) (*DirListing, error) {
	if dir != "" && !filepath.IsAbs(dir) {
		return nil, ErrNotAbsolute // refused at once, never waiting for a slot
	}
	mount := dir
	if mount == "" { // the roots' listing: asked only then, never for a typed path
		mount = filesystemRoot()
	}
	return boundedRead(ctx, l.slots.For(mount), l.timeout, l.log, func(context.Context) (*DirListing, error) { return l.list(dir, limit) })
}

// CheckRoot is CheckRoot(root) within the same slots and time-out as a
// listing: it opens and reads the folder, which hangs on the same mounts.
func (l *DirLister) CheckRoot(ctx context.Context, root string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", ErrNotAbsolute // as List: never waiting for a slot
	}
	return boundedRead(ctx, l.slots.For(root), l.timeout, l.log, func(context.Context) (string, error) { return l.check(root) })
}

// newMountSlots is n slots per mount (mounts.Of), a mount's made when a read
// there first asks for them.
func newMountSlots(n int) *mounts.PerMount[chan struct{}] {
	return mounts.NewPerMount(func(string) chan struct{} { return make(chan struct{}, n) })
}

// readResult is what a boundedRead's goroutine hands its waiter: the read's
// answer, or the panic that ended it.
type readResult[T any] struct {
	v        T
	err      error
	panicked any
	stack    []byte
}

// boundedRead is read on a goroutine of its own holding one of slots, or
// ErrDirsTimedOut once timeout (or ctx) ends first, the read left to finish
// there. read is handed the context the waiter waits under, so a read that
// looks at it stops soon after the waiter has left. A panic in the read is
// raised again on the waiter's goroutine (the request's, where net/http
// recovers and logs it) rather than ending the process; one in a read whose
// waiter has left has no request to reach, so it is logged here (log, or
// slog.Default() when nil).
func boundedRead[T any](ctx context.Context, slots chan struct{}, timeout time.Duration, log *slog.Logger, read func(context.Context) (T, error)) (T, error) {
	var zero T
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return zero, ErrDirsTimedOut
	}
	done := make(chan readResult[T], 1) // buffered: an abandoned read still returns
	go func() {
		defer func() { <-slots }()
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			stack := debug.Stack()
			if ctx.Err() != nil { // the waiter answered ErrDirsTimedOut already
				if log == nil {
					log = slog.Default()
				}
				log.Error("folder read panicked after its time-out", "panic", p, "stack", string(stack))
			}
			done <- readResult[T]{panicked: p, stack: stack}
		}()
		v, err := read(ctx)
		done <- readResult[T]{v: v, err: err}
	}()
	var r readResult[T]
	select {
	case r = <-done:
	case <-ctx.Done():
		select {
		case r = <-done: // it answered as its time ran out: the answer stands
		default:
			return zero, ErrDirsTimedOut
		}
	}
	if r.panicked != nil {
		panic(fmt.Sprintf("folder read panicked: %v\n%s", r.panicked, r.stack))
	}
	return r.v, r.err
}

// filesystemRoot is "/" on Unix and the working directory's drive (C:\) on Windows.
// Only Windows asks for the working directory: off it, Getwd's stats of "." and
// $PWD (which can hang on a share) would only ever give "/".
func filesystemRoot() string {
	if runtime.GOOS != "windows" {
		return string(filepath.Separator)
	}
	wd, err := os.Getwd()
	if err != nil {
		return string(filepath.Separator)
	}
	return filepath.VolumeName(wd) + string(filepath.Separator)
}
