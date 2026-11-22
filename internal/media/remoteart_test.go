package media

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"
)

type artRoundTrip func(*http.Request) (*http.Response, error)

func (f artRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestArtFetcherTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const budget = 5 * time.Second
		slow := artRoundTrip(func(r *http.Request) (*http.Response, error) {
			select {
			case <-r.Context().Done():
				return nil, r.Context().Err()
			case <-time.After(2 * budget):
				return nil, errors.New("fetch ran past its deadline")
			}
		})
		fetcher := NewArtFetcher(slow, budget, "AudioSilo/test")
		start := time.Now()
		_, err := fetcher.Fetch(t.Context(), "https://art.example/cover", 1024)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != budget {
			t.Fatalf("fetch ended after %v with %v", time.Since(start), err)
		}
	})
}
