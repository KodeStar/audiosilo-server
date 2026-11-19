package meta

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

// Coverages answers each book from the bounded console path, never a series
// rail: a lookup then the work by id, cached, so asking again costs nothing; a
// book with no identifier carries nothing; a failed lookup is unknown, not none.
func TestCoverages(t *testing.T) {
	t.Parallel()
	m := fullMock()
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	svc := NewService(srv.URL, nil)
	ctx := context.Background()

	got := svc.Coverages(ctx, []CoverageBook{{ASIN: "B00FLIJJSY"}, {}})
	// The mock's work carries a core description, which counts (TestCoverageDescription).
	if want := []Coverage{{Characters: true, Recaps: true, Description: true, Known: true}, {Known: true}}; !slices.Equal(got, want) {
		t.Fatalf("Coverages = %+v, want %+v", got, want)
	}
	if m.lookupHits.Load() != 1 || m.workHits.Load() != 1 || m.seriesHits.Load() != 0 {
		t.Fatalf("upstream: %d lookups, %d works, %d series; want 1, 1, 0", m.lookupHits.Load(), m.workHits.Load(), m.seriesHits.Load())
	}
	if again := svc.Coverages(ctx, []CoverageBook{{ASIN: " b00flijjsy "}}); !again[0].Known || !again[0].Recaps ||
		m.lookupHits.Load() != 1 || m.workHits.Load() != 1 {
		t.Fatalf("asked again: %+v after %d lookups, %d works", again, m.lookupHits.Load(), m.workHits.Load())
	}

	down := &mockMeta{lookupCode: http.StatusServiceUnavailable}
	dsrv := httptest.NewServer(down.handler())
	defer dsrv.Close()
	if got := NewService(dsrv.URL, nil).Coverages(ctx, []CoverageBook{{ASIN: "B00FLIJJSY"}}); got[0] != (Coverage{}) {
		t.Fatalf("a failed lookup = %+v, want unknown", got[0])
	}
}

