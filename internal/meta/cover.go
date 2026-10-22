package meta

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"time"

	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/netguard"
)

// Community covers. A match candidate's cover_url points at the image's own host
// (Audible's CDN, Open Library, a publisher), not at metaserve, and the admin
// console's CSP loads images only from the server itself. So the server fetches
// them: as thumbnails for the match dialog, and whole when the admin takes one as
// a book's cover. The URL comes from the console (the candidate it was shown), so
// the fetch is held to what a cover needs: http(s), public addresses only, a
// bounded size and time, a few redirects, a few at once.

// ErrCoverURL is a cover URL that is not an absolute http(s) URL.
var ErrCoverURL = errors.New("meta: not an http(s) cover URL")

const (
	// coverTimeout bounds one cover fetch, redirects included.
	coverTimeout = 15 * time.Second
	// maxCoverRedirects is how many redirects a cover fetch follows (a CDN
	// moving an image, http to https).
	maxCoverRedirects = 3
	// maxCoverURLLen bounds a cover URL a caller may ask for.
	maxCoverURLLen = 2048
	// maxConcurrentCoverFetches bounds the cover fetches in flight across all
	// requests, and so how many cover bodies are being read at once (what the
	// caller holds after that is its own to bound).
	maxConcurrentCoverFetches = 4
)

// newCoverClient is the HTTP client covers are fetched with. Every connection it
// makes, a redirect's included, is checked after the name is resolved
// (netguard's dialer Control, netguard.ErrBlockedAddress), so a public name
// can't lead it into the server's own network. It uses no proxy: through one,
// the check would see only the proxy's address.
func newCoverClient(allowed func(netip.Addr) bool) *http.Client {
	dialer := netguard.Dialer(func(ap netip.AddrPort) bool { return allowed(ap.Addr()) }, 5*time.Second)
	return &http.Client{
		Timeout: coverTimeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			// Most covers come from one CDN: keep a connection per fetch slot warm.
			MaxIdleConns:        maxConcurrentCoverFetches,
			MaxIdleConnsPerHost: maxConcurrentCoverFetches,
			IdleConnTimeout:     30 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxCoverRedirects {
				return errors.New("meta: too many cover redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return ErrCoverURL
			}
			return nil
		},
	}
}

// FetchCover GETs a community cover image of at most limit bytes: ErrCoverURL for
// a URL that isn't absolute http(s), media.ErrImageTooLarge past the limit, any
// other error a fetch that failed (an address that isn't public, a timeout, a
// non-200 answer, ctx ending while it waited for a fetch slot). What the bytes are
// is the caller's to check.
func (s *Service) FetchCover(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(rawURL) > maxCoverURLLen {
		return nil, ErrCoverURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, ErrCoverURL
	}
	req.Header.Set("Accept", "image/*")
	select {
	case s.coverSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-s.coverSem }()
	resp, err := s.covers.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &statusError{code: resp.StatusCode, path: u.Host + u.EscapedPath()}
	}
	if resp.ContentLength > limit {
		return nil, media.ErrImageTooLarge
	}
	return media.ReadLimited(resp.Body, limit)
}

// HandsOutCover reports whether rawURL is a cover URL this envelope hands to a
// client: the recording's, or a rail entry's in any view of any rail. The player's
// cover proxy (GET /libraries/{id}/meta/cover) serves only such a URL, so it
// fetches nothing a caller could not already see in the book's own envelope.
func (e *Enrichment) HandsOutCover(rawURL string) bool {
	if rawURL == "" || e == nil {
		return false
	}
	if e.Recording != nil && e.Recording.CoverURL == rawURL {
		return true
	}
	hasCover := func(w MetaSeriesWork) bool { return w.CoverURL == rawURL }
	for _, s := range e.Series {
		if slices.ContainsFunc(s.Works, hasCover) {
			return true
		}
		for _, o := range s.Orderings {
			if slices.ContainsFunc(o.Works, hasCover) {
				return true
			}
		}
	}
	return false
}
