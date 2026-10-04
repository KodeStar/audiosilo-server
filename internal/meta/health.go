package meta

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Health is whether the community metadata service answers, for the admin
// console's Health > System.
type Health struct {
	Reachable bool      `json:"reachable"`
	LatencyMS int64     `json:"latency_ms"`
	CheckedAt time.Time `json:"checked_at"`
	Error     string    `json:"error,omitempty"` // why it didn't answer
}

// healthTTL is how long a Ping answer is reused, so a console left open on the
// System page asks the shared service at most once a minute.
const healthTTL = time.Minute

type healthCache struct {
	mu   sync.Mutex
	last *Health
}

// Ping asks the service's /healthz whether it is up (cached for a minute). Only
// call it while the metadata lookup is on: turning it off stops every request.
// One request at a time: callers meanwhile wait for its answer. It runs on the
// client's own timeout, not a caller's context, so a caller that goes away
// can't leave a "not responding" in the cache.
func (s *Service) Ping() Health {
	s.health.mu.Lock()
	defer s.health.mu.Unlock()
	if h := s.health.last; h != nil && time.Since(h.CheckedAt) < healthTTL {
		return *h
	}
	h := s.ping()
	s.health.last = &h
	return h
}

func (s *Service) ping() Health {
	ctx := context.Background() // bounded by the client's timeout
	start := time.Now()
	h := Health{CheckedAt: start}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/healthz", nil)
	if err != nil {
		h.Error = err.Error()
		return h
	}
	resp, err := s.client.http.Do(req)
	if err != nil {
		h.Error = "not responding"
		return h
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	_ = resp.Body.Close()
	h.LatencyMS = time.Since(start).Milliseconds()
	if resp.StatusCode != http.StatusOK {
		h.Error = fmt.Sprintf("answered HTTP %d", resp.StatusCode)
		return h
	}
	h.Reachable = true
	return h
}
