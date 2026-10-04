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
type fakeGitHub struct {
	requests atomic.Int32
	status   atomic.Int32
	tag      atomic.Value
	sawUA    atomic.Value
}

func (f *fakeGitHub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	f.tag.Store("v1.16.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		f.sawUA.Store(r.Header.Get("User-Agent"))
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
