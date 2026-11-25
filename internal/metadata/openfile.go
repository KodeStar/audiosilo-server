package metadata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

// errReadRefused, errOpenAbandoned and errNotRegular are openBlockFile's: the
// file's mount has maxAbandonedReads reads hanging (it was not opened), its
// open did not answer in time (left to finish on its own), or it is no regular
// file (a FIFO, a device: never read as audio).
var (
	errReadRefused   = errors.New("reads from its mount are stuck")
	errOpenAbandoned = errors.New("open did not answer")
	errNotRegular    = errors.New("not a regular file")
)

// await runs fn on a goroutine of its own and waits for it as long as ctx
// lives: a call on a share that stops answering (an open, a pread, the wait for
// a process stuck in one) can't be interrupted, and would hold its caller for
// good. ok is false when ctx ended first: fn is left to finish (or not) on its
// own goroutine, counted in stuck until it does, and drop (when not nil) is
// handed what it then returns, a file to close. With ctx ended already fn is
// not run (ok false, nothing counted). A call that returns in time is progress
// on ctx's read (WithProgress).
func await[T any](ctx context.Context, stuck *stuckReads, fn func() T, drop func(T)) (v T, ok bool) {
	return awaitUntil(ctx, nil, stuck, fn, drop)
}

// awaitUntil is await, given up on too once expire fires (nil: never), so a
// caller bounding one call by a time needs no context of its own for it (a
// look per audio file in a no-change scan: Bounded).
func awaitUntil[T any](ctx context.Context, expire <-chan time.Time, stuck *stuckReads, fn func() T, drop func(T)) (v T, ok bool) {
	if ctx.Err() != nil {
		return v, false
	}
	done := make(chan T, 1)
	var state atomic.Int32 // readRunning, readAbandoned or readReturned
	go func() {
		v := fn()
		if state.Swap(readReturned) == readAbandoned {
			if drop != nil {
				drop(v)
			}
			stuck.release()
			return
		}
		done <- v
	}()
	select {
	case v = <-done:
		noteProgress(ctx)
		return v, true
	case <-ctx.Done():
	case <-expire:
	}
	// Counted before it is marked, so the goroutine's release always follows;
	// a call that returned meanwhile was never abandoned.
	stuck.n.Add(1)
	if state.CompareAndSwap(readRunning, readAbandoned) {
		return v, false
	}
	stuck.release()
	noteProgress(ctx)
	return <-done, true
}

// An await's call: still running, abandoned by its waiter, or returned.
const (
	readRunning int32 = iota
	readAbandoned
	readReturned
)

// heldOpens is HoldOpensForTests's hook, called before each open and Bounded
// call, look true for one that only looks at the path (StatBounded,
// LstatBounded, ReadDirBounded, EvalSymlinksBounded, LookBounded); nil
// outside a test.
var heldOpens atomic.Pointer[func(path string, look bool)]

// heldCloses is HoldClosesForTests's hook, called before closeBlockFile
// closes a file; nil outside a test.
var heldCloses atomic.Pointer[func(path string)]

// closeBlockFile closes f, the file openBlockFile opened at path. On a FUSE
// share (rclone, mergerfs, shfs, sshfs) the close waits for a read given up
// on to return, however long the share takes, so the read's caller does too:
// HoldClosesForTests holds it as such a share does.
func closeBlockFile(f *os.File, path string) {
	if hook := heldCloses.Load(); hook != nil {
		(*hook)(path)
	}
	_ = f.Close()
}

// OpenRegular is openRegular for the files a request or a scan opens outside
// the audio reads (a stream, a cover): a FIFO named as one is turned away at
// once (ErrNotRegular) rather than waited on for a writer that may never come,
// which would hold a Bounded call, counted on its mount, for good.
func OpenRegular(path string) (*os.File, os.FileInfo, error) { return openRegular(path) }

// OpenRegularBounded is OpenRegular as a Bounded call: a file opened after a
// call given up on is closed when its open returns.
func OpenRegularBounded(ctx context.Context, path string, timeout time.Duration) (*os.File, os.FileInfo, error) {
	type opened struct {
		f    *os.File
		info os.FileInfo
	}
	o, err := Bounded(ctx, path, timeout, func() (opened, error) {
		f, info, err := openRegular(path)
		return opened{f, info}, err
	}, func(o opened) { _ = o.f.Close() })
	return o.f, o.info, err
}

