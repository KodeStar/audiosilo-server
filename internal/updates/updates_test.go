package updates

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGitHub answers like the releases/latest endpoint. status overrides the
// answer once set; every request is counted, and If-None-Match is honoured.
// With release set, each request signals entered on arrival and waits for
// release to close before answering.
type fakeGitHub struct {
	requests atomic.Int32
	status   atomic.Int32
	tag      atomic.Value
	sawUA    atomic.Value
	entered  chan struct{}
	release  chan struct{}
}

func (f *fakeGitHub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	f.tag.Store("v1.16.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		f.sawUA.Store(r.Header.Get("User-Agent"))
		if f.release != nil {
			f.entered <- struct{}{}
			select {
			case <-f.release:
			case <-r.Context().Done():
				return
			}
		}
		if s := f.status.Load(); s != 0 {
			if s == http.StatusForbidden {
				w.Header().Set("X-RateLimit-Remaining", "0")
			}
			w.WriteHeader(int(s))
			return
		}
		etag := `"` + f.tag.Load().(string) + `"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = w.Write([]byte(`{"tag_name":"` + f.tag.Load().(string) + `","name":"Release",` +
			`"html_url":"https://github.com/KodeStar/audiosilo-server/releases/tag/x","published_at":"2026-10-01T00:00:00Z"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheckFindsNewerRelease(t *testing.T) {
	gh := &fakeGitHub{}
	srv := gh.serve(t)
	c := New("v1.15.0", srv.URL, true, nil)

	if err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := c.Status()
	if !st.Available || st.Latest == nil || st.Latest.Version != "v1.16.0" || st.CheckedAt == nil || st.Error != "" {
		t.Fatalf("status = %+v", st)
	}
	if ua := gh.sawUA.Load(); ua != "AudioSilo/v1.15.0" {
		t.Fatalf("User-Agent = %v, want the version and nothing else", ua)
	}
	// Within a minute a Check answers without asking again.
	if err := c.Check(context.Background()); err != nil || gh.requests.Load() != 1 {
		t.Fatalf("a second Check within a minute must not request (requests = %d)", gh.requests.Load())
	}
	// Later, the conditional request gets a 304 and keeps what it knew.
	c.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	_ = c.Check(context.Background())
	st = c.Status()
	if gh.requests.Load() != 2 || st.Latest == nil || st.Latest.Version != "v1.16.0" {
		t.Fatalf("after 304: requests %d, status %+v", gh.requests.Load(), st)
	}
}

func TestDisabledMakesNoRequest(t *testing.T) {
	gh := &fakeGitHub{}
	srv := gh.serve(t)
	c := New("v1.15.0", srv.URL, false, nil)
	if err := c.Check(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Check while off = %v, want ErrDisabled", err)
	}
	if c.due() {
		t.Fatal("Run must not check while off")
	}
	if gh.requests.Load() != 0 {
		t.Fatal("no request may leave the server while the check is off")
	}
	c.SetEnabled(true)
	if !c.due() {
		t.Fatal("turned on with no check yet, Run must check")
	}
}

func TestRunChecksWhenWoken(t *testing.T) {
	gh := &fakeGitHub{}
	srv := gh.serve(t)
	c := New("v1.15.0", srv.URL, false, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	c.SetEnabled(true) // wakes Run, which checks at once
	deadline := time.Now().Add(5 * time.Second)
	for gh.requests.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if gh.requests.Load() != 1 {
		t.Fatalf("turning the check on must check once, got %d requests", gh.requests.Load())
	}
}

func TestCheckErrors(t *testing.T) {
	gh := &fakeGitHub{}
	srv := gh.serve(t)
	for status, want := range map[int32]string{http.StatusForbidden: "rate_limited", http.StatusTooManyRequests: "rate_limited", http.StatusInternalServerError: "bad_response"} {
		gh.status.Store(status)
		c := New("v1.15.0", srv.URL, true, nil)
		_ = c.Check(context.Background())
		if st := c.Status(); st.Error != want || st.Latest != nil {
			t.Errorf("HTTP %d: status %+v, want error %q", status, st, want)
		}
	}
	c := New("v1.15.0", "http://127.0.0.1:1", true, nil)
	_ = c.Check(context.Background())
	if st := c.Status(); st.Error != "unreachable" {
		t.Errorf("no server: error %q, want unreachable", st.Error)
	}
	// A prerelease or a tag that isn't a version is not offered.
	gh.status.Store(0)
	gh.tag.Store("nightly")
	c = New("v1.15.0", srv.URL, true, nil)
	_ = c.Check(context.Background())
	if st := c.Status(); st.Error != "bad_response" || st.Latest != nil {
		t.Errorf("non-version tag: %+v", st)
	}
}

func TestLocalBuildNeverOffersUpdate(t *testing.T) {
	gh := &fakeGitHub{}
	srv := gh.serve(t)
	c := New("dev", srv.URL, true, nil)
	_ = c.Check(context.Background())
	st := c.Status()
	if st.Comparable || st.Available || st.Latest == nil {
		t.Fatalf("a dev build shows the latest release but no update: %+v", st)
	}
}

func TestVersionOrder(t *testing.T) {
	cases := []struct {
		a, b  string
		newer bool
	}{
		{"v1.16.0", "v1.15.9", true},
		{"v1.15.0", "v1.15.0", false},
		{"v2.0.0", "v1.99.99", true},
		{"v1.15.0", "v1.15.0-rc.1", true},
		{"v1.15.0-rc.2", "v1.15.0-rc.1", true},
		{"1.10.0", "v1.9.0", true},
	}
	for _, tc := range cases {
		a, ok1 := parseVersion(tc.a)
		b, ok2 := parseVersion(tc.b)
		if !ok1 || !ok2 || a.newerThan(b) != tc.newer {
			t.Errorf("%s > %s = %v, want %v", tc.a, tc.b, a.newerThan(b), tc.newer)
		}
	}
	for _, bad := range []string{"dev", "", "v1.2", "v1.x.3"} {
		if _, ok := parseVersion(bad); ok {
			t.Errorf("%q must not parse", bad)
		}
	}
}

// A request whose caller's deadline passed learned nothing: it must not read
// as a successful check (and so block "Check now" for a minute).
func TestExpiredCallerRecordsNothing(t *testing.T) {
	gh := &fakeGitHub{}
	srv := gh.serve(t)
	c := New("v1.15.0", srv.URL, true, nil)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := c.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if st := c.Status(); st.CheckedAt != nil || st.Error != "" {
		t.Fatalf("an expired caller recorded a check: %+v", st)
	}
	if err := c.Check(context.Background()); err != nil || c.Status().Latest == nil {
		t.Fatalf("the next Check must ask at once: %v %+v", err, c.Status())
	}
}

func TestOnAvailable(t *testing.T) {
	gh := &fakeGitHub{}
	srv := gh.serve(t)
	var heard []string
	c := New("v1.15.0", srv.URL, true, nil)
	c.OnAvailable = func(r Release) { heard = append(heard, r.Version) }
	_ = c.Check(context.Background())
	if len(heard) != 1 || heard[0] != "v1.16.0" {
		t.Fatalf("heard = %v", heard)
	}

	// Up to date: nothing to hear.
	gh2 := &fakeGitHub{}
	srv2 := gh2.serve(t)
	quiet := New("v1.16.0", srv2.URL, true, nil)
	quiet.OnAvailable = func(Release) { t.Fatal("heard about the running version") }
	_ = quiet.Check(context.Background())
	// A failed check says nothing either.
	gh2.status.Store(http.StatusInternalServerError)
	quiet.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	_ = quiet.Check(context.Background())
}

// startInFlight starts what Run does, a check, against a GitHub that holds the
// request until release is closed, and returns once the request has arrived.
// scheduled closes when that check is done.
func startInFlight(t *testing.T) (gh *fakeGitHub, c *Checker, release, scheduled chan struct{}) {
	t.Helper()
	gh = &fakeGitHub{entered: make(chan struct{}, 4), release: make(chan struct{})}
	c = New("v1.15.0", gh.serve(t).URL, true, nil)
	scheduled = make(chan struct{})
	go func() { _ = c.check(context.Background()); close(scheduled) }()
	<-gh.entered
	return gh, c, gh.release, scheduled
}

// "Check now" while the daily check is on its way waits for that check's answer
// and returns it, without a second request.
func TestCheckJoinsTheCheckInFlight(t *testing.T) {
	gh, c, release, scheduled := startInFlight(t)
	manual := make(chan error, 1)
	go func() { manual <- c.Check(context.Background()) }()
	select {
	case err := <-manual:
		t.Fatalf("Check answered (%v) before the check in flight did: status %+v", err, c.Status())
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-manual; err != nil {
		t.Fatal(err)
	}
	if st := c.Status(); st.Latest == nil || st.Latest.Version != "v1.16.0" || !st.Available {
		t.Fatalf("Check returned before the answer was recorded: %+v", st)
	}
	<-scheduled
	if n := gh.requests.Load(); n != 1 {
		t.Fatalf("%d requests, want the one in flight only", n)
	}
}

// A caller that gives up while waiting gets its context's error at once; the
// request in flight goes on and is recorded.
func TestCheckWaitRespectsContext(t *testing.T) {
	gh, c, release, scheduled := startInFlight(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.Check(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Check = %v, want the caller's deadline", err)
	}
	if c.Status().CheckedAt != nil {
		t.Fatal("nothing should be recorded yet")
	}
	close(release)
	<-scheduled
	if st := c.Status(); st.Latest == nil || gh.requests.Load() != 1 {
		t.Fatalf("the check in flight wasn't recorded: %+v, %d requests", st, gh.requests.Load())
	}
}

// When the request a check joined records nothing (its caller went away), the
// waiting check asks itself instead of answering with an unchanged Status.
func TestCheckAsksWhenTheJoinedRequestIsAbandoned(t *testing.T) {
	gh := &fakeGitHub{entered: make(chan struct{}, 4), release: make(chan struct{})}
	c := New("v1.15.0", gh.serve(t).URL, true, nil)
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan struct{})
	go func() { _ = c.Check(ctx); close(first) }()
	<-gh.entered
	second := make(chan error, 1)
	go func() { second <- c.Check(context.Background()) }()
	time.Sleep(50 * time.Millisecond) // the second Check is waiting on the first
	cancel()
	<-first
	select {
	case <-gh.entered: // the second Check's own request
	case err := <-second:
		t.Fatalf("Check answered (%v) without asking, status %+v", err, c.Status())
	}
	close(gh.release)
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if st := c.Status(); st.Latest == nil || st.Latest.Version != "v1.16.0" || gh.requests.Load() != 2 {
		t.Fatalf("the waiting Check didn't ask itself: %+v, %d requests", st, gh.requests.Load())
	}
}
