package discovery

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const shared = "../testdata/phase2c3/"

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeDirectory is a local stand-in for both directories and the art host,
// reached through the service's own guarded transport (its address is the
// test origin's, the only loopback address the guard admits).
type fakeDirectory struct {
	srv   *httptest.Server
	mu    sync.Mutex
	hits  map[string]int
	last  *http.Request
	route func(w http.ResponseWriter, r *http.Request) bool // true: handled
}

func newFake(t *testing.T) *fakeDirectory {
	f := &fakeDirectory{hits: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits[r.URL.Path]++
		f.last = r.Clone(context.Background())
		route := f.route
		f.mu.Unlock()
		if route != nil && route(w, r) {
			return
		}
		switch r.URL.Path {
		case "/itunes/search":
			serveFile(t, w, shared+"providers/itunes-search.json")
		case "/index/api/1.0/search/byterm":
			serveFile(t, w, shared+"providers/podcast-index-search.json")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDirectory) origin() netip.AddrPort {
	return netip.MustParseAddrPort(f.srv.Listener.Addr().String())
}

func (f *fakeDirectory) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func (f *fakeDirectory) lastRequest() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

func (f *fakeDirectory) setRoute(route func(w http.ResponseWriter, r *http.Request) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.route = route
}

func serveFile(t *testing.T, w http.ResponseWriter, name string) {
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Error(err)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8") // what Apple sends
	_, _ = w.Write(raw)
}

type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func newTestService(t *testing.T, f *fakeDirectory, c *clock, logs *logSink) *Service {
	t.Helper()
	opts := Options{Version: "fixture", IndexKey: "fixture-key", IndexSecret: "fixture-secret", Now: c.Now}
	if logs != nil {
		opts.Log = slog.New(slog.NewTextHandler(logs, nil))
	}
	return newService(opts, f.origin())
}

func query(t *testing.T, raw string) Query {
	t.Helper()
	v, _ := url.ParseQuery(raw)
	q, e := ParseQuery(v)
	if e != nil {
		t.Fatalf("%s: %v", raw, e)
	}
	return q
}

func asJSON(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func fileJSON(t *testing.T, name string) any {
	t.Helper()
	raw, err := os.ReadFile(shared + name)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestParseQuery(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want Query
		code string
	}{
		{"q=fixture%20show", Query{ITunes, "fixture show", 20, "US"}, ""},
		{"q=%20%20Fixture%E3%80%80%C2%A0Show%20&provider=itunes&limit=50&country=gb", Query{ITunes, "Fixture Show", 50, "GB"}, ""},
		{"q=ab&provider=podcast_index&limit=1", Query{PodcastIndex, "ab", 1, ""}, ""},
		{"q=" + strings.Repeat("%C3%A9", 200), Query{ITunes, strings.Repeat("é", 200), 20, "US"}, ""},
		{"", Query{}, "invalid_query"},
		{"q=", Query{}, "invalid_query"},
		{"q=a", Query{}, "invalid_query"},
		{"q=%20a%20", Query{}, "invalid_query"},
		{"q=a%09b", Query{}, "invalid_query"},
		{"q=ab%00", Query{}, "invalid_query"},
		{"q=%ff%fe", Query{}, "invalid_query"},
		{"q=" + strings.Repeat("a", 201), Query{}, "invalid_query"},
		{"q=" + strings.Repeat("%E2%82%AC", 200) + "x", Query{}, "invalid_query"},
		{"q=ab&provider=", Query{}, "invalid_provider"},
		{"q=ab&provider=ITUNES", Query{}, "invalid_provider"},
		{"q=ab&limit=0", Query{}, "invalid_limit"},
		{"q=ab&limit=51", Query{}, "invalid_limit"},
		{"q=ab&limit=+5", Query{}, "invalid_limit"},
		{"q=ab&limit=2.0", Query{}, "invalid_limit"},
		{"q=ab&limit=", Query{}, "invalid_limit"},
		{"q=ab&country=XX", Query{}, "invalid_country"},
		{"q=ab&country=USA", Query{}, "invalid_country"},
		{"q=ab&provider=podcast_index&country=US", Query{}, "invalid_country"},
		{"q=ab&q=cd", Query{}, "invalid_request"},
		{"q=ab&token=x", Query{}, "invalid_request"},
		{"q=ab&limit=5&limit=6", Query{}, "invalid_request"},
	} {
		v, _ := url.ParseQuery(tc.raw)
		q, e := ParseQuery(v)
		switch {
		case tc.code == "" && (e != nil || q != tc.want):
			t.Errorf("%s = %+v %v", tc.raw, q, e)
		case tc.code != "" && (e == nil || e.Code != tc.code || e.Status != 400):
			t.Errorf("%s = %v, want %s", tc.raw, e, tc.code)
		}
	}
}

// Both providers' samples normalize to the shared goldens: kind filter,
// fallbacks, the >2^53 id read losslessly, zero counts kept, unsafe and
// duplicate feeds dropped, artwork proxied by id. The requests carry the
// fixed parameters and, for Podcast Index, the signing vector's headers.
func TestSearchGoldens(t *testing.T) {
	f := newFake(t)
	var vec struct {
		Key, Secret   string
		UnixSeconds   int64  `json:"unix_seconds"`
		UserAgent     string `json:"user_agent"`
		Authorization string
	}
	if err := json.Unmarshal(mustRead(t, shared+"providers/signing-vector.json"), &vec); err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Unix(vec.UnixSeconds, 0)}
	s := newTestService(t, f, c, nil)

	res, e := s.Search(context.Background(), "1", query(t, "q=fixture%20show&provider=itunes&limit=20&country=US"))
	if e != nil {
		t.Fatal(e)
	}
	if got, want := asJSON(t, res), fileJSON(t, "providers/itunes-normalized.json"); !reflect.DeepEqual(got, want) {
		t.Fatalf("itunes:\n%v\nwant\n%v", got, want)
	}
	req := f.lastRequest()
	if req.URL.RawQuery != "term=fixture+show&media=podcast&entity=podcast&limit=20&country=US&version=2" || req.Header.Get("Accept") != "application/json" ||
		req.Header.Get("User-Agent") != "AudioSilo/fixture" || req.Header.Get("Authorization") != "" || req.Header.Get("X-Auth-Key") != "" || req.Header.Get("Cookie") != "" {
		t.Fatalf("itunes request %s %v", req.URL, req.Header)
	}

	res, e = s.Search(context.Background(), "1", query(t, "q=fixture%20show&provider=podcast_index&limit=20"))
	if e != nil {
		t.Fatal(e)
	}
	if got, want := asJSON(t, res), fileJSON(t, "providers/podcast-index-normalized.json"); !reflect.DeepEqual(got, want) {
		t.Fatalf("index:\n%v\nwant\n%v", got, want)
	}
	req = f.lastRequest()
	if req.URL.RawQuery != "q=fixture+show&max=20" || req.Header.Get("X-Auth-Key") != vec.Key || req.Header.Get("X-Auth-Date") != fmt.Sprint(vec.UnixSeconds) ||
		req.Header.Get("Authorization") != vec.Authorization || req.Header.Get("User-Agent") != vec.UserAgent {
		t.Fatalf("index request %s %v", req.URL, req.Header)
	}
	for k, vals := range req.Header {
		if strings.Contains(k+strings.Join(vals, ","), vec.Secret) {
			t.Fatal("the secret was sent")
		}
	}
	if Signature(vec.Key, vec.Secret, fmt.Sprint(vec.UnixSeconds)) != vec.Authorization {
		t.Fatal("signature vector")
	}
	// Search fetched no artwork and no feed.
	if f.count("/art/one.jpg") != 0 || len(f.hits) != 2 {
		t.Fatalf("hits %v", f.hits)
	}
	// Both results' artwork is registered under the golden's id.
	if _, ok := s.art.get(ArtworkID("https://images.example.org/one.jpg")); !ok {
		t.Fatal("artwork not registered")
	}
}

func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Without a key pair Podcast Index is unavailable (409, nothing asked), and the
// providers list says so in its fixed order.
func TestProviderUnavailable(t *testing.T) {
	f := newFake(t)
	s := newService(Options{Now: (&clock{t: time.Unix(1, 0)}).Now}, f.origin())
	if got := s.Providers(); !reflect.DeepEqual(asJSON(t, map[string]any{"providers": got}), fileJSON(t, "wire.json").(map[string]any)["providers"]) {
		t.Fatalf("providers %v", got)
	}
	if _, e := s.Search(context.Background(), "1", query(t, "q=ab&provider=podcast_index")); e != ErrProviderUnavailable || e.Status != 409 {
		t.Fatalf("= %v", e)
	}
	if len(f.hits) != 0 {
		t.Fatal("asked")
	}
	s.opts.IndexKey = "k"
	if !s.Providers()[1].Available {
		t.Fatal("configured")
	}
}

// The shared error samples, plus the body bounds: each failure is its fixed
// code, never the body, and only the provider and a reason are logged.
func TestSearchFailures(t *testing.T) {
	var cases []struct {
		Case, Provider string
		Status         int
		Body           string
		Headers        map[string]string
		HTTPStatus     int `json:"http_status"`
		Code           string
		RetryAfter     int `json:"retry_after"`
	}
	if err := json.Unmarshal(mustRead(t, shared+"providers/errors.json"), &cases); err != nil {
		t.Fatal(err)
	}
	gz := func(b []byte) []byte {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		_, _ = w.Write(b)
		_ = w.Close()
		return buf.Bytes()
	}
	bomb := gz(append([]byte(`{"results":["`), bytes.Repeat([]byte("a"), maxBodyBytes)...))
	type extra struct {
		name   string
		handle func(w http.ResponseWriter)
		code   string
	}
	extras := []extra{
		{"trailing", func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"results":[]} {}`)) }, "discovery_unavailable"},
		{"results not array", func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"results":{}}`)) }, "discovery_unavailable"},
		{"declared oversize", func(w http.ResponseWriter) {
			w.Header().Set("Content-Length", fmt.Sprint(maxBodyBytes+1))
			_, _ = w.Write(make([]byte, maxBodyBytes+1))
		}, "discovery_unavailable"},
		{"chunked oversize", func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"results":["`))
			w.(http.Flusher).Flush()
			_, _ = w.Write(bytes.Repeat([]byte("a"), maxBodyBytes))
		}, "discovery_unavailable"},
		{"gzip bomb", func(w http.ResponseWriter) {
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(bomb)
		}, "discovery_unavailable"},
		{"gzip ok", func(w http.ResponseWriter) {
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(gz([]byte(`{"results":[]}`)))
		}, ""},
		{"empty", func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"resultCount":0,"results":[]}`)) }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.Case, func(t *testing.T) {
			f := newFake(t)
			c := &clock{t: time.Unix(1700000000, 0)}
			logs := &logSink{}
			s := newTestService(t, f, c, logs)
			s.budget = 300 * time.Millisecond
			f.setRoute(func(w http.ResponseWriter, r *http.Request) bool {
				if tc.Case == "timeout" {
					select {
					case <-r.Context().Done():
					case <-time.After(5 * time.Second):
					}
					return true
				}
				for k, v := range tc.Headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.Status)
				_, _ = w.Write([]byte(tc.Body))
				return true
			})
			provider := tc.Provider
			if provider == "" {
				provider = "itunes"
			}
			_, e := s.Search(context.Background(), "1", query(t, "q=fixture&provider="+provider))
			if e == nil || e.Status != tc.HTTPStatus || e.Code != tc.Code || e.RetryAfter != tc.RetryAfter {
				t.Fatalf("= %+v", e)
			}
			out := logs.String() + e.Message
			for _, leak := range []string{"fixture-key", "fixture-secret", "169.254", "meta-data", "127.0.0.1", "term="} {
				if strings.Contains(out, leak) {
					t.Fatalf("leaked %q: %s", leak, out)
				}
			}
			if !strings.Contains(logs.String(), "provider="+provider) {
				t.Fatalf("log %s", logs)
			}
		})
	}
	for _, tc := range extras {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			s := newTestService(t, f, &clock{t: time.Unix(1, 0)}, nil)
			f.setRoute(func(w http.ResponseWriter, _ *http.Request) bool { tc.handle(w); return true })
			res, e := s.Search(context.Background(), "1", query(t, "q=fixture"))
			if tc.code == "" {
				if e != nil || res.Results == nil || len(res.Results) != 0 {
					t.Fatalf("= %+v %v", res, e)
				}
				raw, _ := json.Marshal(res)
				if !strings.Contains(string(raw), `"results":[]`) {
					t.Fatal(string(raw))
				}
				return
			}
			if e == nil || e.Code != tc.code {
				t.Fatalf("= %v", e)
			}
		})
	}
}

// A 429's Retry-After (seconds or a date, clamped, 60 by default) cools the
// provider down for every key; the other provider is unaffected.
func TestCooldown(t *testing.T) {
	now := time.Unix(1700000000, 0)
	for v, want := range map[string]int{"": 60, "0": 1, "120": 120, "9999": 300, "junk": 60, now.Add(30 * time.Second).UTC().Format(http.TimeFormat): 30, now.Add(-time.Hour).UTC().Format(http.TimeFormat): 1} {
		if got := retryAfter(v, now); got != want {
			t.Errorf("retryAfter(%q) = %d, want %d", v, got, want)
		}
	}
	f := newFake(t)
	c := &clock{t: now}
	s := newTestService(t, f, c, nil)
	f.setRoute(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/itunes/search" {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(429)
			return true
		}
		return false
	})
	if _, e := s.Search(context.Background(), "1", query(t, "q=one")); e == nil || e.Code != "provider_rate_limited" || e.RetryAfter != 30 {
		t.Fatalf("= %v", e)
	}
	c.Add(10 * time.Second)
	if _, e := s.Search(context.Background(), "2", query(t, "q=two")); e == nil || e.Code != "provider_rate_limited" || e.RetryAfter != 20 {
		t.Fatalf("cooling = %+v", e)
	}
	if f.count("/itunes/search") != 1 {
		t.Fatal("asked while cooling")
	}
	if _, e := s.Search(context.Background(), "2", query(t, "q=two&provider=podcast_index")); e != nil {
		t.Fatalf("index = %v", e)
	}
	c.Add(21 * time.Second)
	f.setRoute(nil)
	if _, e := s.Search(context.Background(), "3", query(t, "q=two")); e != nil {
		t.Fatalf("after = %v", e)
	}
}

// Budgets: a user's 10/minute burst 3 counts cache hits and spans providers;
// a miss spends the provider's bucket (iTunes burst 1); a failure is answered
// again for 15 s without asking; an entry lives 15 minutes.
func TestQuotasAndCache(t *testing.T) {
	f := newFake(t)
	c := &clock{t: time.Unix(1700000000, 0)}
	s := newTestService(t, f, c, nil)
	ctx := context.Background()
	q := query(t, "q=fixture")
	for i := range 3 {
		if _, e := s.Search(ctx, "1", q); e != nil {
			t.Fatalf("%d: %v", i, e)
		}
	}
	if f.count("/itunes/search") != 1 {
		t.Fatal("hits were fetched")
	}
	if _, e := s.Search(ctx, "1", query(t, "q=fixture&provider=podcast_index")); e == nil || e.Code != "discovery_rate_limited" || e.Status != 429 || e.RetryAfter != 6 {
		t.Fatalf("user = %+v", e)
	}
	// Another user: a different key misses, and iTunes' bucket (burst 1) is spent.
	if _, e := s.Search(ctx, "2", query(t, "q=other")); e == nil || e.Code != "provider_rate_limited" || e.RetryAfter != 3 {
		t.Fatalf("provider = %+v", e)
	}
	if _, e := s.Search(ctx, "2", q); e != nil {
		t.Fatalf("a hit needs no provider token: %v", e)
	}
	// Keys differ by country and limit.
	c.Add(3 * time.Second)
	if _, e := s.Search(ctx, "2", query(t, "q=fixture&country=GB")); e != nil || f.count("/itunes/search") != 2 {
		t.Fatalf("country key %v", e)
	}
	// Failures are remembered for 15 s.
	c.Add(time.Minute)
	f.setRoute(func(w http.ResponseWriter, _ *http.Request) bool { w.WriteHeader(500); return true })
	if _, e := s.Search(ctx, "3", query(t, "q=broken")); e == nil || e.Code != "discovery_unavailable" {
		t.Fatalf("= %v", e)
	}
	c.Add(14 * time.Second)
	if _, e := s.Search(ctx, "3", query(t, "q=broken")); e == nil || e.Code != "discovery_unavailable" || f.count("/itunes/search") != 3 {
		t.Fatalf("suppressed = %v, %d", e, f.count("/itunes/search"))
	}
	c.Add(2 * time.Second)
	f.setRoute(nil)
	if _, e := s.Search(ctx, "4", query(t, "q=broken")); e != nil || f.count("/itunes/search") != 4 {
		t.Fatalf("after = %v", e)
	}
	// Expiry after 15 minutes.
	c.Add(15 * time.Minute)
	if _, e := s.Search(ctx, "5", q); e != nil || f.count("/itunes/search") != 5 {
		t.Fatalf("expired = %v %d", e, f.count("/itunes/search"))
	}
}

// Concurrent requests for one key share one fetch (one provider token, one
// user token each); the slots bound different keys to two fetches.
func TestCoalescingAndSlots(t *testing.T) {
	f := newFake(t)
	c := &clock{t: time.Unix(1700000000, 0)}
	s := newTestService(t, f, c, nil)
	release := make(chan struct{})
	var started atomic.Int32
	f.setRoute(func(w http.ResponseWriter, r *http.Request) bool {
		started.Add(1)
		<-release
		return false
	})
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan *Error, 3)
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.Search(ctx, fmt.Sprint(i), query(t, "q=shared"))
			errs <- e
		}()
	}
	waitFor(t, func() bool { return started.Load() == 1 && waiters(s, ITunes) == 3 })
	// A second key takes the second slot (the index); a third finds none.
	go func() { _, _ = s.Search(ctx, "x", query(t, "q=shared&provider=podcast_index")) }()
	waitFor(t, func() bool { return started.Load() == 2 })
	if _, e := s.Search(ctx, "y", query(t, "q=third&provider=podcast_index")); e == nil || e.Code != "discovery_busy" || e.RetryAfter != 1 || e.Status != 503 {
		t.Fatalf("busy = %+v", e)
	}
	close(release)
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if f.count("/itunes/search") != 1 {
		t.Fatalf("fetched %d times", f.count("/itunes/search"))
	}
}

func waiters(s *Service, p Provider) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, f := range s.flights {
		if strings.HasPrefix(k, string(p)+"\x00") {
			n += f.waiters
		}
	}
	return n
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A waiter that leaves detaches; the last one cancels the fetch, which is
// neither cached nor remembered as a failure. Reset (the module turned off)
// ends a fetch in flight, publishes nothing and empties every cache.
func TestCancellationAndReset(t *testing.T) {
	f := newFake(t)
	c := &clock{t: time.Unix(1700000000, 0)}
	s := newTestService(t, f, c, nil)
	gone := make(chan struct{}, 4)
	f.setRoute(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/itunes/search" {
			return false
		}
		<-r.Context().Done()
		gone <- struct{}{}
		return true
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *Error)
	go func() { _, e := s.Search(ctx, "1", query(t, "q=leave")); done <- e }()
	waitFor(t, func() bool { return f.count("/itunes/search") == 1 })
	cancel()
	<-done
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the last waiter's leaving did not cancel the fetch")
	}
	s.mu.Lock()
	if s.failures.len() != 0 || s.results.len() != 0 {
		t.Fatal("a cancelled fetch was recorded")
	}
	s.mu.Unlock()

	c.Add(time.Minute)
	go func() { _, e := s.Search(context.Background(), "2", query(t, "q=reset")); done <- e }()
	waitFor(t, func() bool { return f.count("/itunes/search") == 2 })
	s.Reset()
	if e := <-done; e != ErrModuleOff {
		t.Fatalf("reset = %v", e)
	}
	// Re-enabled: empty, and the next search fetches again.
	f.setRoute(nil)
	c.Add(time.Minute)
	if _, e := s.Search(context.Background(), "3", query(t, "q=fixture")); e != nil {
		t.Fatal(e)
	}
	id := ArtworkID("https://images.example.org/one.jpg")
	s.Reset()
	if _, e := s.Artwork(context.Background(), "3", id); e == nil || e.Code != "artwork_not_found" {
		t.Fatalf("art after reset = %v", e)
	}
	// A module switched off while the fetch ran publishes nothing.
	off := atomic.Bool{}
	s.opts.Enabled = func() bool { return !off.Load() }
	f.setRoute(func(w http.ResponseWriter, r *http.Request) bool { off.Store(true); return false })
	c.Add(time.Minute)
	if _, e := s.Search(context.Background(), "4", query(t, "q=fixture")); e != ErrModuleOff {
		t.Fatalf("off midflight = %v", e)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.results.len() != 0 || s.art.len() != 0 {
		t.Fatal("published while off")
	}
}

// The per-user map holds 4,096 active buckets: a new user past that is busy,
// never evicting an active budget.
func TestUserMapFull(t *testing.T) {
	f := newFake(t)
	c := &clock{t: time.Unix(1700000000, 0)}
	s := newTestService(t, f, c, nil)
	for i := range maxUserBuckets {
		s.userSearch.Take(fmt.Sprint(i))
	}
	if _, e := s.Search(context.Background(), "new", query(t, "q=fixture")); e == nil || e.Code != "discovery_busy" || e.RetryAfter != 1 {
		t.Fatalf("= %v", e)
	}
	c.Add(userBucketIdle)
	if _, e := s.Search(context.Background(), "new", query(t, "q=fixture")); e != nil {
		t.Fatalf("idle swept = %v", e)
	}
}

func pngBytes(t *testing.T, w, h int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.NRGBA{200, 10, 10, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// artService serves a search whose results' artwork lives on the fake's
// /art/ paths (the test origin), and returns the ids it registered.
func artService(t *testing.T, f *fakeDirectory, c *clock, names ...string) (*Service, map[string]string) {
	t.Helper()
	s := newTestService(t, f, c, nil)
	base := "http://" + f.origin().String()
	rows := []string{}
	ids := map[string]string{}
	for i, n := range names {
		art := base + "/art/" + n
		if strings.HasPrefix(n, "http") {
			art = n
		}
		ids[n] = ArtworkID(art)
		rows = append(rows, fmt.Sprintf(`{"kind":"podcast","collectionName":"Show %d","feedUrl":"https://feeds.example.org/%d.xml","artworkUrl600":%q}`, i, i, art))
	}
	f.setRoute(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/itunes/search" {
			_, _ = w.Write([]byte(`{"results":[` + strings.Join(rows, ",") + `]}`))
			return true
		}
		return false
	})
	if _, e := s.Search(context.Background(), "u", query(t, "q=art&limit=50")); e != nil {
		t.Fatal(e)
	}
	return s, ids
}

// Artwork: only a registered id is fetched, through the guard, as a
// re-encoded JPEG of at most 320 px; anything that isn't an image, is too
// large, or is reached by a refused redirect fails without retrying for a
// minute. No credential or cookie goes with it.
func TestArtwork(t *testing.T) {
	f := newFake(t)
	c := &clock{t: time.Unix(1700000000, 0)}
	big := pngBytes(t, 800, 600)
	var otherHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { otherHits.Add(1); _, _ = w.Write(big) }))
	defer other.Close()
	s, ids := artService(t, f, c, "big.png", "page.html", "image.svg", "huge.png", "to-private", "to-loopback-name", "to-other-port", "loop", "missing", "https://images.example.org/one.jpg")
	var mu sync.Mutex
	var headers []http.Header
	f.setRoute(func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/art/") {
			return false
		}
		mu.Lock()
		headers = append(headers, r.Header.Clone())
		mu.Unlock()
		switch strings.TrimPrefix(r.URL.Path, "/art/") {
		case "big.png":
			_, _ = w.Write(big)
		case "page.html":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("<html><script>alert(1)</script></html>"))
		case "image.svg":
			_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
		case "huge.png":
			_, _ = w.Write(make([]byte, maxImageBytes+1))
		case "to-private":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
		case "to-loopback-name":
			http.Redirect(w, r, "http://localhost:"+other.URL[strings.LastIndex(other.URL, ":")+1:]+"/x.png", http.StatusFound)
		case "to-other-port":
			http.Redirect(w, r, other.URL+"/x.png", http.StatusFound)
		case "loop":
			http.Redirect(w, r, "/art/loop", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
		return true
	})
	ctx := context.Background()
	jpg, e := s.Artwork(ctx, "u", ids["big.png"])
	if e != nil {
		t.Fatal(e)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(jpg))
	if err != nil || format != "jpeg" || cfg.Width != 320 || cfg.Height != 240 {
		t.Fatalf("thumbnail %s %dx%d %v", format, cfg.Width, cfg.Height, err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(jpg)); err != nil {
		t.Fatal(err)
	}
	// Cached: no second fetch.
	if _, e := s.Artwork(ctx, "u", ids["big.png"]); e != nil || f.count("/art/big.png") != 1 {
		t.Fatalf("cached %v %d", e, f.count("/art/big.png"))
	}
	for _, name := range []string{"page.html", "image.svg", "huge.png", "to-private", "to-loopback-name", "to-other-port", "loop", "missing"} {
		c.Add(time.Second) // the image bucket refills
		if _, e := s.Artwork(ctx, "u", ids[name]); e == nil || e.Code != "artwork_unavailable" || e.Status != 502 {
			t.Fatalf("%s = %v", name, e)
		}
	}
	if otherHits.Load() != 0 || f.count("/art/big.png") != 1 {
		t.Fatal("a refused address was dialled")
	}
	before := f.count("/art/page.html")
	if _, e := s.Artwork(ctx, "u", ids["page.html"]); e == nil || f.count("/art/page.html") != before {
		t.Fatal("a failure was fetched again within a minute")
	}
	c.Add(time.Minute)
	if _, e := s.Artwork(ctx, "u", ids["page.html"]); e == nil || f.count("/art/page.html") != before+1 {
		t.Fatal("not retried after a minute")
	}
	for _, h := range headers {
		if h.Get("Authorization") != "" || h.Get("Cookie") != "" || h.Get("X-Auth-Key") != "" || h.Get("Accept") != "image/*" {
			t.Fatalf("headers %v", h)
		}
	}
	// Unknown, malformed and never-registered ids fetch nothing.
	hits := len(headers)
	for _, id := range []string{"", "abc", strings.Repeat("A", 64), strings.Repeat("0", 64), ArtworkID("http://" + f.origin().String() + "/art/unlisted.png")} {
		if _, e := s.Artwork(ctx, "u", id); e == nil || e.Code != "artwork_not_found" || e.Status != 404 {
			t.Fatalf("%q = %v", id, e)
		}
	}
	if len(headers) != hits {
		t.Fatal("an unregistered id was fetched")
	}
	// Registrations expire with the search's 15 minutes.
	c.Add(16 * time.Minute)
	if _, e := s.Artwork(ctx, "u", ids["big.png"]); e == nil || e.Code != "artwork_not_found" {
		t.Fatalf("expired = %v", e)
	}
}

// The artwork budgets: 120/minute burst 20 per user, hits included; 60/minute
// burst 4 server-wide misses; four image fetches at once.
func TestArtworkQuotas(t *testing.T) {
	f := newFake(t)
	c := &clock{t: time.Unix(1700000000, 0)}
	names := []string{}
	for i := range 6 {
		names = append(names, fmt.Sprintf("%d.png", i))
	}
	s, ids := artService(t, f, c, names...)
	img := pngBytes(t, 10, 10)
	release := make(chan struct{})
	f.setRoute(func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/art/") {
			return false
		}
		if r.URL.Path != "/art/0.png" {
			<-release
		}
		_, _ = w.Write(img)
		return true
	})
	ctx := context.Background()
	if _, e := s.Artwork(ctx, "u", ids["0.png"]); e != nil {
		t.Fatal(e)
	}
	for i := 1; i < 20; i++ {
		if _, e := s.Artwork(ctx, "u", ids["0.png"]); e != nil {
			t.Fatalf("hit %d: %v", i, e)
		}
	}
	if _, e := s.Artwork(ctx, "u", ids["0.png"]); e == nil || e.Code != "discovery_rate_limited" || e.RetryAfter != 1 {
		t.Fatalf("user = %+v", e)
	}
	// Server-wide: three more misses in flight fill the bucket's burst of 4.
	var wg sync.WaitGroup
	for _, n := range []string{"1.png", "2.png", "3.png"} {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = s.Artwork(ctx, "v"+n, ids[n]) }()
	}
	waitFor(t, func() bool { return f.count("/art/3.png")+f.count("/art/2.png")+f.count("/art/1.png") == 3 })
	if _, e := s.Artwork(ctx, "w", ids["4.png"]); e == nil || e.Code != "provider_rate_limited" || e.RetryAfter != 1 {
		t.Fatalf("global = %+v", e)
	}
	c.Add(time.Second)
	go func() { _, _ = s.Artwork(ctx, "x", ids["4.png"]) }()
	waitFor(t, func() bool { return f.count("/art/4.png") == 1 })
	c.Add(time.Second)
	if _, e := s.Artwork(ctx, "y", ids["5.png"]); e == nil || e.Code != "discovery_busy" || e.RetryAfter != 1 {
		t.Fatalf("slots = %+v", e)
	}
	close(release)
	wg.Wait()
}

// Feed and artwork URL rules: public only, the test origin's exact address
// admitted, nothing resolved.
func TestURLRules(t *testing.T) {
	origin := netip.MustParseAddrPort("127.0.0.1:9123")
	plain, test := rules{}, rules{origin: origin}
	for raw, want := range map[string]string{
		"HTTPS://Feeds.Example.org:443/one.xml#x":                "https://feeds.example.org/one.xml",
		"http://feeds.example.org/a b":                           "http://feeds.example.org/a%20b",
		"https://feeds.example.org/p?token=x":                    "",
		"https://user:pw@feeds.example.org/":                     "",
		"https://www.patreon.com/rss/x":                          "",
		"https://feeds.example.org/abcdefghijklmnopqrstuvw1234":  "",
		"file:///etc/hosts":                                      "",
		"http://127.0.0.1/feed":                                  "",
		"http://[::1]/feed":                                      "",
		"http://10.0.0.1/feed":                                   "",
		"http://169.254.169.254/feed":                            "",
		"http://localhost/feed":                                  "",
		"http://a.localhost/feed":                                "",
		"http://printer.local/feed":                              "",
		"http://93.184.216.34/feed":                              "http://93.184.216.34/feed",
		"http://127.0.0.1:9123/feed.xml":                         "",
		"https://feeds.example.org/" + strings.Repeat("a", 2100): "",
	} {
		got, ok := plain.feedURL(raw)
		if got != want || ok != (want != "") {
			t.Errorf("feedURL(%.60q) = %q %v, want %q", raw, got, ok, want)
		}
	}
	if got, ok := test.feedURL("http://127.0.0.1:9123/feed.xml"); !ok || got != "http://127.0.0.1:9123/feed.xml" {
		t.Error("test origin feed refused")
	}
	if _, ok := test.feedURL("http://127.0.0.1:9124/feed.xml"); ok {
		t.Error("another loopback port admitted")
	}
	for raw, want := range map[string]bool{
		"https://images.example.org/one.jpg":                      true,
		"https://images.example.org/a.jpg?w=600":                  true,
		"HTTP://images.example.org/a.jpg":                         true,
		"https://images.example.org/a.jpg#f":                      false,
		"https://u:p@images.example.org/a.jpg":                    false,
		"ftp://images.example.org/a.jpg":                          false,
		"//images.example.org/a.jpg":                              false,
		"/relative.jpg":                                           false,
		"http://127.0.0.1/a.jpg":                                  false,
		"http://localhost/a.jpg":                                  false,
		"https://images.example.org/" + strings.Repeat("a", 2100): false,
	} {
		if got := plain.artURL(raw); got != want {
			t.Errorf("artURL(%.60q) = %v", raw, got)
		}
	}
	if !test.artURL("http://127.0.0.1:9123/art/x.png") || test.artURL("http://127.0.0.1:80/art/x.png") {
		t.Error("test origin art")
	}
	for raw, want := range map[string]string{`12345`: "12345", `9007199254740993`: "9007199254740993", `"77"`: "77", `0`: "", `-1`: "", `1.5`: "", `1e3`: "", `"0012"`: "", `null`: "", `true`: ""} {
		if got := jsonID(json.RawMessage(raw)); got != want {
			t.Errorf("jsonID(%s) = %q", raw, got)
		}
	}
	for raw, want := range map[string]string{`0`: "0", `12`: "12", `2147483647`: "2147483647", `2147483648`: "nil", `-1`: "nil", `1.0`: "nil", `"3"`: "nil", `null`: "nil"} {
		got := "nil"
		if n := jsonCount(json.RawMessage(raw)); n != nil {
			got = fmt.Sprint(*n)
		}
		if got != want {
			t.Errorf("jsonCount(%s) = %s", raw, got)
		}
	}
	for raw, want := range map[string]string{
		"https://podcasts.apple.com/us/podcast/x/id1": "https://podcasts.apple.com/us/podcast/x/id1",
		"https://itunes.apple.com/x":                  "https://itunes.apple.com/x",
		"http://podcasts.apple.com/x":                 "",
		"https://evil.example/podcasts.apple.com":     "",
		"https://podcasts.apple.com.evil.example/":    "",
		"https://u@podcasts.apple.com/":               "",
		"https://podcasts.apple.com:8443/":            "",
	} {
		if got := appleURL(raw); got != want {
			t.Errorf("appleURL(%q) = %q", raw, got)
		}
	}
}

// Only a podcastdiscoverytest build reads the origin variable; an ordinary
// one (this test) always asks the fixed origins and its guard admits no
// loopback address.
func TestTestOriginSeam(t *testing.T) {
	if TestOriginBuild() {
		t.Skip("tagged build: see origin_tagged_test.go")
	}
	t.Setenv(TestOriginEnv, "http://127.0.0.1:9123")
	s := New(Options{})
	if s.ends.itunes != itunesSearchURL || s.ends.index != indexSearchURL || s.ends.origin.IsValid() || s.rules.origin.IsValid() {
		t.Fatalf("untagged build honoured the origin: %+v", s.ends)
	}
	if testOrigin().IsValid() {
		t.Fatal("testOrigin in an untagged build")
	}
	// The ordinary guard refuses loopback even at the variable's address.
	f := newFake(t)
	s = newService(Options{Now: time.Now}, netip.AddrPort{})
	s.ends = endpoints{itunes: f.srv.URL + "/itunes/search"}
	if _, e := s.Search(context.Background(), "1", query(t, "q=fixture")); e == nil || e.Code != "discovery_unavailable" {
		t.Fatalf("loopback provider = %v", e)
	}
	if f.count("/itunes/search") != 0 {
		t.Fatal("the guard let a loopback provider through")
	}
	for raw, ok := range map[string]bool{
		"http://127.0.0.1:9123":    true,
		"http://127.0.0.1":         false,
		"http://127.0.0.1:0":       false,
		"https://127.0.0.1:9123":   false,
		"http://localhost:9123":    false,
		"http://10.0.0.1:9123":     false,
		"http://u@127.0.0.1:9123":  false,
		"http://127.0.0.1:9123/":   false,
		"http://127.0.0.1:9123?x":  false,
		"http://127.0.0.1:9123#x":  false,
		"http://127.0.0.1:70000":   false,
		"":                         false,
		"http://[::1]:9123":        false,
		"http://127.0.0.1:9123/x/": false,
	} {
		if got := parseTestOrigin(raw).IsValid(); got != ok {
			t.Errorf("parseTestOrigin(%q) = %v", raw, got)
		}
	}
	// A provider that redirects is refused, even to the origin itself.
	f.setRoute(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/itunes/search" {
			http.Redirect(w, r, "/index/api/1.0/search/byterm", http.StatusFound)
			return true
		}
		return false
	})
	s = newTestService(t, f, &clock{t: time.Unix(1, 0)}, nil)
	if _, e := s.Search(context.Background(), "1", query(t, "q=fixture")); e == nil || e.Code != "discovery_unavailable" || f.count("/index/api/1.0/search/byterm") != 0 {
		t.Fatalf("redirect = %v", e)
	}
}
