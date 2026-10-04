// Package updates asks GitHub Releases whether a newer AudioSilo server exists.
//
// The check is on by default and one switch turns it off (config update_check,
// the admin console's Settings > General). While it's on, Checker.Run asks once a
// day and a "Check now" asks at most once a minute. Each request is a plain GET
// of the repository's latest release, conditional on the last answer's ETag (a
// 304 doesn't count against GitHub's unauthenticated limit of 60 an hour per
// IP), with a User-Agent naming the running version. Nothing else is sent: no
// identifier, no library or user data. While it's off, no request is made at all.
package updates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultURL is the latest-release endpoint of the server's GitHub repository.
const DefaultURL = "https://api.github.com/repos/KodeStar/audiosilo-server/releases/latest"

// Timing.
const (
	// Interval is how often Run checks while the check is on.
	Interval = 24 * time.Hour
	// minManual is the shortest gap between two requests a "Check now" may cause.
	minManual = time.Minute
	// firstDelay holds Run's first check back after the server starts, so a
	// restart loop doesn't turn into a request loop.
	firstDelay = time.Minute
	timeout    = 15 * time.Second
	maxBody    = 1 << 20
)

// Release is a published server release.
type Release struct {
	Version     string    `json:"version"` // the tag, e.g. "v1.16.0"
	Name        string    `json:"name"`
	URL         string    `json:"url"` // the release page
	PublishedAt time.Time `json:"published_at"`
}

// Status is what the console shows about updates.
type Status struct {
	Enabled bool   `json:"enabled"`
	Current string `json:"current"` // the running version ("dev" for a local build)
	// Latest is the newest release found by the last successful check (nil before one).
	Latest *Release `json:"latest"`
	// Available reports that Latest is newer than Current. Always false when the
	// running version isn't a release (a local build: Comparable is false).
	Available  bool       `json:"update_available"`
	Comparable bool       `json:"comparable"`
	CheckedAt  *time.Time `json:"checked_at"` // the last request's time, successful or not
	// Error is why the last request failed: "rate_limited", "unreachable" or
	// "bad_response" ("" when it worked).
	Error string `json:"error"`
}

// ErrDisabled is Check's answer while the check is off.
var ErrDisabled = errors.New("update check is turned off")

// Checker checks for and remembers the latest release. Safe for concurrent use.
type Checker struct {
	current string
	url     string
	client  *http.Client
	log     *slog.Logger
	now     func() time.Time
	wake    chan struct{}

	// OnAvailable, when set before Run, hears about a newer release after each
	// check that finds one (the notifications announce each version once).
	OnAvailable func(Release)

	mu      sync.Mutex
	enabled bool
	latest  *Release
	etag    string
	checked time.Time // the last request (zero: none)
	lastErr string
	// inFlight is closed when the request on its way is recorded (nil: none), so
	// a check arriving meanwhile waits for that answer instead of asking again.
	inFlight chan struct{}
	recorded uint64 // how many requests' outcomes record has kept
}

// New returns a checker for the running version, asking url (DefaultURL when
// empty). enabled is the initial state of the switch.
func New(current, url string, enabled bool, log *slog.Logger) *Checker {
	if url == "" {
		url = DefaultURL
	}
	if log == nil {
		log = slog.Default()
	}
	return &Checker{
		current: current,
		url:     url,
		client:  &http.Client{Timeout: timeout},
		log:     log,
		now:     time.Now,
		wake:    make(chan struct{}, 1),
		enabled: enabled,
	}
}

// SetEnabled turns the check on or off. Turning it on wakes Run, which checks
// at once if the last check is more than a day old.
func (c *Checker) SetEnabled(on bool) {
	c.mu.Lock()
	was := c.enabled
	c.enabled = on
	c.mu.Unlock()
	if on && !was {
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
}

// Status reports the checker's state.
func (c *Checker) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, ok := parseVersion(c.current)
	s := Status{Enabled: c.enabled, Current: c.current, Comparable: ok, Error: c.lastErr}
	if !c.checked.IsZero() {
		t := c.checked
		s.CheckedAt = &t
	}
	if c.latest != nil {
		l := *c.latest
		s.Latest = &l
		if lv, lok := parseVersion(l.Version); ok && lok {
			s.Available = lv.newerThan(cur)
		}
	}
	return s
}

// Run checks once a day while the check is on, until ctx ends. The first check
// waits a minute after start.
func (c *Checker) Run(ctx context.Context) {
	timer := time.NewTimer(firstDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-c.wake:
		}
		if c.due() {
			_ = c.check(ctx)
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(time.Hour)
	}
}

// due reports whether Run should check now: on, and no check in the last day.
func (c *Checker) due() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enabled && (c.checked.IsZero() || c.now().Sub(c.checked) >= Interval)
}

