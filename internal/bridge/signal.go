package bridge

import "sync"

// signal is a broadcast: every waiter holding the channel from wait() wakes when
// fire() is called. A fire with nobody waiting is not remembered, which is fine
// for its one use: a waiter always looks at the queue after taking the channel
// and before waiting on it.
type signal struct {
	mu sync.Mutex
	ch chan struct{}
}

func newSignal() *signal { return &signal{ch: make(chan struct{})} }

// wait returns the channel the next fire closes.
func (s *signal) wait() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ch
}

// fire wakes every current waiter.
func (s *signal) fire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.ch)
	s.ch = make(chan struct{})
}
