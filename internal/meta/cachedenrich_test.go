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

	got := svc.Coverages(ctx, []BookIDs{{ASIN: "B00FLIJJSY"}, {}})
	// The mock's work carries a core description, which counts (TestCoverageDescription).
	if want := []Coverage{{Characters: true, Recaps: true, Description: true, Known: true}, {Known: true}}; !slices.Equal(got, want) {
		t.Fatalf("Coverages = %+v, want %+v", got, want)
	}
	if m.lookupHits.Load() != 1 || m.workHits.Load() != 1 || m.seriesHits.Load() != 0 {
		t.Fatalf("upstream: %d lookups, %d works, %d series; want 1, 1, 0", m.lookupHits.Load(), m.workHits.Load(), m.seriesHits.Load())
	}
	if again := svc.Coverages(ctx, []BookIDs{{ASIN: " b00flijjsy "}}); !again[0].Known || !again[0].Recaps ||
		m.lookupHits.Load() != 1 || m.workHits.Load() != 1 {
		t.Fatalf("asked again: %+v after %d lookups, %d works", again, m.lookupHits.Load(), m.workHits.Load())
	}

	down := &mockMeta{lookupCode: http.StatusServiceUnavailable}
	dsrv := httptest.NewServer(down.handler())
	defer dsrv.Close()
	if got := NewService(dsrv.URL, nil).Coverages(ctx, []BookIDs{{ASIN: "B00FLIJJSY"}}); got[0] != (Coverage{}) {
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
