// Package discovery is podcast directory search for the podcasts module
// (Bridge Phase 2c-3): Apple's iTunes Search API by default, Podcast Index when
// the server holds its key pair, each asked only when picked (no fallback, no
// merged ranking). Results are public directory data only, normalized before
// they are cached, with their artwork served through this server
// (Service.Artwork) rather than fetched by a device. Everything is memory only
// and bounded: a 15-minute result cache, an artwork registry and image cache,
// per-user and server-wide token buckets, at most two directory fetches and
// four image fetches at once. Every connection goes through netguard: fixed
// HTTPS origins, no proxy, no cookies, no redirects for a directory.
package discovery

import (
	"context"
	"crypto/sha1" //nolint:gosec // Podcast Index's request signature is SHA-1 of key+secret+time; nothing else uses it
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/netguard"
	"github.com/kodestar/audiosilo-server/internal/podcast/quota"
)

// Bounds and budgets.
const (
	fetchBudget       = 5 * time.Second // one directory fetch, connect through decode
	connectTimeout    = 2 * time.Second
	tlsTimeout        = 2 * time.Second
	headerTimeout     = 3 * time.Second
	maxBodyBytes      = 2 << 20
	resultTTL         = 15 * time.Minute
	maxResultEntries  = 256
	maxResultBytes    = 8 << 20
	failureTTL        = 15 * time.Second
	maxProviderSlots  = 2
	maxUserBuckets    = 4096
	userBucketIdle    = 10 * time.Minute
	defaultCooldown   = 60
	maxCooldown       = 300
	logEvery          = time.Minute
	userSearchPerMin  = 10
	userSearchBurst   = 3
	itunesPerMin      = 20
	itunesBurst       = 1
	indexPerMin       = 30
	indexBurst        = 2
	artRegistryMax    = 4096
	artRegistryTTL    = 15 * time.Minute
	maxImageCache     = 32 << 20
	maxImageBytes     = 2 << 20
	maxImageSlots     = 4
	maxImageRedirects = 3
	artFailureTTL     = time.Minute
	userArtPerMin     = 120
	userArtBurst      = 20
	imageMissPerMin   = 60
	imageMissBurst    = 4
	// ThumbnailSize is the artwork's longer side, re-encoded as JPEG.
	ThumbnailSize = 320
)

// Options configure a Service. Transport and ArtTransport replace the guarded
// transports in tests only (no configuration reaches them); Now replaces the
// clock.
type Options struct {
	Version     string // the server version, for the User-Agent
	IndexKey    string // the Podcast Index pair, already validated (config.PodcastIndex); "" when not configured
	IndexSecret string
	// Enabled is the podcasts module's live switch: a search or image fetch
	// started while it is off publishes nothing.
	Enabled      func() bool
	Now          func() time.Time
	Log          *slog.Logger
	Transport    http.RoundTripper
	ArtTransport http.RoundTripper
}

// ProviderInfo is one entry of GET /podcasts/discovery/providers.
type ProviderInfo struct {
	ID        Provider `json:"id"`
	Available bool     `json:"available"`
}

// Service is the discovery state: caches, quotas and the fetches in flight.
type Service struct {
	opts      Options
	budget    time.Duration // fetchBudget; tests shorten it
	ends      endpoints
	rules     rules
	client    *http.Client
	artClient *http.Client

	userSearch *quota.Limiter
	userArt    *quota.Limiter
	providers  map[Provider]*quota.Limiter
	imageMiss  *quota.Limiter
	fetchSlots quota.Slots
	imageSlots quota.Slots

	mu       sync.Mutex
	gen      uint64
	root     context.Context
	cancel   context.CancelFunc
	results  *lru[*Search]
	failures *lru[*Error]
	art      *lru[string] // artwork id -> exact upstream URL
	images   *lru[[]byte]
	artFails *lru[bool]
	flights  map[string]*flight
	arts     map[string]*flight
	cooldown map[Provider]time.Time
	logged   map[string]time.Time
}

// flight is one fetch shared by every request waiting for the same key.
type flight struct {
	done    chan struct{}
	search  *Search
	jpg     []byte
	err     *Error
	waiters int
	cancel  context.CancelFunc
}

// New returns a Service. A podcastdiscoverytest build with
// AUDIOSILO_TEST_PODCAST_DISCOVERY_ORIGIN set sends every request to that
// loopback origin instead of the fixed ones; any other build ignores it.
func New(opts Options) *Service {
	return newService(opts, testOrigin())
}

