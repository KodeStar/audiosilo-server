package meta

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockMeta is a configurable metaserve stand-in. Each endpoint counts its hits
// so tests can assert caching, and lookup/work/series responses are overridable.
type mockMeta struct {
	lookupJSON string
	lookupCode int
	workJSON   string
	workCode   int
	seriesJSON map[string]string // series id -> body
	lookupHits atomic.Int32
	workHits   atomic.Int32
	seriesHits atomic.Int32
}

func (m *mockMeta) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, _ *http.Request) {
		m.lookupHits.Add(1)
		if m.lookupCode != 0 {
			w.WriteHeader(m.lookupCode)
			return
		}
		_, _ = w.Write([]byte(m.lookupJSON))
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, _ *http.Request) {
		m.workHits.Add(1)
		if m.workCode != 0 {
			w.WriteHeader(m.workCode)
			return
		}
		_, _ = w.Write([]byte(m.workJSON))
	})
	mux.HandleFunc("GET /api/v1/series/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.seriesHits.Add(1)
		body, ok := m.seriesJSON[r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	return mux
}

const martianLookup = `{"work":{"id":"the-martian","title":"The Martian","authors":[{"id":"andy-weir","name":"Andy Weir"}],"series":null,"cover_url":null,"added_at":null},"recording_id":"rec2"}`

const martianWork = `{
  "id":"the-martian","title":"The Martian","subtitle":"A Novel",
  "authors":[{"id":"andy-weir","name":"Andy Weir"}],
  "language":"en","first_published":"2011","description":"Stranded on Mars.",
  "series":[{"id":"mars","name":"Mars","position":"1"}],
  "recordings":[
    {"id":"rec1","narrators":[{"id":"nobody","name":"Nobody"}],"abridged":false,"runtime_min":600,"release_date":"2012-01-01","publisher":"Other","cover_url":"https://c/rec1.jpg","chapter_count":10},
    {"id":"rec2","narrators":[{"id":"r-c-bray","name":"R. C. Bray"}],"abridged":false,"runtime_min":634,"release_date":"2013-03-22","publisher":"Podium Audio","cover_url":"https://c/rec2.jpg","chapter_count":12}
  ]
}`

const marsSeries = `{
  "id":"mars","name":"Mars","authors":[{"id":"andy-weir","name":"Andy Weir"}],
  "works":[
    {"position":"1","work":{"id":"the-martian","title":"The Martian","authors":[{"id":"andy-weir","name":"Andy Weir"}],"series":null,"cover_url":"https://c/tm.jpg","added_at":null}},
    {"position":"2","work":{"id":"artemis","title":"Artemis","authors":[{"id":"andy-weir","name":"Andy Weir"}],"series":null,"cover_url":null,"added_at":null}}
  ]
}`

func fullMock() *mockMeta {
	return &mockMeta{
		lookupJSON: martianLookup,
		workJSON:   martianWork,
		seriesJSON: map[string]string{"mars": marsSeries},
	}
}

func TestEnrichComposition(t *testing.T) {
	m := fullMock()
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	svc := NewService(srv.URL, nil)

	env, err := svc.Enrich(context.Background(), "B00FLIJJSY", "")
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if !env.Matched || env.Work == nil {
		t.Fatalf("expected matched work, got %+v", env)
	}
	if env.Work.ID != "the-martian" || env.Work.Title != "The Martian" || env.Work.Subtitle != "A Novel" {
		t.Fatalf("work fields wrong: %+v", env.Work)
	}
	if env.Work.Description != "Stranded on Mars." || env.Work.FirstPublished != "2011" || env.Work.Language != "en" {
		t.Fatalf("work detail fields wrong: %+v", env.Work)
	}
	if len(env.Work.Authors) != 1 || env.Work.Authors[0].Name != "Andy Weir" {
		t.Fatalf("authors wrong: %+v", env.Work.Authors)
	}
	// Recording is chosen by lookup's recording_id (rec2), not the first (rec1).
	if env.Recording == nil || env.Recording.ID != "rec2" || env.Recording.Publisher != "Podium Audio" {
		t.Fatalf("recording pick wrong: %+v", env.Recording)
	}
	if env.Recording.RuntimeMin != 634 || env.Recording.Narrators[0].Name != "R. C. Bray" {
		t.Fatalf("recording fields wrong: %+v", env.Recording)
	}
	// web_url built from base + escaped work id.
	if env.WebURL != srv.URL+"/work?id=the-martian" {
		t.Fatalf("work web_url = %q", env.WebURL)
	}
	// Series rail: full ordered list including the current work, per-entry web_url.
	if len(env.Series) != 1 {
		t.Fatalf("expected 1 series rail, got %d", len(env.Series))
	}
	rail := env.Series[0]
	if rail.ID != "mars" || rail.Position != "1" || len(rail.Works) != 2 {
		t.Fatalf("series rail wrong: %+v", rail)
	}
	if rail.Works[0].ID != "the-martian" || rail.Works[0].WebURL != srv.URL+"/work?id=the-martian" {
		t.Fatalf("series work[0] wrong: %+v", rail.Works[0])
	}
	if rail.Works[0].CoverURL != "https://c/tm.jpg" {
		t.Fatalf("series work[0] cover wrong: %+v", rail.Works[0])
	}
	if rail.Works[1].ID != "artemis" || rail.Works[1].Position != "2" || rail.Works[1].CoverURL != "" {
		t.Fatalf("series work[1] wrong: %+v", rail.Works[1])
	}
	if rail.Works[1].WebURL != srv.URL+"/work?id=artemis" {
		t.Fatalf("series work[1] web_url wrong: %+v", rail.Works[1])
	}
}

func TestEnrichRecordingFallbackToFirst(t *testing.T) {
	m := fullMock()
	// lookup returns an unknown recording_id -> fall back to the first recording.
	m.lookupJSON = `{"work":{"id":"the-martian","title":"The Martian","authors":[],"series":null,"cover_url":null,"added_at":null},"recording_id":"does-not-exist"}`
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	svc := NewService(srv.URL, nil)

	env, err := svc.Enrich(context.Background(), "B00FLIJJSY", "")
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if env.Recording == nil || env.Recording.ID != "rec1" {
		t.Fatalf("expected fallback to first recording rec1, got %+v", env.Recording)
	}
}

func TestEnrichISBNPreferredAfterASIN(t *testing.T) {
	m := fullMock()
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	svc := NewService(srv.URL, nil)

	// Only an isbn present: lookup should still resolve (query carries isbn).
	env, err := svc.Enrich(context.Background(), "", "9780804139021")
	if err != nil {
		t.Fatalf("Enrich by isbn: %v", err)
	}
	if !env.Matched {
		t.Fatalf("expected match by isbn")
	}
}

func TestEnrichNotFound(t *testing.T) {
	m := fullMock()
	m.lookupCode = http.StatusNotFound
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	svc := NewService(srv.URL, nil)

	_, err := svc.Enrich(context.Background(), "B0MISSING", "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestEnrichNoIDs(t *testing.T) {
	svc := NewService("https://meta.example", nil)
	if _, err := svc.Enrich(context.Background(), "  ", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for empty ids, got %v", err)
	}
}

func TestEnrichTransportError(t *testing.T) {
	m := fullMock()
	m.lookupCode = http.StatusInternalServerError
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	svc := NewService(srv.URL, nil)

	_, err := svc.Enrich(context.Background(), "B00FLIJJSY", "")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("expected a transport error distinct from ErrNotFound, got %v", err)
	}
}

func TestEnrichWorkInconsistencyIsError(t *testing.T) {
	m := fullMock()
	m.workCode = http.StatusNotFound // lookup found a work id the works endpoint 404s
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	svc := NewService(srv.URL, nil)

	_, err := svc.Enrich(context.Background(), "B00FLIJJSY", "")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown work id should surface as an error, not a clean not-found: %v", err)
	}
}

