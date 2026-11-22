package media

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/kodestar/audiosilo-server/internal/netguard"
)

// ArtFetcher reads a bounded image body using a transport that checks each dial.
type ArtFetcher struct {
	client    http.Client
	userAgent string
}

// NewArtFetcher keeps the caller's guarded transport and time budget.
func NewArtFetcher(transport http.RoundTripper, timeout time.Duration, userAgent string) *ArtFetcher {
	return &ArtFetcher{client: http.Client{Transport: transport, Timeout: timeout, CheckRedirect: artRedirect}, userAgent: userAgent}
}

// ErrArtURL marks an invalid artwork URL, including a redirect with user info.
var ErrArtURL = errors.New("invalid artwork URL")

// ErrArtDowngrade marks a redirect from HTTPS to HTTP.
var ErrArtDowngrade = errors.New("artwork redirect from https to http")

func artRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > 3 {
		return errors.New("too many artwork redirects")
	}
	if !validArtURL(req.URL) {
		return ErrArtURL
	}
	if netguard.Downgrade(req, via) {
		return ErrArtDowngrade
	}
	return nil
}

func validArtURL(u *url.URL) bool {
	return u.Hostname() != "" && u.User == nil && (u.Scheme == "https" || u.Scheme == "http")
}

// ArtStatusError is the upstream HTTP status of a failed image fetch.
type ArtStatusError int

func (e ArtStatusError) Error() string { return fmt.Sprintf("artwork returned HTTP %d", e) }

// Fetch gets an image without caller headers, reading at most limit plus one
// decoded bytes to distinguish an exact-size image from an oversized one.
func (f *ArtFetcher) Fetch(ctx context.Context, raw string, limit int64) ([]byte, error) {
	// A zero-value fetcher must never fall back to http.DefaultTransport.
	if f.client.Transport == nil {
		return nil, errors.New("no guarded transport for artwork")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil || !validArtURL(req.URL) {
		return nil, ErrArtURL
	}
	req.Header.Set("Accept", "image/*")
	if f.userAgent != "" {
		req.Header.Set("User-Agent", f.userAgent)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, ArtStatusError(resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return nil, ErrImageTooLarge
	}
	return ReadLimited(resp.Body, limit)
}
