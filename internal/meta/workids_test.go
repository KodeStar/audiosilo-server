package meta

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lookupMock answers GET /lookup by identifier: asins and isbns map to work ids,
// anything else is a 404. hold, when set, keeps every lookup open until the
// request's context ends or release is closed.
type lookupMock struct {
	works   map[string]string // asin or isbn -> work id
	down    map[string]bool   // asin or isbn -> answer this one with a 500
	code    atomic.Int32      // non-zero answers every lookup with this status
	hits    atomic.Int32
	hold    bool
	release chan struct{}
	started chan struct{} // closed on the first lookup when hold is set
	once    sync.Once
}

func (m *lookupMock) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, r *http.Request) {
		m.hits.Add(1)
		if m.hold {
			m.once.Do(func() { close(m.started) })
			select {
			case <-r.Context().Done():
				return
			case <-m.release:
			}
		}
		if code := m.code.Load(); code != 0 {
			w.WriteHeader(int(code))
			return
		}
		q := r.URL.Query()
		if m.down[q.Get("asin")+q.Get("isbn")] {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		id, ok := m.works[q.Get("asin")+q.Get("isbn")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"work":{"id":"` + id + `","title":"T","authors":[]},"recording_id":"r"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// idsOf is the work ids of a WorkIDs answer, failedOf which of its books failed.
func idsOf(ws []WorkID) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.ID
	}
	return out
}

func failedOf(ws []WorkID) []bool {
	out := make([]bool, len(ws))
	for i, w := range ws {
		out[i] = w.Failed
	}
	return out
}

// TestWorkIDsResolvesInOrder: each book resolves to its own work (asin first,
// else isbn), in input order; a book without identifiers or without a match is
// "" and not failed; one identifier is looked up once.
func TestWorkIDsResolvesInOrder(t *testing.T) {
	m := &lookupMock{works: map[string]string{"B0ONE": "book-one", "B0TWO": "book-two", "9780000000002": "book-three"}}
	svc := NewService(m.server(t).URL, nil)

	ws := svc.WorkIDs(context.Background(), []BookIDs{
		{ASIN: "B0ONE"},
		{},                                       // no identifiers: never looked up
		{ASIN: " B0TWO ", ISBN: "9780000000002"}, // the asin wins, trimmed
		{ISBN: "9780000000002"},
		{ASIN: "B0ONE"}, // a second copy of the first book
		{ASIN: "B0NOPE"},
	})
	want := []string{"book-one", "", "book-two", "book-three", "book-one", ""}
	if !slices.Equal(idsOf(ws), want) || slices.Contains(failedOf(ws), true) {
		t.Fatalf("WorkIDs = %+v, want %q none failed", ws, want)
	}
	if got := m.hits.Load(); got != 4 {
		t.Fatalf("lookups = %d, want 4 (one per distinct identifier)", got)
	}

	// A second pass is all cache: hits and misses alike.
	if again := svc.WorkIDs(context.Background(), []BookIDs{{ASIN: "B0ONE"}, {ASIN: "B0NOPE"}}); !slices.Equal(again, []WorkID{{ID: "book-one"}, {}}) {
		t.Fatalf("cached WorkIDs = %+v", again)
	}
	if got := m.hits.Load(); got != 4 {
		t.Fatalf("lookups after a cached pass = %d, want 4", got)
	}
}

// TestWorkIDsReadsEnrichment: a book a player opened costs the Series card no
// lookup, since its cached enrichment carries the work id; spellings of one
// identifier share it, and a cached "no match" is shared as well. Enrich records
// no lookup entry of its own. It only flows one way: Enrich always composes from
// a fresh lookup, so a lookup the card recorded earlier never ages a player's
// enrichment.
func TestWorkIDsReadsEnrichment(t *testing.T) {
	m := fullMock()
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	svc := NewService(srv.URL, nil)

	if _, err := svc.Enrich(context.Background(), "B00FLIJJSY", ""); err != nil {
		t.Fatal(err)
	}
	if n := owned(svc.cache, nsLookup); n != 0 {
		t.Fatalf("lookup entries after Enrich = %d, want 0 (the enrichment holds the work id)", n)
	}
	if ws := svc.WorkIDs(context.Background(), []BookIDs{{ASIN: "b00flijjsy "}}); ws[0] != (WorkID{ID: "the-martian"}) {
		t.Fatalf("WorkIDs after Enrich = %+v", ws)
	}
	if got := m.lookupHits.Load(); got != 1 {
		t.Fatalf("lookups = %d, want 1 (the enrichment's own, read from its entry)", got)
	}

	// The other way round: the card first, then the player, who looks again.
	if ws := svc.WorkIDs(context.Background(), []BookIDs{{ISBN: "978-0-553-41802-6"}}); ws[0].ID != "the-martian" {
		t.Fatalf("WorkIDs by isbn = %+v", ws)
	}
	if env, err := svc.Enrich(context.Background(), "", "9780553418026"); err != nil || env.Work.ID != "the-martian" {
		t.Fatalf("Enrich after WorkIDs = %+v, %v", env, err)
	}
	if got := m.lookupHits.Load(); got != 3 {
		t.Fatalf("lookups = %d, want 3 (Enrich never reads the card's lookup)", got)
	}

	// A "no match" is shared the same way.
	m.lookupCode = http.StatusNotFound
	if _, err := svc.Enrich(context.Background(), "B0MISSING", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Enrich of a missing book = %v, want ErrNotFound", err)
	}
	if ws := svc.WorkIDs(context.Background(), []BookIDs{{ASIN: "B0MISSING"}}); ws[0] != (WorkID{}) {
		t.Fatalf("cached miss = %+v, want no work and not failed", ws)
	}
	if got := m.lookupHits.Load(); got != 4 {
		t.Fatalf("lookups after a shared miss = %d, want 4", got)
	}
	if n := owned(svc.cache, nsLookup); n != 1 {
		t.Fatalf("lookup entries = %d, want 1 (only the card's own isbn lookup)", n)
	}
}

// TestWorkIDsEnrichmentFailureLooksUp: a cached enrichment failure is no answer
// about the lookup (here the work fetch failed), so WorkIDs looks the book up
// itself rather than reporting it failed.
func TestWorkIDsEnrichmentFailureLooksUp(t *testing.T) {
	m := fullMock()
	m.workCode = http.StatusInternalServerError
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	svc := NewService(srv.URL, nil)

	if _, err := svc.Enrich(context.Background(), "B00FLIJJSY", ""); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Enrich with the work down = %v, want an upstream error", err)
	}
	if ws := svc.WorkIDs(context.Background(), []BookIDs{{ASIN: "B00FLIJJSY"}}); ws[0] != (WorkID{ID: "the-martian"}) {
		t.Fatalf("WorkIDs after a failed Enrich = %+v, want its own lookup's answer", ws)
	}
	if got := m.lookupHits.Load(); got != 2 {
		t.Fatalf("lookups = %d, want 2 (Enrich's, then the card's own)", got)
	}
}