func newService(opts Options, origin netip.AddrPort) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Enabled == nil {
		opts.Enabled = func() bool { return true }
	}
	s := &Service{
		opts:       opts,
		budget:     fetchBudget,
		ends:       endpointsFor(origin),
		rules:      rules{origin: origin},
		userSearch: quota.New(userSearchPerMin, userSearchBurst, maxUserBuckets, userBucketIdle, opts.Now),
		userArt:    quota.New(userArtPerMin, userArtBurst, maxUserBuckets, userBucketIdle, opts.Now),
		providers: map[Provider]*quota.Limiter{
			ITunes:       quota.New(itunesPerMin, itunesBurst, 0, time.Hour, opts.Now),
			PodcastIndex: quota.New(indexPerMin, indexBurst, 0, time.Hour, opts.Now),
		},
		imageMiss:  quota.New(imageMissPerMin, imageMissBurst, 0, time.Hour, opts.Now),
		fetchSlots: quota.NewSlots(maxProviderSlots),
		imageSlots: quota.NewSlots(maxImageSlots),
		cooldown:   map[Provider]time.Time{},
		logged:     map[string]time.Time{},
	}
	providerDial := func(ap netip.AddrPort) bool {
		return ap == origin && origin.IsValid() || netguard.PublicAddr(ap.Addr()) && ap.Port() == 443
	}
	artDial := func(ap netip.AddrPort) bool {
		return ap == origin && origin.IsValid() || netguard.PublicAddr(ap.Addr())
	}
	providerTransport := opts.Transport
	if providerTransport == nil {
		providerTransport = guardedTransport(providerDial, maxProviderSlots)
	}
	artTransport := opts.ArtTransport
	if artTransport == nil {
		artTransport = guardedTransport(artDial, maxImageSlots)
	}
	s.client = &http.Client{
		Transport: providerTransport,
		// A directory answers at its fixed URL: no redirect is followed.
		CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect },
	}
	s.artClient = &http.Client{Transport: artTransport, CheckRedirect: checkArtRedirect}
	s.resetLocked()
	return s
}

var errRedirect = errors.New("discovery: redirect refused")

// guardedTransport dials only addresses allowed accepts (netguard), through no
// proxy, with the connect/TLS/header timeouts inside the fetch budget.
func guardedTransport(allowed func(netip.AddrPort) bool, conns int) *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           netguard.Dialer(allowed, connectTimeout).DialContext,
		TLSHandshakeTimeout:   tlsTimeout,
		ResponseHeaderTimeout: headerTimeout,
		MaxIdleConns:          conns,
		MaxIdleConnsPerHost:   conns,
		IdleConnTimeout:       30 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}

// checkArtRedirect follows at most three redirects, only to http(s), and never
// from https down to http.
func checkArtRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > maxImageRedirects {
		return errRedirect
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return errRedirect
	}
	for _, prev := range via {
		if prev.URL.Scheme == "https" && req.URL.Scheme != "https" {
			return errRedirect
		}
	}
	return nil
}

// Providers lists the providers in their fixed order: Podcast Index is
// available when the server holds a valid key pair (no health probe).
func (s *Service) Providers() []ProviderInfo {
	return []ProviderInfo{{ID: ITunes, Available: true}, {ID: PodcastIndex, Available: s.opts.IndexKey != ""}}
}

// Reset ends every fetch in flight (their requests answer module_off), and
// forgets the cached results, the artwork registry and the cached images: the
// podcasts module was turned off, or the server is stopping. A fetch that
// started before it publishes nothing.
func (s *Service) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resetLocked()
}

func (s *Service) resetLocked() {
	if s.cancel != nil {
		s.cancel()
	}
	s.gen++
	s.root, s.cancel = context.WithCancel(context.Background())
	now := s.opts.Now
	s.results = newLRU[*Search](maxResultEntries, maxResultBytes, resultTTL, now)
	s.failures = newLRU[*Error](maxResultEntries, 0, failureTTL, now)
	s.art = newLRU[string](artRegistryMax, 0, artRegistryTTL, now)
	s.images = newLRU[[]byte](artRegistryMax, maxImageCache, 0, now)
	s.artFails = newLRU[bool](artRegistryMax, 0, artFailureTTL, now)
	s.flights = map[string]*flight{}
	s.arts = map[string]*flight{}
}

