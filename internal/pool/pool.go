// Package pool runs work over a list a few items at a time: the background jobs
// that wait on the shared community service (bulk matching, community chapter
// checks) bound what they ask of it with it. The daily jobs (the unknown-ASIN
// report, upcoming releases) share its schedule helpers too: WhileOn and
// Jittered.
package pool

import (
	"context"
	"sync"
)

// Each runs fn on every item, n at a time (at least one), handing items out
// while ctx lasts and more says to go on (asked before each one). fn checks ctx
// itself.
func Each[T any](ctx context.Context, n int, items []T, more func() bool, fn func(*T)) {
	if n <= 1 {
		for i := range items {
			if ctx.Err() != nil || !more() {
				break
			}
			fn(&items[i])
		}
		return
	}
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