// TestWorkIDsUpstreamErrorFailed: a failing upstream leaves the book unresolved
// and failed, and the failure is cached briefly so a page of cards doesn't
// hammer a down service.
func TestWorkIDsUpstreamErrorFailed(t *testing.T) {
	m := &lookupMock{works: map[string]string{"B0DOWN": "back"}}
	m.code.Store(http.StatusInternalServerError)
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	svc := NewService(m.server(t).URL, clk.now)

	for range 2 {
		if ws := svc.WorkIDs(context.Background(), []BookIDs{{ASIN: "B0DOWN"}}); ws[0] != (WorkID{Failed: true}) {
			t.Fatalf("upstream down = %+v, want unresolved and failed", ws)
		}
	}
	if got := m.hits.Load(); got != 1 {
		t.Fatalf("lookups = %d, want 1 (the error is cached)", got)
	}
	clk.advance(errorTTL + time.Second)
	m.code.Store(0)
	if ws := svc.WorkIDs(context.Background(), []BookIDs{{ASIN: "B0DOWN"}}); ws[0] != (WorkID{ID: "back"}) {
		t.Fatalf("after the error TTL = %+v", ws)
	}
}

// TestWorkIDsFailedPerBook: one failed lookup marks only its own books failed
// (both copies), never a resolved book or a clean "no match" in the same batch,
// cached or not.
func TestWorkIDsFailedPerBook(t *testing.T) {
	m := &lookupMock{works: map[string]string{"B0ONE": "book-one"}, down: map[string]bool{"B0DOWN": true}}
	svc := NewService(m.server(t).URL, nil)
	books := []BookIDs{{ASIN: "B0ONE"}, {ASIN: "B0NOPE"}, {ASIN: "B0DOWN"}, {}, {ASIN: "B0DOWN"}}
	want := []WorkID{{ID: "book-one"}, {}, {Failed: true}, {}, {Failed: true}}
	for _, pass := range []string{"fresh", "cached"} {
		if ws := svc.WorkIDs(context.Background(), books); !slices.Equal(ws, want) {
			t.Fatalf("%s WorkIDs = %+v, want %+v", pass, ws, want)
		}
	}
	if got := m.hits.Load(); got != 3 {
		t.Fatalf("lookups = %d, want 3 (the second pass is all cache)", got)
	}
}

