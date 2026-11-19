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
	wctx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		wctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	notAnswered := func() (T, error) {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		return zero, fmt.Errorf("%s: %w", path, ErrNotAnswered)
	}
	select {
	case stuck.calls <- struct{}{}:
	case <-wctx.Done():
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
	r, ok := await(wctx, stuck, func() (r result) {
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
