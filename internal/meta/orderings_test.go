package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The ordering-family fixtures: a Narnia-like franchise whose primary series
// states the publication order and whose variant (ordering_of: narnia) states
// the chronological one. The Lion, the Witch and the Wardrobe is #1 in
// publication order and #2 chronologically, behind The Magician's Nephew - the
// shape that made a second, chronological rail list a LATER book as a previous
// one. "A Narnia Novella" sits in the chronological order alone.

const narniaLookup = `{"work":{"id":"lww","title":"The Lion, the Witch and the Wardrobe","authors":[],"cover_url":null},"recording_id":""}`

const lwwWork = `{
  "id":"lww","title":"The Lion, the Witch and the Wardrobe","authors":[{"id":"c-s-lewis","name":"C. S. Lewis"}],
  "language":"en",
  "series":[
    {"id":"narnia","name":"The Chronicles of Narnia","position":"1"},
    {"id":"narnia-chronological","name":"The Chronicles of Narnia (Chronological)","position":"2","ordering_of":"narnia"}
  ],
  "recordings":[]
}`

const novellaWork = `{
  "id":"novella","title":"A Narnia Novella","authors":[{"id":"c-s-lewis","name":"C. S. Lewis"}],
  "language":"en",
  "series":[
    {"id":"narnia-chronological","name":"The Chronicles of Narnia (Chronological)","position":"2.5","ordering_of":"narnia"}
  ],
  "recordings":[]
}`

const narniaFamily = `[
    {"id":"narnia","name":"The Chronicles of Narnia","ordering":"publication"},
    {"id":"narnia-chronological","name":"The Chronicles of Narnia (Chronological)","ordering":"chronological"}
  ]`

const narniaSeries = `{
  "id":"narnia","name":"The Chronicles of Narnia","language":"en","ordering":"publication",
  "authors":[{"id":"c-s-lewis","name":"C. S. Lewis"}],
  "works":[
    {"position":"1","work":{"id":"lww","title":"The Lion, the Witch and the Wardrobe","authors":[{"id":"c-s-lewis","name":"C. S. Lewis"}],"cover_url":null}},
    {"position":"6","work":{"id":"mn","title":"The Magician's Nephew","authors":[{"id":"c-s-lewis","name":"C. S. Lewis"}],"cover_url":null}}
  ],
  "works_total":2,"limit":0,"offset":0,
  "orderings":` + narniaFamily + `
}`

const narniaChronoSeries = `{
  "id":"narnia-chronological","name":"The Chronicles of Narnia (Chronological)","language":"en",
  "ordering":"chronological","ordering_of":"narnia",
  "authors":[{"id":"c-s-lewis","name":"C. S. Lewis"}],
  "works":[
    {"position":"1","work":{"id":"mn","title":"The Magician's Nephew","authors":[{"id":"c-s-lewis","name":"C. S. Lewis"}],"cover_url":null}},
    {"position":"2","work":{"id":"lww","title":"The Lion, the Witch and the Wardrobe","authors":[{"id":"c-s-lewis","name":"C. S. Lewis"}],"cover_url":null}},
    {"position":"2.5","work":{"id":"novella","title":"A Narnia Novella","authors":[{"id":"c-s-lewis","name":"C. S. Lewis"}],"cover_url":null}}
  ],
  "works_total":3,"limit":0,"offset":0,
  "orderings":` + narniaFamily + `
}`

// familyMock serves one work document and a set of series bodies, counting the
// series requests per id. A series id in failing answers 500 while fail is set.
type familyMock struct {
	work    string
	series  map[string]string
	failing map[string]bool
	fail    atomic.Bool
	lookups atomic.Int32
	mu      sync.Mutex
	hits    map[string]int
}

func newFamilyMock(work string) *familyMock {
	return &familyMock{
		work:   work,
		series: map[string]string{"narnia": narniaSeries, "narnia-chronological": narniaChronoSeries},
		hits:   map[string]int{},
	}
}

