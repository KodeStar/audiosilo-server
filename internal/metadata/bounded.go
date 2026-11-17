package metadata

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
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
			(*hook)(path)
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
	return Bounded(ctx, path, timeout, func() (fs.FileInfo, error) { return os.Stat(path) }, nil)
}

// LstatBounded is os.Lstat of path as a Bounded call.
func LstatBounded(ctx context.Context, path string, timeout time.Duration) (fs.FileInfo, error) {
	return Bounded(ctx, path, timeout, func() (fs.FileInfo, error) { return os.Lstat(path) }, nil)
}

// ReadDirBounded is os.ReadDir of path as a Bounded call.
func ReadDirBounded(ctx context.Context, path string, timeout time.Duration) ([]fs.DirEntry, error) {
	return Bounded(ctx, path, timeout, func() ([]fs.DirEntry, error) { return os.ReadDir(path) }, nil)
}
