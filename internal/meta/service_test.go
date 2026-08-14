package meta

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
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
  ],
  "recap_summary":{"in_short":"An astronaut is left behind on Mars and survives.","ending":"He is rescued by the Hermes crew."}
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
	// The whole-book recap summary is additive enrichment of the current work.
	if env.Work.RecapSummary == nil {
		t.Fatalf("expected a recap_summary on the enriched work: %+v", env.Work)
	}
	if env.Work.RecapSummary.InShort == "" || env.Work.RecapSummary.Ending == "" {
		t.Fatalf("recap_summary fields wrong: %+v", env.Work.RecapSummary)
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

// ---- Service.Work (work-id addressed "catch me up" lookup) ------------------

func TestWorkComposition(t *testing.T) {
	m := fullMock()
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	svc := NewService(srv.URL, nil)

	work, err := svc.Work(context.Background(), "the-martian")
	if err != nil {
		t.Fatalf("Work: %v", err)
	}
	if work.ID != "the-martian" || work.Title != "The Martian" || work.Subtitle != "A Novel" {
		t.Fatalf("work fields wrong: %+v", work)
	}
	if work.Language != "en" || work.FirstPublished != "2011" || work.Description != "Stranded on Mars." {
		t.Fatalf("work detail fields wrong: %+v", work)
	}
	if len(work.Authors) != 1 || work.Authors[0].Name != "Andy Weir" {
		t.Fatalf("authors wrong: %+v", work.Authors)
	}
	// The expressive layer is the whole point of this endpoint.
	if len(work.Characters) != 2 || work.Characters[0].ID != "mark-watney" || work.Characters[0].Reveal.Chapter != 1 {
		t.Fatalf("characters wrong: %+v", work.Characters)
	}
	if len(work.Recaps) != 2 || work.Recaps[0].Through.Chapter != 3 || work.Recaps[1].Through.Chapter != 8 {
		t.Fatalf("recaps wrong: %+v", work.Recaps)
	}
	if work.RecapSummary == nil {
		t.Fatalf("expected a recap_summary: %+v", work)
	}
	if work.RecapSummary.InShort != "An astronaut is left behind on Mars and survives." {
		t.Fatalf("recap_summary in_short wrong: %+v", work.RecapSummary)
	}
	if work.RecapSummary.Ending != "He is rescued by the Hermes crew." {
		t.Fatalf("recap_summary ending wrong: %+v", work.RecapSummary)
	}
	// No lookup or series fan-out: a work-id read is exactly one upstream GET.
	if got := m.workHits.Load(); got != 1 {
		t.Fatalf("expected 1 work fetch, got %d", got)
	}
	if m.lookupHits.Load() != 0 || m.seriesHits.Load() != 0 {
		t.Fatalf("Work must not fan out: lookups=%d series=%d", m.lookupHits.Load(), m.seriesHits.Load())
	}
}

// TestWorkRecapSummaryOptional: an older payload with no recap_summary (and one
// with a present-but-blank object) leaves the field nil, so it is omitted from
// the wire envelope entirely.
func TestWorkRecapSummaryOptional(t *testing.T) {
	for name, body := range map[string]string{
		"absent": `{"id":"bare","title":"Bare","authors":[],"language":"en","series":[],"recordings":[]}`,
		"blank":  `{"id":"bare","title":"Bare","authors":[],"language":"en","series":[],"recordings":[],"recap_summary":{"in_short":"","ending":""}}`,
	} {
		t.Run(name, func(t *testing.T) {
			m := fullMock()
			m.workJSON = body
			srv := httptest.NewServer(m.handler())
			defer srv.Close()
			svc := NewService(srv.URL, nil)

			work, err := svc.Work(context.Background(), "bare")
			if err != nil {
				t.Fatalf("Work: %v", err)
			}
			if work.RecapSummary != nil {
				t.Fatalf("expected no recap_summary, got %+v", work.RecapSummary)
			}
		})
	}
}

// TestWorkIDEscapedUpstream: work ids are slugs that may carry characters that
// are not path-segment safe; they must reach upstream intact.
func TestWorkIDEscapedUpstream(t *testing.T) {
	var got atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.PathValue("id"))
		_, _ = w.Write([]byte(`{"id":"odd","title":"Odd","authors":[],"language":"en","series":[],"recordings":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	svc := NewService(srv.URL, nil)

	const id = "a work/with odd?bits"
	if _, err := svc.Work(context.Background(), id); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if got.Load() != id {
		t.Fatalf("upstream saw id %q, want %q", got.Load(), id)
	}
}

func TestWorkEmptyID(t *testing.T) {
	svc := NewService("https://meta.example", nil)
	if _, err := svc.Work(context.Background(), "   "); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for a blank id, got %v", err)
	}
}

// TestWorkWrongShapedOKIsNotFound is the "blank positive card" regression. A
// work id can collide with a LITERAL upstream route that outranks works/{id} in
// Go's ServeMux (metaserve serves GET /api/v1/works/latest, returning a
// {"works":[...]} collection), and getJSON decodes leniently - so that 200 comes
// back as a zero-valued detail with a nil error. It must read as not-found (and
// be cached as one), never as a positive empty work cached for 24h: the client
// contract is that any failure renders as "unavailable", not as a blank card.
func TestWorkWrongShapedOKIsNotFound(t *testing.T) {
	var hits atomic.Int32
	mux := http.NewServeMux()
	// The literal route wins over works/{id} for id="latest", exactly as upstream.
	mux.HandleFunc("GET /api/v1/works/latest", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"works":[{"id":"the-martian","title":"The Martian"}],"next_cursor":""}`))
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(martianWork))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	svc := NewService(srv.URL, clk.now)

	work, err := svc.Work(context.Background(), "latest")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong-shaped 200 = %+v / %v, want ErrNotFound", work, err)
	}
	if work != nil {
		t.Fatalf("no empty work may be returned, got %+v", work)
	}
	// It is cached as a MISS (notFoundTTL), not as a positive result: still
	// not-found inside the not-found TTL, with no second upstream hit...
	if _, err := svc.Work(context.Background(), "latest"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cached wrong-shaped 200 = %v, want ErrNotFound", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected the miss to be cached (1 fetch), got %d", got)
	}
	// ...and re-checked after an hour, not held for the 24h positive TTL.
	clk.advance(notFoundTTL + time.Minute)
	if _, err := svc.Work(context.Background(), "latest"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("expected a re-check past the not-found TTL, got %d fetches", got)
	}

	// A well-shaped id is unaffected.
	if w, err := svc.Work(context.Background(), "the-martian"); err != nil || w.ID != "the-martian" {
		t.Fatalf("normal work lookup = %+v / %v", w, err)
	}
}

