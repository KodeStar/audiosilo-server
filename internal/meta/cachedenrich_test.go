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

// cachedAnswerOf answers from memory (an enrichment, else a lookup) or the
// store, stale or not, never the upstream, with when the answer was had; a
// cached "no match" is an answer of no work; a cached failure and a row of
// another format are none. A fresh stored row warms memory.
func TestCachedAnswer(t *testing.T) {
	t.Parallel()
	m := fullMock()
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	store := newMemStore()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	svc := NewService(srv.URL, func() time.Time { return now })
	svc.SetStore(store)
	ctx := context.Background()

	if c := svc.cachedAnswerOf(ctx, "B00FLIJJSY", ""); c.ok || c.env != nil {
		t.Fatalf("cold = %+v", c)
	}
	if c := svc.cachedAnswerOf(ctx, "", ""); c.ok {
		t.Fatal("no identifiers answered")
	}
	if _, err := svc.Enrich(ctx, "B00FLIJJSY", ""); err != nil {
		t.Fatal(err)
	}
	if c := svc.cachedAnswerOf(ctx, " b00flijjsy ", ""); !c.ok || c.env == nil || c.id != c.env.Work.ID || c.id == "" || !c.at.Equal(now) {
		t.Fatalf("enriched = %+v", c)
	}
	svc.cache.putMiss(nsISBN.key("9780000000002"), notFoundTTL)
	if c := svc.cachedAnswerOf(ctx, "", "9780000000002"); !c.ok || c.id != "" || c.env != nil || !c.at.Equal(now) {
		t.Fatalf("cached no match = %+v", c)
	}
	svc.cache.putError(nsASIN.key("B0FAILED"), errors.New("down"))
	if c := svc.cachedAnswerOf(ctx, "B0FAILED", ""); c.ok {
		t.Fatal("a cached failure answered")
	}
	cachePut(svc.cache, nsLookup.key(nsASIN.key("B0LOOKUP")), &upstreamLookup{Work: &upstreamWorkCard{ID: "looked-up"}}, positiveTTL)
	if c := svc.cachedAnswerOf(ctx, "B0LOOKUP", ""); !c.ok || c.id != "looked-up" || c.env != nil || !c.at.Equal(now) {
		t.Fatalf("cached lookup = %+v", c)
	}
	store.put(StoredEntry{Key: nsASIN.key("B0STALE"), Version: storeVersion, Source: srv.URL,
		Payload: []byte(`{"matched":true,"work":{"id":"stale","recaps":[{"through":{"chapter":1},"text":"x"}]}}`), Expires: now.Add(-time.Hour)})
	store.put(StoredEntry{Key: nsASIN.key("B0FRESH"), Version: storeVersion, Source: srv.URL,
		Payload: []byte(`{"matched":true,"work":{"id":"fresh","characters":[{"id":"c","name":"C","reveal":{"chapter":1}}]}}`), Expires: now.Add(time.Hour)})
	store.put(StoredEntry{Key: nsASIN.key("B0NOMATCH"), Version: storeVersion, Source: srv.URL, Expires: now.Add(time.Hour)})
	store.put(StoredEntry{Key: nsASIN.key("B0OTHER"), Version: storeVersion + 1, Source: srv.URL,
		Payload: []byte(`{"matched":true,"work":{"id":"other"}}`), Expires: now.Add(time.Hour)})
	if c := svc.cachedAnswerOf(ctx, "B0STALE", ""); c.env == nil || c.id != "stale" || !coverageOf(c.env).Recaps ||
		coverageOf(c.env).Characters || !c.at.Equal(now.Add(-time.Hour-positiveTTL)) {
		t.Fatalf("stale stored row = %+v", c)
	}
	if _, hit, _ := cacheGet[Enrichment](svc.cache, nsASIN.key("B0STALE")); hit {
		t.Error("a stale row warmed memory")
	}
	if c := svc.cachedAnswerOf(ctx, "B0FRESH", ""); c.env == nil || c.id != "fresh" || !coverageOf(c.env).Characters {
		t.Fatalf("fresh stored row = %+v", c)
	}
	if e, hit, err := cacheGet[Enrichment](svc.cache, nsASIN.key("B0FRESH")); !hit || err != nil || e.Work.ID != "fresh" {
		t.Errorf("the fresh row didn't warm memory: %+v, %v, %v", e, hit, err)
	}
	if c := svc.cachedAnswerOf(ctx, "B0NOMATCH", ""); !c.ok || c.id != "" || c.env != nil {
		t.Fatalf("stored no match = %+v", c)
	}
	if c := svc.cachedAnswerOf(ctx, "B0OTHER", ""); c.ok {
		t.Fatal("a row of another format answered")
	}
	if m.lookupHits.Load() != 1 {
		t.Fatalf("lookups = %d, want only the Enrich's", m.lookupHits.Load())
	}
	if cov := coverageOf(nil); cov.Characters || cov.Recaps || cov.Description {
		t.Fatalf("a nil envelope covers %+v", cov)
	}
}