// CachedEnrichment answers from memory or the store, stale or not, never the
// upstream; a cached "no match", a cached failure and a row of another format
// are no answer. A fresh stored row warms memory.
func TestCachedEnrichment(t *testing.T) {
	t.Parallel()
	m := fullMock()
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	store := newMemStore()
	svc := NewService(srv.URL, nil)
	svc.SetStore(store)
	ctx := context.Background()

	if env := svc.CachedEnrichment(ctx, "B00FLIJJSY", ""); env != nil {
		t.Fatalf("cold = %+v", env)
	}
	if env := svc.CachedEnrichment(ctx, "", ""); env != nil {
		t.Fatal("no identifiers answered")
	}
	if _, err := svc.Enrich(ctx, "B00FLIJJSY", ""); err != nil {
		t.Fatal(err)
	}
	if env := svc.CachedEnrichment(ctx, " b00flijjsy ", ""); env == nil || env.Work == nil || env.Work.ID == "" {
		t.Fatalf("enriched = %+v", env)
	}
	svc.cache.putMiss(nsISBN.key("9780000000002"), notFoundTTL)
	if env := svc.CachedEnrichment(ctx, "", "9780000000002"); env != nil {
		t.Fatalf("cached no match = %+v", env)
	}
	svc.cache.putError(nsASIN.key("B0FAILED"), errors.New("down"))
	if env := svc.CachedEnrichment(ctx, "B0FAILED", ""); env != nil {
		t.Fatal("a cached failure answered")
	}
	store.put(StoredEntry{Key: nsASIN.key("B0STALE"), Version: storeVersion, Source: srv.URL,
		Payload: []byte(`{"matched":true,"work":{"id":"stale","recaps":[{"through":{"chapter":1},"text":"x"}]}}`), Expires: time.Now().Add(-time.Hour)})
	store.put(StoredEntry{Key: nsASIN.key("B0FRESH"), Version: storeVersion, Source: srv.URL,
		Payload: []byte(`{"matched":true,"work":{"id":"fresh","characters":[{"id":"c","name":"C","reveal":{"chapter":1}}]}}`), Expires: time.Now().Add(time.Hour)})
	store.put(StoredEntry{Key: nsASIN.key("B0NOMATCH"), Version: storeVersion, Source: srv.URL, Expires: time.Now().Add(time.Hour)})
	store.put(StoredEntry{Key: nsASIN.key("B0OTHER"), Version: storeVersion + 1, Source: srv.URL,
		Payload: []byte(`{"matched":true,"work":{"id":"other"}}`), Expires: time.Now().Add(time.Hour)})
	if env := svc.CachedEnrichment(ctx, "B0STALE", ""); env == nil || env.Work.ID != "stale" || !coverageOf(env).Recaps || coverageOf(env).Characters {
		t.Fatalf("stale stored row = %+v", env)
	}
	if _, hit, _ := cacheGet[Enrichment](svc.cache, nsASIN.key("B0STALE")); hit {
		t.Error("a stale row warmed memory")
	}
	if env := svc.CachedEnrichment(ctx, "B0FRESH", ""); env == nil || env.Work.ID != "fresh" || !coverageOf(env).Characters {
		t.Fatalf("fresh stored row = %+v", env)
	}
	if e, hit, err := cacheGet[Enrichment](svc.cache, nsASIN.key("B0FRESH")); !hit || err != nil || e.Work.ID != "fresh" {
		t.Errorf("the fresh row didn't warm memory: %+v, %v, %v", e, hit, err)
	}
	if env := svc.CachedEnrichment(ctx, "B0NOMATCH", ""); env != nil {
		t.Fatalf("stored no match = %+v", env)
	}
	if env := svc.CachedEnrichment(ctx, "B0OTHER", ""); env != nil {
		t.Fatal("a row of another format answered")
	}
	if m.lookupHits.Load() != 1 {
		t.Fatalf("lookups = %d, want only the Enrich's", m.lookupHits.Load())
	}
	if cov := coverageOf(nil); cov.Characters || cov.Recaps || cov.Description {
		t.Fatalf("a nil envelope covers %+v", cov)
	}
}

// A Coverage says whether the community work has a description the player shows
// before a generated one (the Bridge's estimate: a generated description is
// offered only without one): its community description or its core one.
func TestCoverageDescription(t *testing.T) {
	t.Parallel()
	with := &Enrichment{Work: &MetaWork{CommunityDescription: &MetaCommunityDescription{Text: "Community."}}}
	if c := coverageOf(with); !c.Description || !c.Known || c.Characters || c.Recaps {
		t.Errorf("with a community description = %+v", c)
	}
	if c := coverageOf(&Enrichment{Work: &MetaWork{Description: "Blurb."}}); !c.Description || !c.Known {
		t.Errorf("with a core description only = %+v", c)
	}
	if c := coverageOf(&Enrichment{Work: &MetaWork{Description: "  "}}); c.Description || !c.Known {
		t.Errorf("with a blank core description = %+v", c)
	}
}

