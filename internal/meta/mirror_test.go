package meta

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/query"
	"github.com/kodestar/audiosilo-meta/pkg/query/querytest"
)

// fakeMirror is a local copy for the Service tests: metaserve's own handler over
// the querytest artifact (true metaserve answers), with switches for "no usable
// copy" and "the copy answers 500".
type fakeMirror struct {
	h     http.Handler
	ready atomic.Bool
	fail  atomic.Bool
	hits  atomic.Int32
}

func newFakeMirror(t *testing.T) *fakeMirror {
	t.Helper()
	db, err := query.Open(querytest.Build(t, t.TempDir()), "data-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	m := &fakeMirror{h: query.NewHandler(func() *query.DB { return db }, query.HandlerOptions{})}
	m.ready.Store(true)
	return m
}

func (m *fakeMirror) Ready() bool { return m.ready.Load() }

func (m *fakeMirror) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.hits.Add(1)
	if m.fail.Load() {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	m.h.ServeHTTP(w, r)
}

// countingRemote is a remote metadata service that counts every request and
// answers each 500, so a test asserts it is never asked.
func countingRemote(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// In mirror mode every question the server asks is answered by the local copy
// with metaserve's own code: lookup + work + series (Enrich, by ASIN and by
// ISBN), a work by id (with the community layer), a retired slug's redirect, a
// recording's chapters, the console's match and its search fallback - and the
// remote service is never asked.
func TestMirrorAnswersLocally(t *testing.T) {
	remote, remoteHits := countingRemote(t)
	s := NewService(remote.URL, nil)
	m := newFakeMirror(t)
	s.SetMirror(m)
	ctx := context.Background()

	env, err := s.Enrich(ctx, querytest.ASIN, "")
	if err != nil {
		t.Fatalf("Enrich by ASIN: %v", err)
	}
	if env.Work.ID != querytest.ASINWork || env.Recording == nil || env.Recording.ID != querytest.ASINRecording {
		t.Fatalf("Enrich by ASIN = work %q recording %+v", env.Work.ID, env.Recording)
	}
	if env.Work.Attribution == nil || len(env.Work.Characters) == 0 || env.Work.RecapSummary == nil {
		t.Fatalf("the community layer must come through: %+v", env.Work)
	}
	if !strings.HasPrefix(env.WebURL, remote.URL+"/work?id=") {
		t.Fatalf("web_url must stay on the configured site, got %q", env.WebURL)
	}

	env, err = s.Enrich(ctx, "", querytest.ISBN)
	if err != nil {
		t.Fatalf("Enrich by ISBN: %v", err)
	}
	if env.Work.ID != querytest.ISBNWork || env.Recording == nil || env.Recording.ID != querytest.ISBNRecording {
		t.Fatalf("Enrich by ISBN = work %q recording %+v", env.Work.ID, env.Recording)
	}
	if len(env.Series) != 1 || env.Series[0].ID != querytest.SeriesID || len(env.Series[0].Works) != querytest.SeriesWorks {
		t.Fatalf("series rails = %+v", env.Series)
	}
	if last := env.Series[0].Works[querytest.SeriesWorks-1]; last.Position != "10" {
		t.Fatalf("the rail must be in numeric order, last = %+v", last)
	}

	if _, err := s.Enrich(ctx, querytest.MissingASIN, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an ASIN the copy lacks must be not found, got %v", err)
	}

	work, err := s.Work(ctx, querytest.CommunityWork)
	if err != nil || work.ID != querytest.CommunityWork || len(work.Recaps) == 0 {
		t.Fatalf("Work = %+v, %v", work, err)
	}
	retired, err := s.Work(ctx, querytest.RetiredWork)
	if err != nil || retired.ID != querytest.RetiredWorkTarget {
		t.Fatalf("a retired slug must redirect to %q locally, got %+v, %v", querytest.RetiredWorkTarget, retired, err)
	}

	chs, err := s.RecordingChapters(ctx, querytest.ASIN, "")
	if err != nil || chs.WorkID != querytest.ChapteredWork || chs.RecordingID != querytest.ChapteredRecording || len(chs.Chapters) != querytest.Chapters {
		t.Fatalf("RecordingChapters = %+v, %v", chs, err)
	}

	cands, err := s.Candidates(ctx, MatchQuery{Title: "Project Hail Mary", Author: "Andy Weir"})
	if err != nil || len(cands) == 0 || cands[0].WorkID != querytest.ASINWork || cands[0].Reasons == nil {
		t.Fatalf("Candidates (works/match) = %+v, %v", cands, err)
	}
	// The search an older metaserve falls back to, answered locally too.
	s.matchUnsupportedUntil.Store(time.Now().Add(time.Hour).UnixNano())
	cands, err = s.Candidates(ctx, MatchQuery{Text: "Way of Kings"})
	if err != nil || len(cands) == 0 || cands[0].WorkID != querytest.ISBNWork {
		t.Fatalf("Candidates (works/search) = %+v, %v", cands, err)
	}

	if h := s.Ping(); !h.Reachable || h.Error != "" {
		t.Fatalf("Ping in mirror mode = %+v, want the local copy reachable", h)
	}
	if n := remoteHits.Load(); n != 0 {
		t.Fatalf("the remote service was asked %d times in mirror mode", n)
	}
	if m.hits.Load() == 0 {
		t.Fatal("the local copy was never asked")
	}
}

// Without a usable copy the remote service answers, exactly as in remote mode,
// and Ping says so without asking it.
func TestMirrorNotReadyGoesRemote(t *testing.T) {
	mock := fullMock()
	srv := httptest.NewServer(mock.handler())
	t.Cleanup(srv.Close)
	s := NewService(srv.URL, nil)
	m := newFakeMirror(t)
	m.ready.Store(false)
	s.SetMirror(m)

	env, err := s.Enrich(context.Background(), "B00B5HZGUG", "")
	if err != nil || env.Work.ID != "the-martian" {
		t.Fatalf("Enrich = %+v, %v; want the remote service's answer", env, err)
	}
	if mock.lookupHits.Load() != 1 || m.hits.Load() != 0 {
		t.Fatalf("remote lookups %d, local requests %d; want 1 and 0", mock.lookupHits.Load(), m.hits.Load())
	}
	if h := s.Ping(); h.Reachable || h.Error == "" {
		t.Fatalf("Ping without a copy = %+v", h)
	}
	if mock.lookupHits.Load() != 1 {
		t.Fatal("Ping must not ask the remote service in mirror mode")
	}
}

// A 5xx from the copy (a query it can't run) sends that request to the remote
// service.
func TestMirrorLocalFailureGoesRemote(t *testing.T) {
	mock := fullMock()
	srv := httptest.NewServer(mock.handler())
	t.Cleanup(srv.Close)
	s := NewService(srv.URL, nil)
	m := newFakeMirror(t)
	m.fail.Store(true)
	s.SetMirror(m)

	env, err := s.Enrich(context.Background(), "B00B5HZGUG", "")
	if err != nil || env.Work.ID != "the-martian" {
		t.Fatalf("Enrich = %+v, %v; want the remote service's answer", env, err)
	}
	if mock.lookupHits.Load() != 1 || m.hits.Load() == 0 {
		t.Fatalf("remote lookups %d, local requests %d", mock.lookupHits.Load(), m.hits.Load())
	}
}

// A 404 from the copy is its answer: the remote service is not asked.
func TestMirrorLocalNotFoundIsAuthoritative(t *testing.T) {
	mock := fullMock()
	srv := httptest.NewServer(mock.handler())
	t.Cleanup(srv.Close)
	s := NewService(srv.URL, nil)
	s.SetMirror(newFakeMirror(t))

	if _, err := s.Enrich(context.Background(), querytest.MissingASIN, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Enrich = %v, want ErrNotFound", err)
	}
	if mock.lookupHits.Load() != 0 {
		t.Fatal("a local 404 must not be asked of the remote service")
	}
}

// In mirror mode a "no match" never replaces a stored positive answer (an
// enrichment or a work): the stored one is served, stale, and its row stays as
// it was. Remote mode's own replacing is covered by the store tests.
func TestMirrorNotFoundKeepsStoredAnswer(t *testing.T) {
	remote, remoteHits := countingRemote(t)
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	st := newMemStore()
	s := NewService(remote.URL, clk.now)
	s.SetStore(st)
	s.SetMirror(newFakeMirror(t))
	ctx := context.Background()

	// What an earlier answer left in the cache, long expired.
	stale := clk.now().Add(-48 * time.Hour)
	enrKey := nsASIN.key(querytest.MissingASIN)
	st.put(StoredEntry{Key: enrKey, Version: storeVersion, Source: s.baseURL, Expires: stale,
		Payload: []byte(`{"matched":true,"work":{"id":"gone-work","title":"Gone","authors":[],"language":"en"},"web_url":"x"}`)})
	workKey := nsWork.key("gone-work")
	st.put(StoredEntry{Key: workKey, Version: storeVersion, Source: s.baseURL, Expires: stale,
		Payload: []byte(`{"id":"gone-work","title":"Gone","authors":[],"language":"en"}`)})

	env, err := s.Enrich(ctx, querytest.MissingASIN, "")
	if err != nil || env.Work.ID != "gone-work" {
		t.Fatalf("Enrich = %+v, %v; want the stored answer", env, err)
	}
	work, err := s.Work(ctx, "gone-work")
	if err != nil || work.ID != "gone-work" {
		t.Fatalf("Work = %+v, %v; want the stored answer", work, err)
	}
	for _, key := range []string{enrKey, workKey} {
		if row, _ := st.row(key); row.Payload == nil || !row.Expires.Equal(stale) {
			t.Fatalf("%s's row must stay as it was, got %+v", key, row)
		}
	}
	// Held in memory for the error TTL, then the copy is asked again.
	before := remoteHits.Load()
	if env, err := s.Enrich(ctx, querytest.MissingASIN, ""); err != nil || env.Work.ID != "gone-work" {
		t.Fatalf("a second Enrich = %+v, %v", env, err)
	}
	clk.advance(errorTTL + time.Second)
	if env, err := s.Enrich(ctx, querytest.MissingASIN, ""); err != nil || env.Work.ID != "gone-work" {
		t.Fatalf("Enrich after the error TTL = %+v, %v", env, err)
	}
	if remoteHits.Load() != before || before != 0 {
		t.Fatal("the remote service must not be asked")
	}

	// Without a stored answer a miss is a miss.
	if _, err := s.Work(ctx, "never-was"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Work of an unknown id = %v, want ErrNotFound", err)
	}
}

// The in-memory response writer keeps status, headers and body, and answers too
// large to buffer, panics and 5xx all read as "the copy failed it".
func TestServeLocal(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://meta.example/api/v1/x?q=1", nil)
	resp := serveLocal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "q=1" || r.RequestURI != "/api/v1/x?q=1" {
			t.Errorf("request = %q %q", r.URL.RawQuery, r.RequestURI)
		}
		w.Header().Set("Location", "/api/v1/y")
		w.WriteHeader(http.StatusMovedPermanently)
		_, _ = w.Write([]byte(`{"redirect":"y"}`))
	}), req)
	if resp == nil || resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "/api/v1/y" {
		t.Fatalf("serveLocal = %+v", resp)
	}
	if body, _ := io.ReadAll(resp.Body); string(body) != `{"redirect":"y"}` || resp.ContentLength != int64(len(body)) {
		t.Fatalf("body = %q (%d)", body, resp.ContentLength)
	}
	failed := map[string]http.HandlerFunc{
		"5xx":   func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
		"panic": func(http.ResponseWriter, *http.Request) { panic("query broke") },
		"too large": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(make([]byte, maxLocalBody))
			_, _ = w.Write([]byte("x"))
		},
	}
	for name, h := range failed {
		if resp := serveLocal(h, req); resp != nil {
			t.Errorf("%s: serveLocal = %d, want nil", name, resp.StatusCode)
		}
	}
}