// The upcoming job's resolution and the identifiers' cached answer are of the
// same identifiers: the newer one names the work, for the estimate and the
// Bridge's book facts alike. After a restart the cache answers from its stored
// row, which can be older than the job's monthly recheck: the recheck's work
// then wins; a row stored since the recheck wins over it.
func TestCachedWorkNewestWins(t *testing.T) {
	t.Parallel()
	m := fullMock()
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	store := newMemStore()
	// The stored row, written five days ago (expired four days ago): an old
	// work, merged into the-martian upstream since.
	store.put(StoredEntry{Key: nsASIN.key("B00FLIJJSY"), Version: storeVersion, Source: srv.URL,
		Payload: []byte(`{"matched":true,"work":{"id":"old-work"}}`), Expires: now.Add(-5*24*time.Hour + positiveTTL)})
	svc := NewService(srv.URL, func() time.Time { return now }) // a restarted server: nothing in memory
	svc.SetStore(store)
	ctx := context.Background()

	recheck := CoverageBook{ASIN: "B00FLIJJSY", WorkID: "the-martian", ResolvedAt: now.Add(-24 * time.Hour)}
	if got := svc.CachedWork(ctx, recheck); got != "the-martian" {
		t.Fatalf("book facts with a newer recheck = %q, want the-martian", got)
	}
	if got := svc.Coverages(ctx, []CoverageBook{recheck})[0]; got != (Coverage{Characters: true, Recaps: true, Description: true, Known: true}) {
		t.Fatalf("estimate with a newer recheck = %+v, want the-martian's", got)
	}
	if m.workHits.Load() != 1 || m.lookupHits.Load() != 0 {
		t.Fatalf("upstream: %d works, %d lookups; want the-martian fetched by id alone", m.workHits.Load(), m.lookupHits.Load())
	}
	// A recheck before the row was stored: the row is the newer answer.
	older := CoverageBook{ASIN: "B00FLIJJSY", WorkID: "the-martian", ResolvedAt: now.Add(-10 * 24 * time.Hour)}
	if got := svc.CachedWork(ctx, older); got != "old-work" {
		t.Fatalf("book facts with an older recheck = %q, want the stored old-work", got)
	}
	if got := svc.Coverages(ctx, []CoverageBook{older})[0]; got != (Coverage{Known: true}) {
		t.Fatalf("estimate with an older recheck = %+v, want the stored envelope's (nothing)", got)
	}
	// A resolution of unknown age never wins over a cached answer.
	if got := svc.CachedWork(ctx, CoverageBook{ASIN: "B00FLIJJSY", WorkID: "the-martian"}); got != "old-work" {
		t.Fatalf("book facts with an undated resolution = %q, want the stored old-work", got)
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
