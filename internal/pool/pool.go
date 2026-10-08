// Package pool runs work over a list a few items at a time: the background jobs
// that wait on the shared community service (bulk matching, community chapter
// checks) bound what they ask of it with it.
package pool

import (
	"context"
	"sync"
)

// Each runs fn on every item, n at a time (at least one), handing items out
// while ctx lasts and more says to go on (asked before each one). fn checks ctx
// itself.
func Each[T any](ctx context.Context, n int, items []T, more func() bool, fn func(*T)) {
	n = max(n, 1) // no worker would leave the hand-out waiting for ctx
	jobs := make(chan *T)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			for it := range jobs {
				fn(it)
			}
		})
	}
feed:
	for i := range items {
		if !more() {
			break
		}
		select {
		case jobs <- &items[i]:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
}