// TestWorkIDsCallerCancelNotCached: a failure caused by the caller going away
// (the console left the page) is not the upstream's fault and must not poison
// the book's lookup for the error TTL.
func TestWorkIDsCallerCancelNotCached(t *testing.T) {
	m := &lookupMock{works: map[string]string{"B0ONE": "book-one"}, hold: true,
		release: make(chan struct{}), started: make(chan struct{})}
	svc := NewService(m.server(t).URL, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-m.started
		cancel()
	}()
	if ws := svc.WorkIDs(ctx, []BookIDs{{ASIN: "B0ONE"}}); ws[0] != (WorkID{Failed: true}) {
		t.Fatalf("cancelled = %+v, want unresolved and failed", ws)
	}

	close(m.release)
	if ws := svc.WorkIDs(context.Background(), []BookIDs{{ASIN: "B0ONE"}}); ws[0] != (WorkID{ID: "book-one"}) {
		t.Fatalf("after a cancelled attempt = %+v, want book-one (nothing cached)", ws)
	}
	if got := m.hits.Load(); got != 2 {
		t.Fatalf("lookups = %d, want 2", got)
	}
}

// TestWorkIDsBudgetNotCached: the batch runs under composeBudget; lookups it cuts
// short (in flight or still queued) are unresolved and failed, and not cached.
func TestWorkIDsBudgetNotCached(t *testing.T) {
	m := &lookupMock{works: map[string]string{}, hold: true,
		release: make(chan struct{}), started: make(chan struct{})}
	svc := NewService(m.server(t).URL, nil)
	svc.composeBudget = 50 * time.Millisecond

	books := make([]BookIDs, maxConcurrentLookups+3)
	for i := range books {
		id := "B0" + strconv.Itoa(i)
		books[i] = BookIDs{ASIN: id}
		m.works[id] = "w" + strconv.Itoa(i)
	}
	ws := svc.WorkIDs(context.Background(), books)
	if !slices.Equal(ws, slices.Repeat([]WorkID{{Failed: true}}, len(books))) {
		t.Fatalf("over budget = %+v, want all unresolved and failed", ws)
	}

	close(m.release)
	svc.composeBudget = composeTimeout
	ws = svc.WorkIDs(context.Background(), books)
	if slices.Contains(failedOf(ws), true) || ws[0].ID != "w0" || ws[len(ws)-1].ID != "w"+strconv.Itoa(len(ws)-1) {
		t.Fatalf("with time = %+v, want every book resolved", ws)
	}
}

