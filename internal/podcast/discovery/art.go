package discovery

import (
	"context"
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

// ValidArtworkID reports whether id has an artwork id's shape: 64 lower-case
// hex digits.
func ValidArtworkID(id string) bool { return feed.ValidHex(id, 64) }

// Artwork answers GET /podcasts/discovery/artwork: the image a recent search
// registered under id, fetched by this server and re-encoded as a JPEG of at
// most ThumbnailSize pixels. An id of the wrong shape, unknown, expired or
// evicted is artwork_not_found and fetches nothing: a caller can never name a
// URL. Each request spends one of the user's artwork tokens, a cache hit
// included; a miss also spends the server's image bucket and holds one of four
// image slots, queueing for one (at most maxArtQueue misses waiting, each up
// to the fetch budget) when all four are busy.
func (s *Service) Artwork(ctx context.Context, user, id string) ([]byte, *Error) {
	if !ValidArtworkID(id) {
		return nil, errArtNotFound
	}
	if e := userRefusal(s.userArt.Take(user)); e != nil {
		return nil, e
	}
	s.mu.Lock()
	raw, ok := s.art.get(id)
	if !ok {
		s.mu.Unlock()
		return nil, errArtNotFound
	}
	if jpg, ok := s.images.get(id); ok {
		s.mu.Unlock()
		return jpg, nil
	}
	if _, failed := s.artFails.get(id); failed {
		s.mu.Unlock()
		return nil, errArtUnavailable
	}
	f := s.arts[id]
	if f == nil {
		var e *Error
		if f, e = s.startArtLocked(id, raw); e != nil {
			s.mu.Unlock()
			return nil, e
		}
	}
	f.waiters++
	s.mu.Unlock()
	return wait(ctx, s, f, func() { s.leaveLocked(s.arts, id, f) }, func(f *flight) ([]byte, *Error) { return f.jpg, f.err })
}

func (s *Service) startArtLocked(id, raw string) (*flight, *Error) {
	if len(s.arts) >= maxImageSlots+maxArtQueue { // fetching or queued
		return nil, errBusy
	}
	if d := s.imageMiss.Take(""); !d.OK {
		return nil, errProviderRateLimited.withRetry(d.RetryAfter)
	}
	ctx, cancel := context.WithCancel(s.root)
	f := &flight{done: make(chan struct{}), cancel: cancel}
	s.arts[id] = f
	gen := s.gen
	go func() {
		defer cancel()
		jpg, e := s.queuedImage(ctx, raw)
		s.mu.Lock()
		if s.arts[id] == f {
			delete(s.arts, id)
		}
		switch {
		case gen != s.gen || !s.opts.Enabled():
			e = ErrModuleOff
		case e == nil:
			s.images.put(id, jpg, len(jpg)+len(id))
		case e == errArtUnavailable:
			s.artFails.put(id, true, 1)
		}
		if e != nil {
			jpg = nil
		}
		f.jpg, f.err = jpg, e
		s.mu.Unlock()
		close(f.done)
	}()
	return f, nil
}

// queuedImage waits up to the fetch budget for an image slot, then fetches
// raw within the budget. Its errors: errBusy when no slot came free in time,
// errArtUnavailable for a fetch that failed or timed out (remembered for
// artFailureTTL), or ErrModuleOff when ctx ended (every waiter left, or
// Reset), which is neither cached nor remembered. A miss that never fetched
// gives back the image-miss token its admission took: only a request to an
// image host spends one.
func (s *Service) queuedImage(ctx context.Context, raw string) ([]byte, *Error) {
	queue, stop := context.WithTimeout(ctx, s.budget)
	ok := s.imageSlots.Acquire(queue)
	stop()
	switch {
	case ctx.Err() != nil:
		if ok {
			s.imageSlots.Release()
		}
		s.imageMiss.Refund("")
		return nil, ErrModuleOff
	case !ok:
		s.imageMiss.Refund("")
		return nil, errBusy
	}
	fetch, cancel := context.WithTimeout(ctx, s.budget)
	defer cancel()
	jpg, err := s.fetchImage(fetch, raw)
	s.imageSlots.Release() // before the waiters hear, as a search's
	switch {
	case err == nil:
		return jpg, nil
	case ctx.Err() != nil:
		return nil, ErrModuleOff
	}
	return nil, errArtUnavailable
}

// fetchImage GETs the image (no credential, cookie or caller header: only
// Accept and the server's User-Agent), at most maxImageBytes decoded, and
// re-encodes it: an SVG, HTML or anything image.Decode can't read fails.
func (s *Service) fetchImage(ctx context.Context, raw string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "image/*")
	req.Header.Set("User-Agent", "AudioSilo/"+s.opts.Version)
	resp, err := s.artClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, errArtUnavailable
	}
	if resp.ContentLength > maxImageBytes {
		return nil, errTooLarge
	}
	data, err := readLimited(resp.Body, maxImageBytes)
	if err != nil {
		return nil, err
	}
	return media.ThumbnailJPEG(data, ThumbnailSize)
}