func TestEnrichSeriesFailureNonFatal(t *testing.T) {
	m := fullMock()
	delete(m.seriesJSON, "mars") // series fetch 404s
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	svc := NewService(srv.URL, nil)

	env, err := svc.Enrich(context.Background(), "B00FLIJJSY", "")
	if err != nil {
		t.Fatalf("a failed series rail must not fail the whole enrichment: %v", err)
	}
	if len(env.Series) != 0 {
		t.Fatalf("expected the failed rail to be skipped, got %+v", env.Series)
	}
	if env.Work == nil {
		t.Fatalf("core enrichment should still be present")
	}
}

// clock is a test-controllable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestEnrichPositiveCacheHit(t *testing.T) {
	m := fullMock()
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	svc := NewService(srv.URL, clk.now)

	if _, err := svc.Enrich(context.Background(), "B00FLIJJSY", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enrich(context.Background(), "B00FLIJJSY", ""); err != nil {
		t.Fatal(err)
	}
	// The second call is a cache hit: no additional upstream traffic.
	if got := m.lookupHits.Load(); got != 1 {
		t.Fatalf("expected 1 upstream lookup (cached), got %d", got)
	}

	// Just before the positive TTL expires it is still cached.
	clk.advance(positiveTTL - time.Minute)
	if _, err := svc.Enrich(context.Background(), "B00FLIJJSY", ""); err != nil {
		t.Fatal(err)
	}
	if got := m.lookupHits.Load(); got != 1 {
		t.Fatalf("expected still-cached before TTL, got %d lookups", got)
	}

	// Past the TTL it re-fetches.
	clk.advance(2 * time.Minute)
	if _, err := svc.Enrich(context.Background(), "B00FLIJJSY", ""); err != nil {
		t.Fatal(err)
	}
	if got := m.lookupHits.Load(); got != 2 {
		t.Fatalf("expected a re-fetch past the positive TTL, got %d lookups", got)
	}
}