// TestWorkIDsUpstreamConcurrencyBounded: uncached lookups share one bound across
// requests (a Series page asks for many cards at once). Asserts the ceiling only.
func TestWorkIDsUpstreamConcurrencyBounded(t *testing.T) {
	var inFlight, peak atomic.Int32
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, _ *http.Request) {
		n := inFlight.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		<-release
		inFlight.Add(-1)
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	svc := NewService(srv.URL, nil)

	var wg sync.WaitGroup
	for r := range 3 { // three requests, eight books each
		books := make([]BookIDs, 8)
		for i := range books {
			books[i] = BookIDs{ASIN: "B" + strconv.Itoa(r) + "-" + strconv.Itoa(i)}
		}
		wg.Go(func() { svc.WorkIDs(context.Background(), books) })
	}
	deadline := time.Now().Add(10 * time.Second)
	for peak.Load() < maxConcurrentLookups && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	got := peak.Load()
	close(release)
	wg.Wait()
	if got > maxConcurrentLookups {
		t.Fatalf("peak concurrent lookups = %d, want <= %d", got, maxConcurrentLookups)
	}
	if got == 0 {
		t.Fatal("expected at least one lookup in flight")
	}
}

// TestEnrichNotBlockedByLookupQueue: a player's Enrich never waits behind the
// console. With every lookupSem slot held (a Series page's batch), it still
// composes at once, and its cached enrichment answers WorkIDs, which needs no
// slot either.
func TestEnrichNotBlockedByLookupQueue(t *testing.T) {
	m := fullMock()
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	svc := NewService(srv.URL, nil)
	svc.composeBudget = 5 * time.Second

	for range maxConcurrentLookups {
		svc.lookupSem <- struct{}{}
	}
	defer func() {
		for range maxConcurrentLookups {
			<-svc.lookupSem
		}
	}()
	if env, err := svc.Enrich(context.Background(), "B00FLIJJSY", ""); err != nil || env.Work.ID != "the-martian" {
		t.Fatalf("Enrich behind a full lookup queue = %+v, %v", env, err)
	}
	if ws := svc.WorkIDs(context.Background(), []BookIDs{{ASIN: "B00FLIJJSY"}}); ws[0] != (WorkID{ID: "the-martian"}) {
		t.Fatalf("WorkIDs after Enrich = %+v, want the enrichment's work", ws)
	}
	if got := m.lookupHits.Load(); got != 1 {
		t.Fatalf("lookups = %d, want 1 (WorkIDs read the enrichment)", got)
	}
}