// Search answers one search for user (a key of the caller's account: the
// per-user budget spans their tokens and both providers). Validation and the
// caller's access are the API's. Each request spends one of the user's tokens,
// a cache hit included; only a miss spends the provider's.
func (s *Service) Search(ctx context.Context, user string, q Query) (*Search, *Error) {
	if q.Provider == PodcastIndex && s.opts.IndexKey == "" {
		return nil, ErrProviderUnavailable
	}
	if d := s.userSearch.Take(user); !d.OK {
		if d.Full {
			return nil, errBusy
		}
		return nil, errUserRateLimited.withRetry(d.RetryAfter)
	}
	key := q.key()
	s.mu.Lock()
	if res, ok := s.results.get(key); ok {
		s.registerLocked(res)
		s.mu.Unlock()
		return res, nil
	}
	if e, ok := s.failures.get(key); ok {
		s.mu.Unlock()
		return nil, e
	}
	f := s.flights[key]
	if f == nil {
		var e *Error
		if f, e = s.startSearchLocked(key, q); e != nil {
			s.mu.Unlock()
			return nil, e
		}
	}
	f.waiters++
	s.mu.Unlock()
	return wait(ctx, s, f, func() { s.leaveLocked(s.flights, key, f) }, func(f *flight) (*Search, *Error) { return f.search, f.err })
}

// wait waits for f, or for ctx: a waiter that gives up detaches, and the last
// one to leave cancels the fetch.
func wait[T any](ctx context.Context, s *Service, f *flight, leave func(), result func(*flight) (T, *Error)) (T, *Error) {
	select {
	case <-f.done:
		return result(f)
	case <-ctx.Done():
		s.mu.Lock()
		leave()
		s.mu.Unlock()
		var zero T
		return zero, errTimeout
	}
}

func (s *Service) leaveLocked(m map[string]*flight, key string, f *flight) {
	f.waiters--
	if f.waiters == 0 {
		f.cancel()
		if m[key] == f {
			delete(m, key)
		}
	}
}

// startSearchLocked admits a miss: the provider's cooldown, a fetch slot, then
// the provider's bucket, and starts the shared fetch.
func (s *Service) startSearchLocked(key string, q Query) (*flight, *Error) {
	now := s.opts.Now()
	if until, ok := s.cooldown[q.Provider]; ok && now.Before(until) {
		return nil, errProviderRateLimited.withRetry(int(max(1, until.Sub(now).Round(time.Second)/time.Second)))
	}
	if !s.fetchSlots.TryAcquire() {
		return nil, errBusy
	}
	if d := s.providers[q.Provider].Take(""); !d.OK {
		s.fetchSlots.Release()
		return nil, errProviderRateLimited.withRetry(d.RetryAfter)
	}
	ctx, cancel := context.WithTimeout(s.root, s.budget)
	f := &flight{done: make(chan struct{}), cancel: cancel}
	s.flights[key] = f
	gen := s.gen
	go func() {
		defer s.fetchSlots.Release()
		defer cancel()
		res, e := s.fetch(ctx, q)
		s.mu.Lock()
		if s.flights[key] == f {
			delete(s.flights, key)
		}
		live := gen == s.gen && s.opts.Enabled()
		switch {
		case !live:
			res, e = nil, ErrModuleOff
		case e == nil:
			s.results.put(key, res, res.size())
			s.registerLocked(res)
		case ctx.Err() == nil || errors.Is(ctx.Err(), context.DeadlineExceeded):
			// A provider failure, not every waiter leaving: answer it again
			// for a while without asking the provider.
			s.failures.put(key, e, 1)
		}
		f.search, f.err = res, e
		s.mu.Unlock()
		close(f.done)
	}()
	return f, nil
}

// registerLocked records each result's artwork URL under its id (again, on a
// cache hit, so a cached result's artwork stays reachable).
func (s *Service) registerLocked(res *Search) {
	for _, r := range res.Results {
		if r.art != "" {
			s.art.put(ArtworkID(r.art), r.art, 1)
		}
	}
}

// fetch is one GET to the provider and its normalization (no retry).
func (s *Service) fetch(ctx context.Context, q Query) (*Search, *Error) {
	req, err := s.request(ctx, q)
	if err != nil {
		return nil, errUnavailable
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, s.failed(ctx, q.Provider, "request", 0, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		seconds := retryAfter(resp.Header.Get("Retry-After"), s.opts.Now())
		s.mu.Lock()
		s.cooldown[q.Provider] = s.opts.Now().Add(time.Duration(seconds) * time.Second)
		s.mu.Unlock()
		s.logFailure(q.Provider, "rate limited", resp.StatusCode)
		return nil, errProviderRateLimited.withRetry(seconds)
	case q.Provider == PodcastIndex && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden):
		s.logFailure(q.Provider, "credentials refused", resp.StatusCode)
		return nil, errRejected
	case resp.StatusCode != http.StatusOK:
		s.logFailure(q.Provider, "status", resp.StatusCode)
		return nil, errUnavailable
	}
	body, err := readJSON(resp)
	if err != nil {
		return nil, s.failed(ctx, q.Provider, "body", 0, err)
	}
	rows, ok := []candidate(nil), false
	if q.Provider == ITunes {
		rows, ok = itunesRows(body)
	} else {
		rows, ok = indexRows(body)
	}
	if !ok {
		s.logFailure(q.Provider, "envelope", 0)
		return nil, errUnavailable
	}
	out := &Search{Provider: q.Provider, Query: q.Text, Results: s.rules.finish(rows, q.Limit)}
	if q.Country != "" {
		c := q.Country
		out.Country = &c
	}
	return out, nil
}