// openRegular opens the file at path for reading, without waiting for a
// writer when it is a FIFO (openFlags), and stats it: an error when it is no
// regular file.
func openRegular(path string) (*os.File, os.FileInfo, error) {
	return openRegularWith(path, func() (*os.File, error) { return os.OpenFile(path, openFlags, 0) })
}

// openRegularWith is openRegular of path, opened by open (with openFlags).
func openRegularWith(path string, open func() (*os.File, error)) (*os.File, os.FileInfo, error) {
	if hook := heldOpens.Load(); hook != nil {
		(*hook)(path, false)
	}
	f, err := open()
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = &os.PathError{Op: "open", Path: path, Err: errNotRegular}
	}
	if err == nil {
		err = blockingReads(f)
	}
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

// openBlockFile opens the file at path, under root, for reads bounded by ctx
// (blockFile's bounded, waitRead), through root (openBelow: a link swapped in
// along the path since it was checked is never followed out of the library),
// the open and its stat bounded too: refused at once
// (errReadRefused) when reads from its mount hang already
// (stuckReads.saturated); errOpenAbandoned when they don't answer while ctx
// lives, counted on the mount as a read given up on; errNotRegular when it is
// no regular file. The caller closes f.
func openBlockFile(ctx context.Context, root, path string) (b *blockFile, f *os.File, err error) {
	stuck := stuckByMount.For(path)
	if stuck.saturated() {
		return nil, nil, errReadRefused
	}
	type opened struct {
		f    *os.File
		info os.FileInfo
		err  error
	}
	o, ok := await(ctx, stuck, func() opened {
		f, info, err := openRegularWith(path, func() (*os.File, error) { return openBelow(root, path) })
		return opened{f, info, err}
	}, func(o opened) {
		if o.f != nil {
			_ = o.f.Close()
		}
	})
	if !ok {
		return nil, nil, errOpenAbandoned
	}
	if o.err != nil {
		return nil, nil, o.err
	}
	b = newBlockFile(o.f, o.info.Size())
	b.stuck = stuck
	return b, o.f, nil
}

// fingerprintChunk is how many bytes Fingerprint hashes from the head and the
// tail.
const fingerprintChunk = 64 << 10

// Fingerprint is a cheap content fingerprint of the file at path under root
// (opened through it, as a tag read is: openBlockFile), sha256 of
// (size, first 64 KiB, last 64 KiB), used only to tell "same content, new
// path" moves: not a full hash (large files stay cheap) and not a durable
// identity (that is the path). Its open and reads are bounded as a tag read's
// are (openBlockFile): timeout and ctx. "" when it can't be read in that time,
// or at all.
func Fingerprint(ctx context.Context, root, path string, timeout time.Duration) string {
	fp, _ := FingerprintErr(ctx, root, path, timeout)
	return fp
}

// FingerprintErr is Fingerprint with why it is "": an error GaveUp reports
// when its open or reads were given up on (refused, or not answered in time),
// else the open's own (the file gone, no regular file) or a read's that came
// back short (EIO).
func FingerprintErr(ctx context.Context, root, path string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, timeout, errReadDeadline)
	defer cancel()
	b, f, err := openBlockFile(ctx, root, path)
	if errors.Is(err, errOpenAbandoned) {
		return "", fmt.Errorf("%s: %w", path, ErrNotAnswered)
	}
	if err != nil {
		return "", err
	}
	defer closeBlockFile(f, path)
	h := sha256.New()
	fmt.Fprintf(h, "%d:", b.size)
	if why := b.bounded(ctx, func() {
		head := make([]byte, fingerprintChunk)
		n, _ := b.raw(head, 0)
		h.Write(head[:n])
		if b.size > fingerprintChunk {
			tail := make([]byte, fingerprintChunk)
			m, _ := b.raw(tail, b.size-fingerprintChunk)
			h.Write(tail[:m])
		}
	}); why != "" {
		return "", fmt.Errorf("%s: %s: %w", path, why, ErrNotAnswered)
	}
	if b.err != nil {
		// A short read (EIO on a soft-mounted share, the file cut short as it
		// was read): a hash of what came back is no fingerprint of the file.
		return "", fmt.Errorf("%s: %w", path, b.err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