func (m *familyMock) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, _ *http.Request) {
		m.lookups.Add(1)
		_, _ = w.Write([]byte(narniaLookup))
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(m.work))
	})
	mux.HandleFunc("GET /api/v1/series/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		m.mu.Lock()
		m.hits[id]++
		m.mu.Unlock()
		if m.fail.Load() && m.failing[id] {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		body, ok := m.series[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	return mux
}

func (m *familyMock) totalHits() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.hits {
		n += c
	}
	return n
}

func (m *familyMock) hitsFor(id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits[id]
}

func enrichWith(t *testing.T, m *familyMock, now func() time.Time) (*Service, *Enrichment, string) {
	t.Helper()
	srv := httptest.NewServer(m.handler())
	t.Cleanup(srv.Close)
	svc := NewService(srv.URL, now)
	env, err := svc.Enrich(context.Background(), "B0NARNIA", "")
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	return svc, env, srv.URL
}

// railIDs renders a rail's works as "position:id" so an order reads at a glance.
func railIDs(works []MetaSeriesWork) string {
	parts := make([]string, 0, len(works))
	for _, w := range works {
		parts = append(parts, w.Position+":"+w.ID)
	}
	return strings.Join(parts, " ")
}

// TestEnrichOrderingFamilyCollapses: a work in a primary series AND its variant
// reading order gets ONE rail, whose top-level view is the primary (what a
// shipped player renders, so it never sees the chronological order's earlier
// books as "previous"), with the variant attached as an alternate carrying the
// work's position in it.
func TestEnrichOrderingFamilyCollapses(t *testing.T) {
	m := newFamilyMock(lwwWork)
	_, env, _ := enrichWith(t, m, nil)

	if len(env.Series) != 1 {
		t.Fatalf("a family must collapse to one rail, got %d: %+v", len(env.Series), env.Series)
	}
	rail := env.Series[0]
	if rail.ID != "narnia" || rail.Position != "1" || rail.Ordering != "publication" || rail.OrderingOf != "" {
		t.Fatalf("main view must be the primary: %+v", rail)
	}
	if got := railIDs(rail.Works); got != "1:lww 6:mn" {
		t.Fatalf("main view works = %q, want the publication order", got)
	}
	if len(rail.Orderings) != 1 {
		t.Fatalf("expected the chronological alternate, got %+v", rail.Orderings)
	}
	alt := rail.Orderings[0]
	if alt.ID != "narnia-chronological" || alt.Name != "The Chronicles of Narnia (Chronological)" ||
		alt.Ordering != "chronological" || alt.OrderingOf != "narnia" || alt.Position != "2" {
		t.Fatalf("alternate wrong: %+v", alt)
	}
	if got := railIDs(alt.Works); got != "1:mn 2:lww 2.5:novella" {
		t.Fatalf("alternate works = %q, want the chronological order", got)
	}
	if !strings.HasSuffix(alt.Works[0].WebURL, "/work?id=mn") {
		t.Fatalf("alternate works carry their web_url: %+v", alt.Works[0])
	}
	// The primary is fetched once (not again as an alternate of itself), the
	// variant once as the alternate.
	if m.hitsFor("narnia") != 1 || m.hitsFor("narnia-chronological") != 1 {
		t.Fatalf("series fetches = %v, want one each", m.hits)
	}
}

// TestEnrichVariantOnlyWorkKeepsItsVariant: a work only the variant places keeps
// the variant as its rail's main view (the only order that places it), with the
// primary as an alternate whose position is EMPTY - the work is not in it.
func TestEnrichVariantOnlyWorkKeepsItsVariant(t *testing.T) {
	m := newFamilyMock(novellaWork)
	_, env, _ := enrichWith(t, m, nil)

	if len(env.Series) != 1 {
		t.Fatalf("expected one rail, got %+v", env.Series)
	}
	rail := env.Series[0]
	if rail.ID != "narnia-chronological" || rail.Position != "2.5" ||
		rail.Ordering != "chronological" || rail.OrderingOf != "narnia" {
		t.Fatalf("main view must be the variant that places the work: %+v", rail)
	}
	if len(rail.Orderings) != 1 {
		t.Fatalf("expected the primary as an alternate, got %+v", rail.Orderings)
	}
	alt := rail.Orderings[0]
	if alt.ID != "narnia" || alt.Ordering != "publication" || alt.OrderingOf != "" || alt.Position != "" {
		t.Fatalf("primary alternate wrong (position must be empty): %+v", alt)
	}
	if got := railIDs(alt.Works); got != "1:lww 6:mn" {
		t.Fatalf("primary alternate works = %q", got)
	}
	// The empty position is OMITTED on the wire, not sent as "".
	b, _ := json.Marshal(alt)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["position"]; ok {
		t.Fatalf("an absent position must be omitted: %s", b)
	}
}

// TestEnrichOrderingCapCountsFamilies: maxSeriesRails counts FAMILIES, so a work
// in four families of two orders each gets three rails (each with its
// alternate), and the fourth family is never fetched at all.
func TestEnrichOrderingCapCountsFamilies(t *testing.T) {
	var refs []string
	m := &familyMock{series: map[string]string{}, hits: map[string]int{}}
	for i := 1; i <= 4; i++ {
		p, v := fmt.Sprintf("p%d", i), fmt.Sprintf("p%d-chrono", i)
		refs = append(refs,
			fmt.Sprintf(`{"id":%q,"name":%q,"position":"1"}`, p, p),
			fmt.Sprintf(`{"id":%q,"name":%q,"position":"1","ordering_of":%q}`, v, v, p))
		family := fmt.Sprintf(`[{"id":%q,"name":%q},{"id":%q,"name":%q,"ordering":"chronological"}]`, p, p, v, v)
		m.series[p] = fmt.Sprintf(`{"id":%q,"name":%q,"authors":[],"works":[],"orderings":%s}`, p, p, family)
		m.series[v] = fmt.Sprintf(`{"id":%q,"name":%q,"ordering_of":%q,"authors":[],"works":[],"orderings":%s}`, v, v, p, family)
	}
	m.work = `{"id":"lww","title":"T","authors":[],"language":"en","series":[` + strings.Join(refs, ",") + `],"recordings":[]}`
	_, env, _ := enrichWith(t, m, nil)

	if len(env.Series) != maxSeriesRails {
		t.Fatalf("rails = %d, want %d (one per family)", len(env.Series), maxSeriesRails)
	}
	for i, rail := range env.Series {
		if want := fmt.Sprintf("p%d", i+1); rail.ID != want || len(rail.Orderings) != 1 || rail.Orderings[0].ID != want+"-chrono" {
			t.Fatalf("rail %d = %+v, want %s with its variant", i, rail, want)
		}
	}
	if m.hitsFor("p4") != 0 || m.hitsFor("p4-chrono") != 0 {
		t.Fatalf("the family past the cap must not be fetched: %v", m.hits)
	}
}

// TestEnrichOrderingAlternatesBounded: a family claiming many orders still costs
// at most 1 + maxOrderingAlternates series GETs, counted as attempts, so a
// failing alternate endpoint cannot widen the fan-out.
func TestEnrichOrderingAlternatesBounded(t *testing.T) {
	family := `[{"id":"narnia","name":"N"},{"id":"v1","name":"V1"},{"id":"v2","name":"V2"},{"id":"v3","name":"V3"},{"id":"v4","name":"V4"}]`
	m := newFamilyMock(`{"id":"lww","title":"T","authors":[],"language":"en","series":[{"id":"narnia","name":"N","position":"1"}],"recordings":[]}`)
	m.series = map[string]string{"narnia": `{"id":"narnia","name":"N","authors":[],"works":[],"orderings":` + family + `}`}
	// v1..v4 are not served: every alternate fetch 404s.
	_, env, _ := enrichWith(t, m, nil)

	if got, want := m.totalHits(), 1+maxOrderingAlternates; got != want {
		t.Fatalf("series GETs = %d, want %d: %v", got, want, m.hits)
	}
	if m.hitsFor("v3") != 0 || m.hitsFor("v4") != 0 {
		t.Fatalf("alternates past the cap must not be fetched: %v", m.hits)
	}
	if len(env.Series) != 1 || len(env.Series[0].Orderings) != 0 {
		t.Fatalf("failed alternates are dropped, the rail still ships: %+v", env.Series)
	}
}

// TestEnrichOrderingAlternateFailureShortCached: a failed alternate does not
// fail the enrichment or drop the rail, but it marks the envelope partial, so it
// is cached only for errorTTL and the alternate reappears soon after the blip.
func TestEnrichOrderingAlternateFailureShortCached(t *testing.T) {
	m := newFamilyMock(lwwWork)
	m.failing = map[string]bool{"narnia-chronological": true}
	m.fail.Store(true)
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	svc, env, _ := enrichWith(t, m, clk.now)

	if len(env.Series) != 1 || env.Series[0].ID != "narnia" || len(env.Series[0].Works) != 2 {
		t.Fatalf("the rail must ship without the failed alternate: %+v", env.Series)
	}
	if len(env.Series[0].Orderings) != 0 {
		t.Fatalf("the failed alternate must be left out: %+v", env.Series[0].Orderings)
	}
	// Short-cached: an immediate retry is a hit.
	if _, err := svc.Enrich(context.Background(), "B0NARNIA", ""); err != nil {
		t.Fatal(err)
	}
	if got := m.lookups.Load(); got != 1 {
		t.Fatalf("partial envelope should be cached briefly, lookups = %d", got)
	}
	// Past errorTTL (far inside the positive TTL) the alternate is restored.
	m.fail.Store(false)
	clk.advance(errorTTL + time.Second)
	env, err := svc.Enrich(context.Background(), "B0NARNIA", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Series) != 1 || len(env.Series[0].Orderings) != 1 {
		t.Fatalf("expected the alternate after recovery: %+v", env.Series)
	}
	if got := m.lookups.Load(); got != 2 {
		t.Fatalf("expected a re-fetch past errorTTL, lookups = %d", got)
	}
}

// TestEnrichPreV7RailsUnchanged pins the wire bytes of the series rails against
// an upstream that sends NO ordering fields (a metaserve serving an artifact
// older than schema_version 7): every ref is its own family, so the two Narnia
// series stay two rails and nothing new appears in the envelope - exactly what
// the server produced before orderings existed.
func TestEnrichPreV7RailsUnchanged(t *testing.T) {
	strip := func(s string) string {
		s = strings.ReplaceAll(s, `"ordering":"chronological","ordering_of":"narnia",`, "")
		s = strings.ReplaceAll(s, `,"ordering_of":"narnia"`, "")
		s = strings.ReplaceAll(s, `"ordering":"publication",`, "")
		s = strings.ReplaceAll(s, `,
  "orderings":`+narniaFamily, "")
		if strings.Contains(s, "ordering") {
			t.Fatalf("fixture still carries an ordering field: %s", s)
		}
		return s
	}
	m := newFamilyMock(strip(lwwWork))
	m.series = map[string]string{"narnia": strip(narniaSeries), "narnia-chronological": strip(narniaChronoSeries)}
	_, env, base := enrichWith(t, m, nil)

	got, err := json.Marshal(env.Series)
	if err != nil {
		t.Fatal(err)
	}
	want := `[` +
		`{"id":"narnia","name":"The Chronicles of Narnia","position":"1","works":[` +
		`{"id":"lww","title":"The Lion, the Witch and the Wardrobe","position":"1","authors":[{"id":"c-s-lewis","name":"C. S. Lewis"}],"web_url":"BASE/work?id=lww"},` +
		`{"id":"mn","title":"The Magician's Nephew","position":"6","authors":[{"id":"c-s-lewis","name":"C. S. Lewis"}],"web_url":"BASE/work?id=mn"}]},` +
		`{"id":"narnia-chronological","name":"The Chronicles of Narnia (Chronological)","position":"2","works":[` +
		`{"id":"mn","title":"The Magician's Nephew","position":"1","authors":[{"id":"c-s-lewis","name":"C. S. Lewis"}],"web_url":"BASE/work?id=mn"},` +
		`{"id":"lww","title":"The Lion, the Witch and the Wardrobe","position":"2","authors":[{"id":"c-s-lewis","name":"C. S. Lewis"}],"web_url":"BASE/work?id=lww"},` +
		`{"id":"novella","title":"A Narnia Novella","position":"2.5","authors":[{"id":"c-s-lewis","name":"C. S. Lewis"}],"web_url":"BASE/work?id=novella"}]}` +
		`]`
	if g := strings.ReplaceAll(string(got), base, "BASE"); g != want {
		t.Fatalf("pre-v7 rails changed:\n got %s\nwant %s", g, want)
	}
}