func TestEnrichNotFoundCached(t *testing.T) {
	m := fullMock()
	m.lookupCode = http.StatusNotFound
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	svc := NewService(srv.URL, clk.now)

	for i := 0; i < 3; i++ {
		if _, err := svc.Enrich(context.Background(), "B0MISSING", ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("call %d: expected ErrNotFound, got %v", i, err)
		}
	}
	if got := m.lookupHits.Load(); got != 1 {
		t.Fatalf("not-found should be cached, expected 1 lookup, got %d", got)
	}
	// Past the (shorter) not-found TTL it re-checks.
	clk.advance(notFoundTTL + time.Minute)
	if _, err := svc.Enrich(context.Background(), "B0MISSING", ""); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if got := m.lookupHits.Load(); got != 2 {
		t.Fatalf("expected a re-check past the not-found TTL, got %d", got)
	}
}

// TestEnrichCallerCancelNotCached is the cache-poisoning regression: the api
// handler passes the request context, and players abort in-flight requests
// routinely (navigation cancels fetches). A failure caused by the CALLER's own
// cancelled context must NOT be cached as a transport error - otherwise one
// tap-and-back poisons that book's enrichment with 502s for the whole error TTL
// while upstream is healthy. A subsequent request with a live context must reach
// upstream again and compose normally.
func TestEnrichCallerCancelNotCached(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// First call: hold the request open until the caller cancels.
			close(started)
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte(martianLookup))
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(martianWork))
	})
	mux.HandleFunc("GET /api/v1/series/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(marsSeries))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	svc := NewService(srv.URL, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()
	if _, err := svc.Enrich(ctx, "B00FLIJJSY", ""); err == nil {
		t.Fatal("expected an error from the cancelled request")
	}

	// A fresh, live context must hit upstream again (nothing was cached) and
	// compose the full envelope.
	env, err := svc.Enrich(context.Background(), "B00FLIJJSY", "")
	if err != nil {
		t.Fatalf("post-cancel Enrich must not be poisoned by a cached error: %v", err)
	}
	if !env.Matched || env.Work == nil || env.Work.ID != "the-martian" {
		t.Fatalf("expected a full match after the cancelled attempt, got %+v", env)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected the second Enrich to reach upstream (2 lookups), got %d", got)
	}
}

func TestEnrichTransportErrorCachedBriefly(t *testing.T) {
	m := fullMock()
	m.lookupCode = http.StatusBadGateway
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	svc := NewService(srv.URL, clk.now)

	if _, err := svc.Enrich(context.Background(), "B00FLIJJSY", ""); err == nil {
		t.Fatal("expected a transport error")
	}
	if _, err := svc.Enrich(context.Background(), "B00FLIJJSY", ""); err == nil {
		t.Fatal("expected a cached transport error")
	}
	// A down upstream is not hammered: within the error TTL there is one hit.
	if got := m.lookupHits.Load(); got != 1 {
		t.Fatalf("transport error should be cached, expected 1 lookup, got %d", got)
	}
	clk.advance(errorTTL + time.Second)
	if _, err := svc.Enrich(context.Background(), "B00FLIJJSY", ""); err == nil {
		t.Fatal("expected error")
	}
	if got := m.lookupHits.Load(); got != 2 {
		t.Fatalf("expected a retry past the error TTL, got %d", got)
	}
}
