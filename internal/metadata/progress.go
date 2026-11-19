package metadata

import (
	"context"
	"sync/atomic"
	"time"
)

// Progress is when a read last moved on: a call of it on a file or folder
// (an open, a block read, a Bounded look) that returned in time. Whoever waits
// on another's read (the scanner's wait on a book read under way) gives up on
// one that has stopped moving, not on one that is slow: a big book on a slow
// share that answers keeps its waiter, one stuck in a call a share that
// stopped answering never returns from does not.
type Progress struct{ last atomic.Int64 }

// NewProgress is a Progress noted now: a read that has just begun.
func NewProgress() *Progress {
	p := &Progress{}
	p.Note()
	return p
}

// Note records progress now.
func (p *Progress) Note() { p.last.Store(time.Now().UnixNano()) }

// Idle is how long since the read last moved on.
func (p *Progress) Idle() time.Duration { return time.Since(time.Unix(0, p.last.Load())) }

// progressKey carries a read's Progress (WithProgress).
type progressKey struct{}

// WithProgress has every call under ctx that returns in time (await's: the
// opens, reads and Bounded looks of the read ctx is) note p.
func WithProgress(ctx context.Context, p *Progress) context.Context {
	return context.WithValue(ctx, progressKey{}, p)
}

// noteProgress notes progress on ctx's Progress, when it has one.
func noteProgress(ctx context.Context) {
	if p, ok := ctx.Value(progressKey{}).(*Progress); ok {
		p.Note()
	}
}