// TestCoveragesKnownWork (server#375): a book whose work the catalog knows (a
// console match, with no ASIN or ISBN at all) is answered by that work, fetched
// by id with no lookup; a cached enrichment of its identifiers answers only
// when it is of that work.
func TestCoveragesKnownWork(t *testing.T) {
	t.Parallel()
	m := fullMock()
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	svc := NewService(srv.URL, nil)
	ctx := context.Background()

	got := svc.Coverages(ctx, []CoverageBook{{WorkID: "the-martian", Matched: true}})
	if want := (Coverage{Characters: true, Recaps: true, Description: true, Known: true}); got[0] != want {
		t.Fatalf("match-only book = %+v, want %+v", got[0], want)
	}
	if m.lookupHits.Load() != 0 || m.workHits.Load() != 1 {
		t.Fatalf("upstream: %d lookups, %d works; want 0, 1", m.lookupHits.Load(), m.workHits.Load())
	}
	// The identifiers' cached enrichment is of another work than the match:
	// the match's work answers, not the cached one (which has no recaps).
	other := &Enrichment{Matched: true, Work: &MetaWork{ID: "other", Characters: []MetaCharacter{{Name: "X"}}}}
	cachePut(svc.cache, nsASIN.key("B0OTHER"), other, positiveTTL)
	got = svc.Coverages(ctx, []CoverageBook{{ASIN: "B0OTHER", WorkID: "the-martian", Matched: true}})
	if !got[0].Recaps || !got[0].Known || m.workHits.Load() != 1 {
		t.Fatalf("rematched = %+v after %d works, want the match's work (cached)", got[0], m.workHits.Load())
	}
	// A match's work upstream no longer has leaves the identifiers' cached
	// enrichment standing, as on /meta; with none cached there is nothing.
	gone := &mockMeta{workCode: http.StatusNotFound}
	gsrv := httptest.NewServer(gone.handler())
	defer gsrv.Close()
	gsvc := NewService(gsrv.URL, nil)
	cachePut(gsvc.cache, nsASIN.key("B0OTHER"), other, positiveTTL)
	got = gsvc.Coverages(ctx, []CoverageBook{
		{ASIN: "B0OTHER", WorkID: "rematched", Matched: true},
		{WorkID: "rematched", Matched: true},
		{ASIN: "B0OTHER", WorkID: "other", Matched: true},
	})
	if want := (Coverage{Characters: true, Known: true}); got[0] != want || got[1] != (Coverage{Known: true}) || got[2] != want {
		t.Fatalf("match gone upstream = %+v, none cached = %+v, cached work = %+v", got[0], got[1], got[2])
	}
	if gone.workHits.Load() != 1 {
		t.Fatalf("%d work fetches, want 1 (one work, asked once)", gone.workHits.Load())
	}
}

// TestCoveragesResolvedWork: a work id that is the upcoming job's resolution of
// the book's identifiers (not a recorded match) answers only when those
// identifiers have no cached enrichment. With one, of another work (the
// resolution is up to a month old: the work merged away since), the cached
// enrichment answers and the stale work is never asked for, where it read as
// "no content".
func TestCoveragesResolvedWork(t *testing.T) {
	t.Parallel()
	gone := &mockMeta{workCode: http.StatusNotFound}
	srv := httptest.NewServer(gone.handler())
	defer srv.Close()
	svc := NewService(srv.URL, nil)
	ctx := context.Background()
	cachePut(svc.cache, nsASIN.key("B0MERGED"), &Enrichment{Matched: true, Work: &MetaWork{ID: "kept",
		Characters: []MetaCharacter{{Name: "X"}}}}, positiveTTL)
	got := svc.Coverages(ctx, []CoverageBook{{ASIN: "B0MERGED", WorkID: "merged-away"}})
	if want := (Coverage{Characters: true, Known: true}); got[0] != want {
		t.Fatalf("stale resolution = %+v, want the identifiers' cached enrichment %+v", got[0], want)
	}
	if gone.workHits.Load() != 0 || gone.lookupHits.Load() != 0 {
		t.Fatalf("upstream: %d works, %d lookups; want none", gone.workHits.Load(), gone.lookupHits.Load())
	}

	// Nothing cached for the identifiers: the resolved work is fetched by id.
	m := fullMock()
	fsrv := httptest.NewServer(m.handler())
	defer fsrv.Close()
	got = NewService(fsrv.URL, nil).Coverages(ctx, []CoverageBook{{ASIN: "B0NOTCACHED", WorkID: "the-martian"}})
	if !got[0].Recaps || !got[0].Known || m.workHits.Load() != 1 || m.lookupHits.Load() != 0 {
		t.Fatalf("resolved work = %+v after %d works, %d lookups", got[0], m.workHits.Load(), m.lookupHits.Load())
	}
}
