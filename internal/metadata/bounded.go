package metadata

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// ErrReadRefused is a Bounded call's error when it was not made: reads from
// its path's mount hang already (stuckReads.saturated), as an audio read there
// is refused (openBlockFile). ErrNotAnswered is one's when it did not answer in
// time. ErrNotRegular is an audio file's OpenErr when it is no regular file (a
// FIFO, a device: never read as audio).
var (
	ErrReadRefused = errReadRefused
	ErrNotAnswered = errors.New("the drive or share did not answer in time")
	ErrNotRegular  = errNotRegular
)

// GaveUp reports whether err is a Bounded call's that was given up on: not
// made (ErrReadRefused) or not answered in time (ErrNotAnswered). The file is
// most likely on a share that stopped answering, and says nothing about the
// file itself: a caller keeps what it knows rather than read the call as "none".
func GaveUp(err error) bool {
	return errors.Is(err, ErrReadRefused) || errors.Is(err, ErrNotAnswered)
}

// Unanswered reports whether a look whose error is err went unanswered: given
// up on (GaveUp), or ctx ended (whatever err says), so its caller stops and keeps
// what it knows rather than read the look's answer.
func Unanswered(ctx context.Context, err error) bool {
	return GaveUp(err) || ctx.Err() != nil
}

// ShareFault reports whether err, a look's at a file or folder its parent
// listed or a caller named (a stat, a listing, an open), is how a share fails
// rather than an answer about the file: any error but the file gone (ENOENT,
// ENOTDIR: a folder above it replaced), a look given up on already (GaveUp) or
// a context's. EIO or ESTALE (a soft-mounted NFS share whose server stopped
// answering, or a handle it lost), ENOTCONN or ETIMEDOUT (a FUSE pool or a
// network mount that dropped), a permission (EACCES, EPERM: a share refusing a
// look it allowed the listing) among them. Such a look says nothing about
// whether the file is there: its caller keeps what it knows, as for a look
// given up on (AsGaveUp), never reads it as "none".
func ShareFault(err error) bool {
	return err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) &&
		!GaveUp(err) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// AsGaveUp is err made a look given up on (GaveUp reports it) when it is a
// ShareFault, keeping the share's own message (a permission is not "did not
// answer in time" in a run's log); any other err as it is.
func AsGaveUp(err error) error {
	if ShareFault(err) {
		return shareFaultErr{err}
	}
	return err
}

// shareFaultErr is a ShareFault's error made a look given up on (AsGaveUp).
type shareFaultErr struct{ err error }

func (e shareFaultErr) Error() string        { return e.err.Error() }
func (e shareFaultErr) Unwrap() error        { return e.err }
func (e shareFaultErr) Is(target error) bool { return target == ErrNotAnswered }

// Faulted reports whether err is a ShareFault made a look given up on
// (AsGaveUp), and the share's own error: a caller that tells a look a share
// failed from one it did not answer (a permission, which stays until an admin
// changes it, from a share that may come back).
func Faulted(err error) (cause error, ok bool) {
	var f shareFaultErr
	if errors.As(err, &f) {
		return f.err, true
	}
	return nil, false
}

// Bounded runs fn, a call on the file or folder at path that a share which
// stopped answering would hold for good (a stat, a listing, an open and its
// reads), under the bounds of a scan's audio reads (openBlockFile): on a
// goroutine of its own, waited for while ctx lives and at most timeout (none
// when timeout is 0), and counted on path's mount (stuckByMount) when given up
// on, until it returns. It is ErrReadRefused at once, fn not run, when
// maxAbandonedReads calls on that mount hang already; ErrNotAnswered when fn
// did not answer in time (left to finish, drop, when not nil, handed what it
// then returns without an error: a file to close); ctx's error when ctx ended
// first (fn not made when it had ended already, else left and counted as
// above). fn is made once it has one of its mount's slots (stuckReads.calls),
// held until it returns: with every slot held, a call waits for one within
// the same time, and is given up on unmade when none frees, so calls on a
// mount never pass the cap together. A panic in fn is its error, never the
// server's.
func Bounded[T any](ctx context.Context, path string, timeout time.Duration, fn func() (T, error), drop func(T)) (T, error) {
	return bounded(ctx, path, timeout, false, fn, drop)
}

