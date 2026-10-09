package meta

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"

	"github.com/kodestar/audiosilo-server/internal/media"
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

// errBlockedAddress is a connection to an address that is not public.
var errBlockedAddress = errors.New("meta: cover host is not a public address")

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
// makes, a redirect's included, is checked after the name is resolved (the
// dialer's Control), so a public name can't lead it into the server's own
// network. It uses no proxy: through one, the check would see only the proxy's
// address.
func newCoverClient(allowed func(netip.Addr) bool) *http.Client {
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return err
			}
			if !allowed(ap.Addr().Unmap()) {
				return fmt.Errorf("%w: %s", errBlockedAddress, ap.Addr())
			}
			return nil
		},
	}
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

// notPublic are the ranges IsGlobalUnicast and IsPrivate leave in that are still
// no public host's.
var notPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),  // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved
	netip.MustParsePrefix("::/96"),          // IPv4-compatible (deprecated), which embeds any IPv4 address
	netip.MustParsePrefix("2002::/16"),      // 6to4, which embeds any IPv4 address
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64 (RFC 8215), a network's own translator
	netip.MustParsePrefix("fec0::/10"),      // site-local (deprecated)
}

// nat64 is the well-known NAT64 prefix: on an IPv6-only network every IPv4 host
// is reached through it, so the IPv4 address it carries is what is checked.
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// publicAddr reports whether a is an address on the public internet: not
// loopback, private, link-local, multicast, unspecified or reserved.
func publicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if nat64.Contains(a) {
		b := a.As16()
		a = netip.AddrFrom4([4]byte(b[12:]))
	}
	if !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	for _, p := range notPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
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
	inWorks := func(works []MetaSeriesWork) bool {
		for _, w := range works {
			if w.CoverURL == rawURL {
				return true
			}
		}
		return false
	}
	for _, s := range e.Series {
		if inWorks(s.Works) {
			return true
		}
		for _, o := range s.Orderings {
			if inWorks(o.Works) {
				return true
			}
		}
	}
	return false
}