// request builds the provider's GET (fixed origin and parameter order).
func (s *Service) request(ctx context.Context, q Query) (*http.Request, error) {
	var target string
	if q.Provider == ITunes {
		target = s.ends.itunes + "?term=" + url.QueryEscape(q.Text) + "&media=podcast&entity=podcast&limit=" + strconv.Itoa(q.Limit) + "&country=" + q.Country + "&version=2"
	} else {
		target = s.ends.index + "?q=" + url.QueryEscape(q.Text) + "&max=" + strconv.Itoa(q.Limit)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "AudioSilo/"+s.opts.Version)
	if q.Provider == PodcastIndex {
		date := strconv.FormatInt(s.opts.Now().Unix(), 10)
		req.Header.Set("X-Auth-Key", s.opts.IndexKey)
		req.Header.Set("X-Auth-Date", date)
		req.Header.Set("Authorization", Signature(s.opts.IndexKey, s.opts.IndexSecret, date))
	}
	return req, nil
}

// Signature is Podcast Index's request signature: the lower-case hex SHA-1 of
// key, secret and the decimal Unix time, joined with nothing.
func Signature(key, secret, date string) string {
	h := sha1.Sum([]byte(key + secret + date)) //nolint:gosec // the provider's signature scheme
	return hex.EncodeToString(h[:])
}

// readJSON reads one JSON document of at most maxBodyBytes (decoded: a gzip
// body the transport inflated counts as inflated), refusing a declared length
// over the cap before reading and anything but whitespace after the document.
func readJSON(resp *http.Response) ([]byte, error) {
	if resp.ContentLength > maxBodyBytes {
		return nil, errTooLarge
	}
	body, err := readLimited(resp.Body, maxBodyBytes)
	if err != nil {
		return nil, err
	}
	if !oneJSONDocument(body) {
		return nil, errNotJSON
	}
	return body, nil
}

var (
	errTooLarge = errors.New("discovery: body over its cap")
	errNotJSON  = errors.New("discovery: not one JSON document")
)

// failed classifies a transport or body failure: the budget (or a sub-timeout)
// running out is a timeout, anything else unavailable. The log names the
// provider and a fixed reason only, never the URL or the error's text.
func (s *Service) failed(ctx context.Context, p Provider, what string, status int, err error) *Error {
	var ne net.Error
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
		s.logFailure(p, what+" timeout", status)
		return errTimeout
	}
	reason := what + " failed"
	switch {
	case errors.Is(err, netguard.ErrBlockedAddress):
		reason = "blocked address"
	case errors.Is(err, errRedirect):
		reason = "redirect"
	case errors.Is(err, errTooLarge):
		reason = "body too large"
	case errors.Is(err, errNotJSON):
		reason = "not JSON"
	}
	s.logFailure(p, reason, status)
	return errUnavailable
}

// logFailure logs a provider failure at most once a minute per provider and
// reason.
func (s *Service) logFailure(p Provider, reason string, status int) {
	key := string(p) + "|" + reason
	now := s.opts.Now()
	s.mu.Lock()
	last, seen := s.logged[key]
	if seen && now.Sub(last) < logEvery {
		s.mu.Unlock()
		return
	}
	if len(s.logged) > 64 {
		clear(s.logged)
	}
	s.logged[key] = now
	s.mu.Unlock()
	if status != 0 {
		s.opts.Log.Warn("podcast discovery failed", "provider", string(p), "reason", reason, "status", status)
	} else {
		s.opts.Log.Warn("podcast discovery failed", "provider", string(p), "reason", reason)
	}
}

// retryAfter reads an upstream 429's Retry-After (seconds or an HTTP date),
// clamped to 1..300 seconds, 60 when absent or unreadable.
func retryAfter(v string, now time.Time) int {
	if v == "" {
		return defaultCooldown
	}
	if n, err := strconv.Atoi(v); err == nil {
		return min(max(n, 1), maxCooldown)
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(int(t.Sub(now).Seconds()+0.999), 1), maxCooldown)
	}
	return defaultCooldown
}