// bounded is Bounded, look saying the call only looks at path (a stat, a
// listing: HoldReadsForTests lets it answer).
func bounded[T any](ctx context.Context, path string, timeout time.Duration, look bool, fn func() (T, error), drop func(T)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	stuck := stuckByMount.For(path)
	if stuck.saturated() {
		return zero, fmt.Errorf("%s: %w", path, ErrReadRefused)
	}
	// One timer bounds the wait for a slot and the call together (a context of
	// its own per call would cost a no-change scan, a look per audio file).
	var expire <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		expire = t.C
	}
	notAnswered := func() (T, error) {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		return zero, fmt.Errorf("%s: %w", path, ErrNotAnswered)
	}
	select {
	case stuck.calls <- struct{}{}:
	case <-ctx.Done():
		return notAnswered()
	case <-expire:
		return notAnswered()
	}
	// The slot is freed once, by whichever claims it first: fn's goroutine as
	// fn begins (freeing it when fn returns), or the give-up below when fn had
	// not begun (or await never ran it), and fn is then never made.
	var claimed atomic.Bool
	free := func() { <-stuck.calls }
	type result struct {
		v   T
		err error
	}
	r, ok := awaitUntil(ctx, expire, stuck, func() (r result) {
		if !claimed.CompareAndSwap(false, true) {
			return result{err: ErrNotAnswered}
		}
		defer free()
		defer func() {
			if p := recover(); p != nil {
				r = result{err: fmt.Errorf("%s: panic: %v", path, p)}
			}
		}()
		if hook := heldOpens.Load(); hook != nil {
			(*hook)(path, look)
		}
		r.v, r.err = fn()
		return r
	}, func(r result) {
		if drop != nil && r.err == nil {
			drop(r.v)
		}
	})
	if !ok {
		if claimed.CompareAndSwap(false, true) {
			free()
		}
		return notAnswered()
	}
	return r.v, r.err
}

// StatBounded is os.Stat of path as a Bounded call.
func StatBounded(ctx context.Context, path string, timeout time.Duration) (fs.FileInfo, error) {
	return bounded(ctx, path, timeout, true, func() (fs.FileInfo, error) { return os.Stat(path) }, nil)
}

// LstatBounded is os.Lstat of path as a Bounded call.
func LstatBounded(ctx context.Context, path string, timeout time.Duration) (fs.FileInfo, error) {
	return bounded(ctx, path, timeout, true, func() (fs.FileInfo, error) { return os.Lstat(path) }, nil)
}

// EvalSymlinksBounded is filepath.EvalSymlinks of path as a Bounded call: a
// library's root as the spelling checks list below it (library.SafeJoinListed,
// catalog's ownEntry: one form, so they read a folder's kept listing, DirNames,
// under one name), and a link met on the way.
func EvalSymlinksBounded(ctx context.Context, path string, timeout time.Duration) (string, error) {
	return Bounded(ctx, path, timeout, func() (string, error) { return filepath.EvalSymlinks(path) }, nil)
}

// ReadDirBounded is os.ReadDir of path as a Bounded call.
func ReadDirBounded(ctx context.Context, path string, timeout time.Duration) ([]fs.DirEntry, error) {
	return bounded(ctx, path, timeout, true, func() ([]fs.DirEntry, error) { return os.ReadDir(path) }, nil)
}

// ReadDirNamesBounded is the names in the folder at path, unsorted, as a
// Bounded call: os.File.Readdirnames, which, unlike os.ReadDir, neither sorts
// them nor stats an entry whose type the listing leaves out (a filesystem that
// answers DT_UNKNOWN), so a check of a name against a large folder (NamesListed)
// costs the listing alone.
func ReadDirNamesBounded(ctx context.Context, path string, timeout time.Duration) ([]string, error) {
	return bounded(ctx, path, timeout, true, func() ([]string, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return f.Readdirnames(-1)
	}, nil)
}

// NamesListed reports whether each name of rel (a slash path under root) is
// in its folder's listing as given, byte for byte, each listing's names read
// by readDir (a DirNames' Names, or ReadDirNamesBounded). A disk that ignores
// case (macOS's, a Windows or SMB share) or the Unicode spelling finds a name
// written another way, but lists it only as it is stored, so a name it finds
// but doesn't list is not there as given. A listing that fails is false; err
// is a listing given up on (GaveUp) or ctx's, the answer then unknown.
func NamesListed(ctx context.Context, root, rel string,
	readDir func(ctx context.Context, dir string) ([]string, error)) (bool, error) {
	dir := root
	for name := range strings.SplitSeq(rel, "/") {
		names, err := readDir(ctx, dir)
		if Unanswered(ctx, err) {
			return false, cmp.Or(err, ctx.Err())
		}
		if err != nil || !slices.Contains(names, name) {
			return false, nil
		}
		dir = filepath.Join(dir, name)
	}
	return true, nil
}
