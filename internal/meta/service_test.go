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
  ],
  "characters":[
    {"id":"mark-watney","name":"Mark Watney","aliases":["Watney"],"role":"protagonist","reveal":{"chapter":1},"description":"An astronaut-botanist stranded alone on Mars."},
    {"id":"mission-control","name":"Venkat Kapoor","role":"supporting","reveal":{"chapter":6},"description":"A NASA director coordinating the rescue."}
  ],
  "recaps":[
    {"through":{"chapter":3},"scope":"book","text":"Watney is stranded and takes stock."},
    {"through":{"chapter":8},"scope":"book","text":"NASA realizes he is alive."}
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
	// Characters flow through in upstream order, with reveal position + aliases.
	if len(env.Work.Characters) != 2 {
		t.Fatalf("expected 2 characters, got %d: %+v", len(env.Work.Characters), env.Work.Characters)
	}
	c0 := env.Work.Characters[0]
	if c0.ID != "mark-watney" || c0.Name != "Mark Watney" || c0.Role != "protagonist" || c0.Reveal.Chapter != 1 {
		t.Fatalf("character[0] wrong: %+v", c0)
	}
	if len(c0.Aliases) != 1 || c0.Aliases[0] != "Watney" || c0.Description == "" {
		t.Fatalf("character[0] aliases/desc wrong: %+v", c0)
	}
	if env.Work.Characters[1].Reveal.Chapter != 6 {
		t.Fatalf("character[1] reveal wrong: %+v", env.Work.Characters[1])
	}
	// Recaps flow through in upstream (position) order with scope + through.
	if len(env.Work.Recaps) != 2 {
		t.Fatalf("expected 2 recaps, got %d: %+v", len(env.Work.Recaps), env.Work.Recaps)
	}
	if env.Work.Recaps[0].Through.Chapter != 3 || env.Work.Recaps[0].Scope != "book" || env.Work.Recaps[0].Text == "" {
		t.Fatalf("recap[0] wrong: %+v", env.Work.Recaps[0])
	}
	if env.Work.Recaps[1].Through.Chapter != 8 {
		t.Fatalf("recap[1] wrong: %+v", env.Work.Recaps[1])
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

// TestEnrichPartialSeriesShortCached: a transient series-rail failure must not
// be cached positive for the full 24h - the partial envelope is held only for
// the short errorTTL, so "more in this series" reappears within minutes of the
// blip clearing rather than a day later.
func TestEnrichPartialSeriesShortCached(t *testing.T) {
	var seriesFail atomic.Bool
	seriesFail.Store(true)
	var lookupHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, _ *http.Request) {
		lookupHits.Add(1)
		_, _ = w.Write([]byte(martianLookup))
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(martianWork))
	})
	mux.HandleFunc("GET /api/v1/series/{id}", func(w http.ResponseWriter, _ *http.Request) {
		if seriesFail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(marsSeries))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	svc := NewService(srv.URL, clk.now)

	// First call: the series endpoint is down, the envelope is usable but the
	// rail is missing.
	env, err := svc.Enrich(context.Background(), "B00FLIJJSY", "")
	if err != nil {
		t.Fatalf("partial Enrich should still succeed: %v", err)
	}
	if len(env.Series) != 0 {
		t.Fatalf("expected the failed rail to be missing, got %+v", env.Series)
	}
	// The partial result IS cached (briefly): an immediate retry is a hit.
	if _, err := svc.Enrich(context.Background(), "B00FLIJJSY", ""); err != nil {
		t.Fatal(err)
	}
	if got := lookupHits.Load(); got != 1 {
		t.Fatalf("partial result should be short-cached, expected 1 lookup, got %d", got)
	}

	// Past the SHORT (error) TTL - far inside the 24h positive TTL - the blip
	// has cleared and a re-fetch restores the full rails.
	seriesFail.Store(false)
	clk.advance(errorTTL + time.Second)
	env, err = svc.Enrich(context.Background(), "B00FLIJJSY", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := lookupHits.Load(); got != 2 {
		t.Fatalf("expected a re-fetch past the short partial TTL, got %d lookups", got)
	}
	if len(env.Series) != 1 || len(env.Series[0].Works) != 2 {
		t.Fatalf("expected the full series rail after recovery, got %+v", env.Series)
	}
}

// TestEnrichSeriesAttemptsBounded: the series fan-out is bounded by ATTEMPTS,
// not successes - a work carrying more series refs than maxSeriesRails with a
// failing series endpoint issues exactly maxSeriesRails series requests, never
// one per ref.
func TestEnrichSeriesAttemptsBounded(t *testing.T) {
	manySeriesWork := `{
	  "id":"odd","title":"Odd","subtitle":"",
	  "authors":[{"id":"a","name":"A"}],
	  "language":"en","first_published":"","description":"",
	  "series":[
	    {"id":"s1","name":"S1","position":"1"},
	    {"id":"s2","name":"S2","position":"1"},
	    {"id":"s3","name":"S3","position":"1"},
	    {"id":"s4","name":"S4","position":"1"},
	    {"id":"s5","name":"S5","position":"1"},
	    {"id":"s6","name":"S6","position":"1"}
	  ],
	  "recordings":[]
	}`
	var seriesHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"work":{"id":"odd","title":"Odd","authors":[],"cover_url":null},"recording_id":""}`))
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(manySeriesWork))
	})
	mux.HandleFunc("GET /api/v1/series/{id}", func(w http.ResponseWriter, _ *http.Request) {
		seriesHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	svc := NewService(srv.URL, nil)

	if _, err := svc.Enrich(context.Background(), "B0ODD", ""); err != nil {
		t.Fatalf("series failures are non-fatal: %v", err)
	}
	if got := seriesHits.Load(); got != maxSeriesRails {
		t.Fatalf("series attempts = %d, want exactly %d (bounded by attempts, not successes)", got, maxSeriesRails)
	}
}

// TestEnrichComposeDeadlineCached: the compose fan-out has its own deadline, and
// hitting it while the CALLER is still live is a genuine upstream-degradation
// signal - the failure must be cached (for errorTTL) so a slow-but-alive
// upstream is not re-hammered on every cold book.
func TestEnrichComposeDeadlineCached(t *testing.T) {
	var lookupHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, r *http.Request) {
		lookupHits.Add(1)
		// Block past the (shrunk) compose budget; released when the client's
		// derived deadline cancels the request.
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	svc := NewService(srv.URL, nil)
	svc.composeBudget = 50 * time.Millisecond // test-scoped; production default is composeTimeout

	if _, err := svc.Enrich(context.Background(), "B00FLIJJSY", ""); err == nil {
		t.Fatal("expected the compose deadline to surface as an error")
	}
	// The parent context was live, so the deadline failure WAS cached: a second
	// call within errorTTL never reaches upstream.
	if _, err := svc.Enrich(context.Background(), "B00FLIJJSY", ""); err == nil {
		t.Fatal("expected the cached deadline error")
	}
	if got := lookupHits.Load(); got != 1 {
		t.Fatalf("deadline failure should be cached, expected 1 lookup, got %d", got)
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