// TestWorkUpstreamConcurrencyBounded: each uncached work id is one outbound GET
// to the SHARED community metadata service, so the fetches are capped (cache
// hits bypass the semaphore entirely). Asserts the ceiling only - never an exact
// count - so it cannot flake on scheduling.
func TestWorkUpstreamConcurrencyBounded(t *testing.T) {
	var inFlight, peak atomic.Int32
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, _ *http.Request) {
		n := inFlight.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		<-release // hold every request open until the whole burst has queued
		inFlight.Add(-1)
		_, _ = w.Write([]byte(`{"id":"x","title":"X","authors":[],"language":"en","series":[],"recordings":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	svc := NewService(srv.URL, nil)

	const burst = 24
	var wg sync.WaitGroup
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = svc.Work(context.Background(), "id-"+strconv.Itoa(i))
		}(i)
	}
	// Wait for the burst to saturate the semaphore rather than sleeping a fixed
	// amount (no timing assumption: the ceiling can never be exceeded, so the
	// poll simply stops as soon as it is reached, and a slow machine only takes
	// longer to get there).
	deadline := time.Now().Add(10 * time.Second)
	for peak.Load() < maxConcurrentWorkFetches && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	got := peak.Load()
	close(release)
	wg.Wait()

	if got > maxConcurrentWorkFetches {
		t.Fatalf("peak concurrent upstream work fetches = %d, want <= %d", got, maxConcurrentWorkFetches)
	}
	if got == 0 {
		t.Fatal("expected at least one upstream fetch to be in flight")
	}
}

func TestWorkPositiveCacheHit(t *testing.T) {
	m := fullMock()
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	svc := NewService(srv.URL, clk.now)

	for i := 0; i < 3; i++ {
		if _, err := svc.Work(context.Background(), "the-martian"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := m.workHits.Load(); got != 1 {
		t.Fatalf("expected 1 upstream work fetch (cached), got %d", got)
	}
	// Still cached just inside the positive TTL.
	clk.advance(positiveTTL - time.Minute)
	if _, err := svc.Work(context.Background(), "the-martian"); err != nil {
		t.Fatal(err)
	}
	if got := m.workHits.Load(); got != 1 {
		t.Fatalf("expected still-cached before TTL, got %d fetches", got)
	}
	// Past it, re-fetch.
	clk.advance(2 * time.Minute)
	if _, err := svc.Work(context.Background(), "the-martian"); err != nil {
		t.Fatal(err)
	}
	if got := m.workHits.Load(); got != 2 {
		t.Fatalf("expected a re-fetch past the positive TTL, got %d", got)
	}
}

func TestWorkNotFoundCached(t *testing.T) {
	m := fullMock()
	m.workCode = http.StatusNotFound
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	svc := NewService(srv.URL, clk.now)

	for i := 0; i < 3; i++ {
		if _, err := svc.Work(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("call %d: expected ErrNotFound, got %v", i, err)
		}
	}
	if got := m.workHits.Load(); got != 1 {
		t.Fatalf("not-found should be cached, expected 1 fetch, got %d", got)
	}
	clk.advance(notFoundTTL + time.Minute)
	if _, err := svc.Work(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if got := m.workHits.Load(); got != 2 {
		t.Fatalf("expected a re-check past the not-found TTL, got %d", got)
	}
}

func TestWorkTransportErrorCachedBriefly(t *testing.T) {
	m := fullMock()
	m.workCode = http.StatusInternalServerError
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	svc := NewService(srv.URL, clk.now)

	for i := 0; i < 2; i++ {
		_, err := svc.Work(context.Background(), "the-martian")
		if err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("call %d: expected a transport error distinct from ErrNotFound, got %v", i, err)
		}
	}
	if got := m.workHits.Load(); got != 1 {
		t.Fatalf("a down upstream should not be hammered, expected 1 fetch, got %d", got)
	}
	clk.advance(errorTTL + time.Second)
	if _, err := svc.Work(context.Background(), "the-martian"); err == nil {
		t.Fatal("expected error")
	}
	if got := m.workHits.Load(); got != 2 {
		t.Fatalf("expected a retry past the error TTL, got %d", got)
	}
}

// TestWorkCacheKeyspaceDistinct: Work and Enrich share one bounded cache, so
// their key spaces must not collide - enriching a book must not satisfy (or be
// satisfied by) a work-id read, even for the same work.
func TestWorkCacheKeyspaceDistinct(t *testing.T) {
	m := fullMock()
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	svc := NewService(srv.URL, nil)

	if _, err := svc.Enrich(context.Background(), "the-martian", ""); err != nil {
		t.Fatal(err)
	}
	if got := m.workHits.Load(); got != 1 {
		t.Fatalf("setup: expected 1 work fetch from Enrich, got %d", got)
	}
	// Same work id as the asin string above: the "w:" key space keeps them apart.
	work, err := svc.Work(context.Background(), "the-martian")
	if err != nil {
		t.Fatalf("Work: %v", err)
	}
	if work.ID != "the-martian" {
		t.Fatalf("work wrong: %+v", work)
	}
	if got := m.workHits.Load(); got != 2 {
		t.Fatalf("Work must not read Enrich's cache entry, expected 2 fetches, got %d", got)
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