// waitFlightRefs waits until the shared lookup of asin has n callers waiting.
func waitFlightRefs(t *testing.T, svc *Service, asin string, n int) {
	t.Helper()
	key := nsLookup.key(nsASIN.key(asin))
	deadline := time.Now().Add(10 * time.Second)
	for {
		svc.flightMu.Lock()
		f := svc.flights[key]
		ok := f != nil && f.refs == n
		svc.flightMu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("shared lookup of %s never reached %d waiters", asin, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestWorkIDsConcurrentMissesShareLookup: two requests missing one identifier at
// the same time share one upstream lookup (and one lookupSem slot).
func TestWorkIDsConcurrentMissesShareLookup(t *testing.T) {
	m := &lookupMock{works: map[string]string{"B0ONE": "book-one"}, hold: true,
		release: make(chan struct{}), started: make(chan struct{})}
	svc := NewService(m.server(t).URL, nil)

	results := make(chan WorkID, 2)
	ask := func() { results <- svc.WorkIDs(context.Background(), []BookIDs{{ASIN: "B0ONE"}})[0] }
	go ask()
	<-m.started
	go ask()
	waitFlightRefs(t, svc, "B0ONE", 2)
	close(m.release)
	for range 2 {
		if w := <-results; w != (WorkID{ID: "book-one"}) {
			t.Fatalf("WorkIDs = %+v, want book-one", w)
		}
	}
	if got := m.hits.Load(); got != 1 {
		t.Fatalf("lookups = %d, want 1 (the concurrent misses shared one)", got)
	}
}

// TestWorkIDsCanceledWaiterKeepsFlight: one caller leaving a shared lookup (the
// console left the page) fails only that caller; the other still gets the answer,
// from the same upstream lookup, which is then cached.
func TestWorkIDsCanceledWaiterKeepsFlight(t *testing.T) {
	m := &lookupMock{works: map[string]string{"B0ONE": "book-one"}, hold: true,
		release: make(chan struct{}), started: make(chan struct{})}
	svc := NewService(m.server(t).URL, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	left := make(chan WorkID, 1)
	stayed := make(chan WorkID, 1)
	go func() { left <- svc.WorkIDs(ctx, []BookIDs{{ASIN: "B0ONE"}})[0] }()
	<-m.started
	go func() { stayed <- svc.WorkIDs(context.Background(), []BookIDs{{ASIN: "B0ONE"}})[0] }()
	waitFlightRefs(t, svc, "B0ONE", 2)

	cancel()
	if w := <-left; w != (WorkID{Failed: true}) {
		t.Fatalf("canceled caller = %+v, want unresolved and failed", w)
	}
	waitFlightRefs(t, svc, "B0ONE", 1)
	close(m.release)
	if w := <-stayed; w != (WorkID{ID: "book-one"}) {
		t.Fatalf("remaining caller = %+v, want book-one", w)
	}
	if ws := svc.WorkIDs(context.Background(), []BookIDs{{ASIN: "B0ONE"}}); ws[0] != (WorkID{ID: "book-one"}) {
		t.Fatalf("after the flight = %+v, want book-one cached", ws)
	}
	if got := m.hits.Load(); got != 1 {
		t.Fatalf("lookups = %d, want 1", got)
	}
}

// TestCachedWorkID: the work id for an identifier from the in-memory cache only
// - the enrichment first, then the "l:" lookup - with a cached "no match" as
// ("", true). A miss, a cached failure, a book without identifiers and a row
// only the persistent store holds all answer ("", false), and nothing ever
// reaches the upstream or the store.
func TestCachedWorkID(t *testing.T) {
	m := fullMock()
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	store := newMemStore()
	svc := NewService(srv.URL, nil)
	svc.SetStore(store)

	if id, ok := svc.CachedWorkID("B00FLIJJSY", ""); ok || id != "" {
		t.Fatalf("cold = %q, %v", id, ok)
	}
	if id, ok := svc.CachedWorkID("", ""); ok || id != "" {
		t.Fatalf("no identifiers = %q, %v", id, ok)
	}
	// An enrichment answers it, by any spelling of the identifier.
	if _, err := svc.Enrich(context.Background(), "B00FLIJJSY", ""); err != nil {
		t.Fatal(err)
	}
	if id, ok := svc.CachedWorkID(" b00flijjsy ", ""); !ok || id != "the-martian" {
		t.Fatalf("enriched = %q, %v", id, ok)
	}
	// A cached "no match" enrichment.
	svc.cache.putMiss(nsISBN.key("9780000000002"), notFoundTTL)
	if id, ok := svc.CachedWorkID("", "9780000000002"); !ok || id != "" {
		t.Fatalf("cached no match = %q, %v", id, ok)
	}
	// The console's lookup key space: an answer, and a failure (not an answer).
	cachePut(svc.cache, nsLookup.key(nsASIN.key("B0LOOKED")), &upstreamLookup{Work: &upstreamWorkCard{ID: "looked"}}, positiveTTL)
	if id, ok := svc.CachedWorkID("B0LOOKED", ""); !ok || id != "looked" {
		t.Fatalf("lookup entry = %q, %v", id, ok)
	}
	svc.cache.putError(nsLookup.key(nsASIN.key("B0FAILED")), errors.New("down"))
	if id, ok := svc.CachedWorkID("B0FAILED", ""); ok || id != "" {
		t.Fatalf("cached failure = %q, %v", id, ok)
	}
	// A row only the store holds is not consulted.
	store.put(StoredEntry{Key: nsASIN.key("B0STORED"), Version: storeVersion, Source: srv.URL,
		Payload: []byte(`{"matched":true,"work":{"id":"stored"}}`), Expires: time.Now().Add(time.Hour)})
	loads := store.loads.Load()
	if id, ok := svc.CachedWorkID("B0STORED", ""); ok || id != "" || store.loads.Load() != loads {
		t.Fatalf("store-only row = %q, %v (loads %d -> %d)", id, ok, loads, store.loads.Load())
	}
	if m.lookupHits.Load() != 1 {
		t.Fatalf("lookups = %d, want only the Enrich's", m.lookupHits.Load())
	}
}