// Check asks GitHub now ("Check now"); Status then has the answer. It refuses
// while the check is off (ErrDisabled). Within a minute of the last request it
// doesn't ask again: Status still has that request's answer. While a request is
// on its way (the daily check, or another Check) it waits for that one's answer
// rather than asking twice; ctx ending first returns its error, with Status
// unchanged.
func (c *Checker) Check(ctx context.Context) error {
	c.mu.Lock()
	enabled, recent := c.enabled, !c.checked.IsZero() && c.now().Sub(c.checked) < minManual
	c.mu.Unlock()
	switch {
	case !enabled:
		return ErrDisabled
	case !recent:
		return c.check(ctx)
	}
	return nil
}

// check makes one request and records its outcome; a failure is logged and kept
// for Status. When a request is already on its way it makes none and waits for
// that one to be recorded instead (or for ctx, whose error it then returns). If
// that request recorded nothing (its caller went away first), it asks itself.
func (c *Checker) check(ctx context.Context) error {
	c.mu.Lock()
	for c.inFlight != nil {
		wait, before := c.inFlight, c.recorded
		c.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
		c.mu.Lock()
		if c.recorded != before {
			c.mu.Unlock()
			return nil
		}
	}
	done := make(chan struct{})
	c.inFlight = done
	etag := ""
	if c.latest != nil { // only a remembered release makes a 304 meaningful
		etag = c.etag
	}
	c.mu.Unlock()

	rel, newTag, code, err := c.fetch(ctx, etag)

	c.mu.Lock()
	c.record(ctx, rel, newTag, code, err)
	c.inFlight = nil
	close(done)
	c.mu.Unlock()
	if st := c.Status(); err == nil && st.Available && c.OnAvailable != nil {
		c.OnAvailable(*st.Latest)
	}
	return nil
}

// record keeps a request's outcome. mu held.
func (c *Checker) record(ctx context.Context, rel *Release, newTag, code string, err error) {
	// The caller went away (cancelled or past its deadline): nothing was learned,
	// and fetch gives such a failure no code, so recording it would read as a
	// successful check. Ask the caller's context, not the error: the client's own
	// timeout also reads as DeadlineExceeded, and that one is "unreachable".
	if err != nil && ctx.Err() != nil {
		return
	}
	c.checked = c.now()
	c.recorded++
	c.lastErr = code
	if err != nil {
		c.log.Info("update check failed", "err", err)
		return
	}
	if rel != nil {
		c.latest = rel
		c.etag = newTag
	}
}

// fetch makes the request. rel is nil on a 304 (nothing new). code classifies a
// failure for Status.Error.
func (c *Checker) fetch(ctx context.Context, etag string) (rel *Release, newTag, code string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, "", "bad_response", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "AudioSilo/"+c.current)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", "", ctx.Err()
		}
		return nil, "", "unreachable", err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotModified:
		return nil, "", "", nil
	case resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0"):
		return nil, "", "rate_limited", fmt.Errorf("GitHub rate limit (HTTP %d)", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, "", "bad_response", fmt.Errorf("GitHub answered HTTP %d", resp.StatusCode)
	}
	var body struct {
		TagName     string    `json:"tag_name"`
		Name        string    `json:"name"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
		Draft       bool      `json:"draft"`
		Prerelease  bool      `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&body); err != nil {
		return nil, "", "bad_response", fmt.Errorf("decode release: %w", err)
	}
	if _, ok := parseVersion(body.TagName); !ok || body.Draft || body.Prerelease ||
		!strings.HasPrefix(body.HTMLURL, "https://github.com/") {
		return nil, "", "bad_response", fmt.Errorf("unexpected release %q", body.TagName)
	}
	return &Release{
		Version: body.TagName, Name: body.Name, URL: body.HTMLURL, PublishedAt: body.PublishedAt,
	}, resp.Header.Get("ETag"), "", nil
}

// version is a parsed vMAJOR.MINOR.PATCH, with an optional -prerelease.
type version struct {
	n   [3]int
	pre string
}

// parseVersion reads "v1.2.3", "1.2.3" or "v1.2.3-rc.1"; anything else (like a
// local build's "dev") isn't a version.
func parseVersion(s string) (version, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	core, pre, _ := strings.Cut(s, "-")
	core, _, _ = strings.Cut(core, "+")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return version{}, false
	}
	var v version
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return version{}, false
		}
		v.n[i] = n
	}
	v.pre = pre
	return v, true
}

// newerThan reports v > o. A release beats a prerelease of the same numbers;
// two prereleases of the same numbers compare as text.
func (v version) newerThan(o version) bool {
	for i := range v.n {
		if v.n[i] != o.n[i] {
			return v.n[i] > o.n[i]
		}
	}
	switch {
	case v.pre == o.pre:
		return false
	case v.pre == "":
		return true
	case o.pre == "":
		return false
	}
	return v.pre > o.pre
}
