package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// escape url-escapes a ?path= value (a query param, matching the other tests).
func escape(s string) string { return url.QueryEscape(s) }

// mockMetaserve is a minimal metaserve stand-in for the /meta handler tests.
type mockMetaserve struct {
	lookupCode int // non-zero overrides the lookup response status
	workCode   int // non-zero overrides the works/{id} response status
	// match serves works/match (a current metaserve); without it the route
	// falls through to works/{id} and 404s, as on a metaserve that predates it.
	match bool
	// chapters, when set, is the body of the recordings' chapters route
	// (without it the route 404s, as on a metaserve that predates it).
	chapters string
	// series, when set, is the body of series/{id} (else the two-book Mars rail).
	series   string
	mu       sync.Mutex
	gotMatch url.Values
}

func (m *mockMetaserve) handler() http.Handler {
	mux := http.NewServeMux()
	if m.match {
		mux.HandleFunc("GET /api/v1/works/match", func(w http.ResponseWriter, r *http.Request) {
			m.mu.Lock()
			m.gotMatch = r.URL.Query()
			m.mu.Unlock()
			_, _ = w.Write([]byte(`{"results":[{"kind":"work","id":"the-martian","title":"The Martian","cover_url":"https://c/w.jpg","score":88,"reasons":{"title":1,"author":"full"}}]}`))
		})
	}
	if m.chapters != "" {
		mux.HandleFunc("GET /api/v1/works/{id}/recordings/{rid}/chapters", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(m.chapters))
		})
	}
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, _ *http.Request) {
		if m.lookupCode != 0 {
			w.WriteHeader(m.lookupCode)
			return
		}
		_, _ = w.Write([]byte(`{"work":{"id":"the-martian","title":"The Martian","authors":[{"id":"andy-weir","name":"Andy Weir"}],"series":null,"cover_url":null,"added_at":null},"recording_id":"rec1"}`))
	})
	// Only "the-martian" exists upstream; any other id 404s, so the work-id
	// handler's unknown-work path is exercised with a real upstream 404.
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, r *http.Request) {
		if m.workCode != 0 {
			w.WriteHeader(m.workCode)
			return
		}
		if r.PathValue("id") != "the-martian" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"id":"the-martian","title":"The Martian","subtitle":"","authors":[{"id":"andy-weir","name":"Andy Weir"}],"language":"en","first_published":"2011","description":"Stranded.","series":[{"id":"mars","name":"Mars","position":"1"}],"recordings":[{"id":"rec1","narrators":[{"id":"r-c-bray","name":"R. C. Bray"}],"abridged":false,"runtime_min":634,"asin":[{"region":"us","asin":"B00B5HZGUG"},{"region":"uk","asin":"B00B5HZGUG"}],"isbn":["9780553418026"],"release_date":"2013-03-22","publisher":"Podium Audio","cover_url":"https://c/1.jpg","chapter_count":12}],"characters":[{"id":"mark-watney","name":"Mark Watney","role":"protagonist","reveal":{"chapter":1},"description":"Stranded astronaut."}],"recaps":[{"through":{"chapter":3},"scope":"book","text":"Watney takes stock."}],"recap_summary":{"in_short":"Left behind on Mars.","ending":"Rescued by the Hermes crew."}}`))
	})
	// Search knows one work; a "down" query is an outage, "nothing" no hits.
	mux.HandleFunc("GET /api/v1/works/search", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("q") {
		case "down":
			w.WriteHeader(http.StatusInternalServerError)
		case "nothing":
			_, _ = w.Write([]byte(`{"results":[]}`))
		default:
			_, _ = w.Write([]byte(`{"results":[{"kind":"work","id":"the-martian","title":"The Martian","cover_url":"https://c/w.jpg"}]}`))
		}
	})
	mux.HandleFunc("GET /api/v1/series/{id}", func(w http.ResponseWriter, _ *http.Request) {
		if m.series != "" {
			_, _ = w.Write([]byte(m.series))
			return
		}
		_, _ = w.Write([]byte(`{"id":"mars","name":"Mars","authors":[{"id":"andy-weir","name":"Andy Weir"}],"works":[{"position":"1","work":{"id":"the-martian","title":"The Martian","authors":[{"id":"andy-weir","name":"Andy Weir"}],"series":null,"cover_url":null,"added_at":null}},{"position":"2","work":{"id":"artemis","title":"Artemis","authors":[{"id":"andy-weir","name":"Andy Weir"}],"series":null,"cover_url":null,"added_at":null}}]}`))
	})
	return mux
}

// newMetaEnv builds a test env whose metadata service points at a fresh mock
// metaserve (torn down with the test). enabled=false disables the feature.
func newMetaEnv(t *testing.T, enabled bool, lookupCode int) *testEnv {
	t.Helper()
	return newMetaEnvMock(t, enabled, &mockMetaserve{lookupCode: lookupCode})
}

// newMetaEnvMock is newMetaEnv with a fully configured mock (so a test can fail
// the works endpoint, not just the lookup).
func newMetaEnvMock(t *testing.T, enabled bool, m *mockMetaserve) *testEnv {
	t.Helper()
	mock := httptest.NewServer(m.handler())
	t.Cleanup(mock.Close)
	return newTestEnvWith(t, func(c *config.Config) {
		c.Metadata.Enabled = enabled
		c.Metadata.BaseURL = mock.URL
	})
}

// seedBook upserts a book at path with the given asin, returning the library id.
func seedBook(t *testing.T, e *testEnv, path, asin string) int64 {
	t.Helper()
	lib, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	book := &catalog.Book{LibraryID: lib.ID, RelPath: path, Title: "Book", Author: "Author", ASIN: asin, AddedAt: "2020-01-01"}
	if _, err := e.cat.UpsertBook(context.Background(), book); err != nil {
		t.Fatal(err)
	}
	return lib.ID
}

func TestMetaMatch(t *testing.T) {
	e := newMetaEnv(t, true, 0)
	libID := seedBook(t, e, "Andy Weir/The Martian", "B00FLIJJSY")
	adminTok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)

	path := "/api/v1/libraries/" + strconv.FormatInt(libID, 10) + "/meta?path=" + escape("Andy Weir/The Martian")
	resp, body := e.do(t, "GET", path, adminTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("meta match = %d %s, want 200", resp.StatusCode, body)
	}
	for _, want := range []string{`"matched":true`, `"the-martian"`, `"R. C. Bray"`, `"Podium Audio"`, `/work?id=the-martian`, `"artemis"`, `"mark-watney"`, `"characters"`, `"recaps"`, `"reveal":{"chapter":1}`, `"recap_summary"`, `"in_short":"Left behind on Mars."`} {
		if !strings.Contains(body, want) {
			t.Fatalf("meta envelope missing %q: %s", want, body)
		}
	}

	// The capability is advertised when the service is configured.
	_, si := e.do(t, "GET", "/api/v1/server", "", "")
	if !strings.Contains(si, `"metadata":true`) {
		t.Fatalf("expected metadata capability true: %s", si)
	}
}

func TestMetaNoIDs(t *testing.T) {
	e := newMetaEnv(t, true, 0)
	libID := seedBook(t, e, "Author/No IDs", "") // neither asin nor isbn
	adminTok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)

	path := "/api/v1/libraries/" + strconv.FormatInt(libID, 10) + "/meta?path=" + escape("Author/No IDs")
	resp, body := e.do(t, "GET", path, adminTok, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"matched":false`) {
		t.Fatalf("no-ids meta = %d %s, want 200 matched:false", resp.StatusCode, body)
	}
}

func TestMetaUpstreamNotFound(t *testing.T) {
	e := newMetaEnv(t, true, http.StatusNotFound)
	libID := seedBook(t, e, "Author/Unknown", "B0UNKNOWN")
	adminTok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)

	path := "/api/v1/libraries/" + strconv.FormatInt(libID, 10) + "/meta?path=" + escape("Author/Unknown")
	resp, body := e.do(t, "GET", path, adminTok, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"matched":false`) {
		t.Fatalf("upstream 404 meta = %d %s, want 200 matched:false", resp.StatusCode, body)
	}
}

func TestMetaUpstreamDown(t *testing.T) {
	e := newMetaEnv(t, true, http.StatusInternalServerError)
	libID := seedBook(t, e, "Author/Book", "B0DOWN")
	adminTok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)

	path := "/api/v1/libraries/" + strconv.FormatInt(libID, 10) + "/meta?path=" + escape("Author/Book")
	resp, body := e.do(t, "GET", path, adminTok, "")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("upstream down meta = %d %s, want 502", resp.StatusCode, body)
	}
}

func TestMetaDisabled(t *testing.T) {
	e := newMetaEnv(t, false, 0)
	libID := seedBook(t, e, "Author/Book", "B0OFF")
	adminTok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)

	path := "/api/v1/libraries/" + strconv.FormatInt(libID, 10) + "/meta?path=" + escape("Author/Book")
	if resp, body := e.do(t, "GET", path, adminTok, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled meta = %d %s, want 404", resp.StatusCode, body)
	}
	// The capability reflects the disabled service.
	if _, si := e.do(t, "GET", "/api/v1/server", "", ""); !strings.Contains(si, `"metadata":false`) {
		t.Fatalf("expected metadata capability false: %s", si)
	}
}

// TestMetaScopeSecurity is the required allowed+denied pair: a scoped non-admin
// may probe a book inside their grant but must be refused (403) for a path
// outside it, exactly like the other content handlers.
func TestMetaScopeSecurity(t *testing.T) {
	e := newMetaEnv(t, true, 0)
	// Two books under distinct top-level folders in one library.
	lib, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []*catalog.Book{
		{LibraryID: lib.ID, RelPath: "Andy Weir/The Martian", Title: "Book", Author: "Author", ASIN: "B00FLIJJSY", AddedAt: "2020-01-01"},
		{LibraryID: lib.ID, RelPath: "Other Author/Secret", Title: "Secret", Author: "Other", ASIN: "B0SECRET", AddedAt: "2020-01-01"},
	} {
		if _, err := e.cat.UpsertBook(context.Background(), b); err != nil {
			t.Fatal(err)
		}
	}

	// A non-admin granted only the "Andy Weir" subtree.
	kid, _ := e.auth.CreateUser(context.Background(), "kid", "kid-password", auth.RoleUser)
	share, _ := e.cat.CreateShare(context.Background(), catalog.Share{Name: "Weir only"})
	e.cat.AddSharePath(context.Background(), share.ID, catalog.PathRule{LibraryID: lib.ID, Path: "Andy Weir"})
	e.cat.GrantShare(context.Background(), kid.ID, share.ID)
	token, _ := e.auth.IssueToken(context.Background(), kid.ID, auth.KindSession, "t", 0)
	libPath := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10)

	// Allowed: a book inside the grant resolves against the mock.
	in := libPath + "/meta?path=" + escape("Andy Weir/The Martian")
	if resp, body := e.do(t, "GET", in, token, ""); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"matched":true`) {
		t.Fatalf("in-scope meta = %d %s, want 200 matched", resp.StatusCode, body)
	}

	// Denied: a path outside the grant must be refused (403), never probed.
	out := libPath + "/meta?path=" + escape("Other Author/Secret")
	if resp, body := e.do(t, "GET", out, token, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("out-of-scope meta = %d %s, want 403", resp.StatusCode, body)
	}
}

// ---- GET /meta/work (work-id addressed community lookup) --------------------

const metaWorkPath = "/api/v1/meta/work?id="

// TestMetaWorkAuth is the required allowed+denied pair for the new route: it
// carries no library scope (global read-only community data), so authentication
// itself is the gate - a signed-in user gets the work, an unauthenticated
// caller is refused before any upstream call.
func TestMetaWorkAuth(t *testing.T) {
	e := newMetaEnv(t, true, 0)
	adminTok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)

	// Allowed: a signed-in user gets the full work document.
	resp, body := e.do(t, "GET", metaWorkPath+escape("the-martian"), adminTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("meta work = %d %s, want 200", resp.StatusCode, body)
	}
	for _, want := range []string{`"work"`, `"the-martian"`, `"Andy Weir"`, `"mark-watney"`, `"recaps"`, `"recap_summary"`, `"in_short":"Left behind on Mars."`, `"ending":"Rescued by the Hermes crew."`} {
		if !strings.Contains(body, want) {
			t.Fatalf("meta work payload missing %q: %s", want, body)
		}
	}

	// Denied: no bearer token at all.
	if resp, body := e.do(t, "GET", metaWorkPath+escape("the-martian"), "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated meta work = %d %s, want 401", resp.StatusCode, body)
	}
	// Denied: a bogus bearer token.
	if resp, body := e.do(t, "GET", metaWorkPath+escape("the-martian"), "not-a-real-token", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad-token meta work = %d %s, want 401", resp.StatusCode, body)
	}
}

// TestMetaWorkScopedUserAllowed: the route is deliberately NOT library-scoped -
// a share-scoped non-admin may read any community work (it discloses nothing
// about this server's content), matching how the data is public upstream.
func TestMetaWorkScopedUserAllowed(t *testing.T) {
	e := newMetaEnv(t, true, 0)
	kid, err := e.auth.CreateUser(context.Background(), "kid", "kid-password", auth.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := e.auth.IssueToken(context.Background(), kid.ID, auth.KindSession, "t", 0)

	resp, body := e.do(t, "GET", metaWorkPath+escape("the-martian"), token, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"the-martian"`) {
		t.Fatalf("scoped-user meta work = %d %s, want 200", resp.StatusCode, body)
	}
}

func TestMetaWorkMissingID(t *testing.T) {
	e := newMetaEnv(t, true, 0)
	adminTok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)

	for _, q := range []string{"", escape("   ")} {
		resp, body := e.do(t, "GET", metaWorkPath+q, adminTok, "")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("meta work id=%q = %d %s, want 400", q, resp.StatusCode, body)
		}
	}
}

// TestMetaWorkMalformedID: the work id is the first externally-chosen value that
// becomes a cache key AND a log field, so the handler bounds its shape before
// the service ever sees it. An oversized id (Go accepts a ~1MB request line)
// would otherwise burn a cache entry and an outbound upstream GET each; a
// control character would reach the log line verbatim.
func TestMetaWorkMalformedID(t *testing.T) {
	e := newMetaEnv(t, true, 0)
	adminTok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)

	for name, id := range map[string]string{
		"oversized": strings.Repeat("a", 201),
		"huge":      strings.Repeat("a", 100_000),
		"newline":   "the-martian\nfake log line",
		"nul":       "the-\x00martian",
	} {
		t.Run(name, func(t *testing.T) {
			resp, body := e.do(t, "GET", metaWorkPath+escape(id), adminTok, "")
			if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "invalid id") {
				t.Fatalf("malformed id = %d %s, want 400 invalid id", resp.StatusCode, body)
			}
		})
	}

	// The bound is generous: an id at the limit is still accepted (it reaches
	// upstream and 404s there, rather than being rejected as malformed).
	atLimit := strings.Repeat("a", 200)
	if resp, body := e.do(t, "GET", metaWorkPath+escape(atLimit), adminTok, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("id at the length limit = %d %s, want 404 (accepted, unknown upstream)", resp.StatusCode, body)
	}
}

func TestMetaWorkUnknownID(t *testing.T) {
	e := newMetaEnv(t, true, 0)
	adminTok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)

	resp, body := e.do(t, "GET", metaWorkPath+escape("no-such-work"), adminTok, "")
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, `"error"`) {
		t.Fatalf("unknown work = %d %s, want 404 {error}", resp.StatusCode, body)
	}
}

func TestMetaWorkUpstreamDown(t *testing.T) {
	e := newMetaEnvMock(t, true, &mockMetaserve{workCode: http.StatusInternalServerError})
	adminTok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)

	resp, body := e.do(t, "GET", metaWorkPath+escape("the-martian"), adminTok, "")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("upstream-down meta work = %d %s, want 502", resp.StatusCode, body)
	}
}

func TestMetaWorkDisabled(t *testing.T) {
	e := newMetaEnv(t, false, 0)
	adminTok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)

	resp, body := e.do(t, "GET", metaWorkPath+escape("the-martian"), adminTok, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled meta work = %d %s, want 404", resp.StatusCode, body)
	}
}

// TestMetaSeriesOrderingEnvelope pins the WIRE shape of a rail for a work in an
// ordering family (a primary series plus its chronological variant), end to end
// through the handler: ONE rail whose top-level view is the primary, so a
// shipped player that ignores `orderings` renders the publication order alone,
// and the variant as an additive alternate carrying the work's position in it.
func TestMetaSeriesOrderingEnvelope(t *testing.T) {
	const family = `[{"id":"narnia","name":"Narnia","ordering":"publication"},{"id":"narnia-chrono","name":"Narnia (Chronological)","ordering":"chronological"}]`
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"work":{"id":"lww","title":"LWW","authors":[],"cover_url":null},"recording_id":""}`))
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"lww","title":"LWW","authors":[],"language":"en","series":[{"id":"narnia","name":"Narnia","position":"1"},{"id":"narnia-chrono","name":"Narnia (Chronological)","position":"2","ordering_of":"narnia"}],"recordings":[]}`))
	})
	mux.HandleFunc("GET /api/v1/series/{id}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("id") {
		case "narnia":
			_, _ = w.Write([]byte(`{"id":"narnia","name":"Narnia","ordering":"publication","authors":[],"works":[{"position":"1","work":{"id":"lww","title":"LWW","authors":[],"cover_url":null}},{"position":"6","work":{"id":"mn","title":"MN","authors":[],"cover_url":"https://c/mn.jpg"}}],"orderings":` + family + `}`))
		case "narnia-chrono":
			_, _ = w.Write([]byte(`{"id":"narnia-chrono","name":"Narnia (Chronological)","ordering":"chronological","ordering_of":"narnia","authors":[],"works":[{"position":"1","work":{"id":"mn","title":"MN","authors":[],"cover_url":"https://c/mn.jpg"}},{"position":"2","work":{"id":"lww","title":"LWW","authors":[],"cover_url":null}}],"orderings":` + family + `}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mock := httptest.NewServer(mux)
	t.Cleanup(mock.Close)
	e := newTestEnvWith(t, func(c *config.Config) {
		c.Metadata.Enabled = true
		c.Metadata.BaseURL = mock.URL
	})
	libID := seedBook(t, e, "C. S. Lewis/LWW", "B0LWW")
	adminTok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)

	path := "/api/v1/libraries/" + strconv.FormatInt(libID, 10) + "/meta?path=" + escape("C. S. Lewis/LWW")
	resp, body := e.do(t, "GET", path, adminTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("meta = %d %s, want 200", resp.StatusCode, body)
	}
	var env struct {
		Series json.RawMessage `json:"series"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode envelope: %v: %s", err, body)
	}
	// The requested book holds its own work's entry in both views (`local`).
	local := `"local":{"library_id":` + strconv.FormatInt(libID, 10) + `,"path":"C. S. Lewis/LWW"}`
	want := `[{"id":"narnia","name":"Narnia","position":"1","works":[` +
		`{"id":"lww","title":"LWW","position":"1","authors":[],"web_url":"BASE/work?id=lww",` + local + `},` +
		`{"id":"mn","title":"MN","position":"6","authors":[],"cover_url":"https://c/mn.jpg","web_url":"BASE/work?id=mn"}],` +
		`"ordering":"publication",` +
		`"orderings":[{"id":"narnia-chrono","name":"Narnia (Chronological)","ordering":"chronological","ordering_of":"narnia","position":"2","works":[` +
		`{"id":"mn","title":"MN","position":"1","authors":[],"cover_url":"https://c/mn.jpg","web_url":"BASE/work?id=mn"},` +
		`{"id":"lww","title":"LWW","position":"2","authors":[],"web_url":"BASE/work?id=lww",` + local + `}]}]}]`
	if got := strings.ReplaceAll(string(env.Series), mock.URL, "BASE"); got != want {
		t.Fatalf("series envelope:\n got %s\nwant %s", got, want)
	}
}

// ---- the /meta bundle: include=previous, spoilers=hide ----------------------

// bundleEnv serves a three-book series upstream - the requested book is book
// two - with a book 1.5 that 404s (left out of previous) and a book three after
// it (never previous). Book two's cast is revealed at chapters 1, 3 and 5.
func bundleEnv(t *testing.T) (*testEnv, string) {
	t.Helper()
	works := map[string]string{
		"book-one": `{"id":"book-one","title":"One","authors":[],"language":"en","series":[{"id":"s","name":"S","position":"1"}],` +
			`"characters":[{"id":"hero","name":"Hero","reveal":{"chapter":1}}],"recap_summary":{"in_short":"One in short.","ending":"One ends."}}`,
		"book-two": `{"id":"book-two","title":"Two","authors":[],"language":"en","series":[{"id":"s","name":"S","position":"2"}],` +
			`"recordings":[{"id":"r2","narrators":[],"chapter_count":5}],` +
			`"characters":[{"id":"early","name":"Early","reveal":{"chapter":1}},{"id":"middle","name":"Middle","reveal":{"chapter":3}},{"id":"late","name":"Late","reveal":{"chapter":5}}],` +
			`"recaps":[{"through":{"chapter":0},"text":"Before."},{"through":{"chapter":2},"text":"Through two."}],` +
			`"recap_summary":{"in_short":"Two in short.","ending":"Two ends."}}`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"work":{"id":"book-two","title":"Two","authors":[]},"recording_id":"r2"}`))
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, r *http.Request) {
		body, ok := works[r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("GET /api/v1/series/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"s","name":"S","authors":[],"works":[` +
			`{"position":"1","work":{"id":"book-one","title":"One","authors":[]}},` +
			`{"position":"1.5","work":{"id":"book-gone","title":"Gone","authors":[]}},` +
			`{"position":"2","work":{"id":"book-two","title":"Two","authors":[]}},` +
			`{"position":"3","work":{"id":"book-three","title":"Three","authors":[]}}]}`))
	})
	mock := httptest.NewServer(mux)
	t.Cleanup(mock.Close)
	e := newTestEnvWith(t, func(c *config.Config) {
		c.Metadata.Enabled = true
		c.Metadata.BaseURL = mock.URL
	})
	return e, mock.URL
}

// seedChapteredBook seeds a five-chapter book (chapters start every 100s) and
// returns its library.
func seedChapteredBook(t *testing.T, e *testEnv, path, asin string) *catalog.Library {
	t.Helper()
	ctx := context.Background()
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	book := &catalog.Book{LibraryID: lib.ID, RelPath: path, Title: "Two", Author: "A", ASIN: asin, AddedAt: "2020-01-01"}
	for i := range 5 {
		book.Chapters = append(book.Chapters, metadata.Chapter{Index: i, Title: "Ch", FilePath: path, Start: float64(i * 100), End: float64(i*100 + 100), BookOffset: float64(i * 100)})
	}
	if _, err := e.cat.UpsertBook(ctx, book); err != nil {
		t.Fatal(err)
	}
	return lib
}

// bundleBody is the slice of the envelope the bundle tests read.
type bundleBody struct {
	Matched bool `json:"matched"`
	Work    struct {
		ID         string `json:"id"`
		Characters []struct {
			ID string `json:"id"`
		} `json:"characters"`
		Recaps []struct {
			Text string `json:"text"`
		} `json:"recaps"`
		RecapSummary *struct {
			InShort string `json:"in_short"`
			Ending  string `json:"ending"`
		} `json:"recap_summary"`
		Attribution *struct {
			Credit    string `json:"credit"`
			SourceURL string `json:"source_url"`
		} `json:"attribution"`
	} `json:"work"`
	Recording struct {
		ChapterCount int `json:"chapter_count"`
	} `json:"recording"`
	Previous []struct {
		ID           string `json:"id"`
		RecapSummary *struct {
			InShort string `json:"in_short"`
			Ending  string `json:"ending"`
		} `json:"recap_summary"`
	} `json:"previous"`
}

func getBundle(t *testing.T, e *testEnv, path, token string) bundleBody {
	t.Helper()
	resp, body := e.do(t, "GET", path, token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, resp.StatusCode, body)
	}
	var b bundleBody
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	return b
}

func characterIDsOf(b bundleBody) []string {
	var ids []string
	for _, c := range b.Work.Characters {
		ids = append(ids, c.ID)
	}
	return ids
}

// TestMetaIncludePrevious: include=previous adds the works before this one,
// nearest first, a failed one left out; without it (or with an unknown value)
// the envelope is unchanged, and the cached envelope never carries a previous
// list into a later plain request.
func TestMetaIncludePrevious(t *testing.T) {
	e, base := bundleEnv(t)
	lib := seedChapteredBook(t, e, "A/Two", "B0TWO")
	adminTok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	path := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10) + "/meta?path=" + escape("A/Two")

	b := getBundle(t, e, path+"&include=previous", adminTok)
	if len(b.Previous) != 1 || b.Previous[0].ID != "book-one" {
		t.Fatalf("previous = %+v, want [book-one] (book-gone 404s, book-three is later)", b.Previous)
	}
	// Without spoilers=hide a previous work is whole.
	if s := b.Previous[0].RecapSummary; s == nil || s.Ending != "One ends." {
		t.Fatalf("previous summary = %+v", s)
	}
	// The additive fields ride along.
	if b.Recording.ChapterCount != 5 || b.Work.Attribution == nil || b.Work.Attribution.SourceURL != base+"/work?id=book-two" {
		t.Fatalf("chapter_count/attribution = %d / %+v", b.Recording.ChapterCount, b.Work.Attribution)
	}

	for _, q := range []string{"", "&include=everything", "&spoilers=maybe"} {
		_, body := e.do(t, "GET", path+q, adminTok, "")
		if strings.Contains(body, `"previous"`) {
			t.Fatalf("%q carries previous: %s", q, body)
		}
		if !strings.Contains(body, `"ending":"Two ends."`) || !strings.Contains(body, `"late"`) {
			t.Fatalf("%q is not the full envelope: %s", q, body)
		}
	}
}

// TestMetaSpoilersHide is the allowed+denied pair for spoiler gating: the
// caller's OWN saved progress gates the current work, and another user's
// progress on the same book never leaks into it.
func TestMetaSpoilersHide(t *testing.T) {
	e, _ := bundleEnv(t)
	ctx := context.Background()
	lib := seedChapteredBook(t, e, "A/Two", "B0TWO")
	adminTok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	reader, err := e.auth.CreateUser(ctx, "reader", "reader-password", auth.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.cat.GrantWholeLibrary(ctx, reader.ID, lib.ID); err != nil {
		t.Fatal(err)
	}
	readerTok, _ := e.auth.IssueToken(ctx, reader.ID, auth.KindSession, "t", 0)
	path := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10) + "/meta?path=" + escape("A/Two") + "&spoilers=hide&include=previous"

	// No progress: the opening cast, the chapter-0 recap, no summary; the
	// previous book loses only its ending.
	b := getBundle(t, e, path, readerTok)
	if ids := characterIDsOf(b); !slices.Equal(ids, []string{"early"}) {
		t.Fatalf("no progress: characters = %v, want [early]", ids)
	}
	if len(b.Work.Recaps) != 1 || b.Work.Recaps[0].Text != "Before." || b.Work.RecapSummary != nil {
		t.Fatalf("no progress: recaps = %+v, summary = %+v", b.Work.Recaps, b.Work.RecapSummary)
	}
	if len(b.Previous) != 1 || b.Previous[0].RecapSummary == nil || b.Previous[0].RecapSummary.InShort != "One in short." || b.Previous[0].RecapSummary.Ending != "" {
		t.Fatalf("previous under spoilers=hide = %+v", b.Previous)
	}

	// The admin is mid-book (inside chapter 4): more cast, the chapter-2 recap.
	if _, err := e.cat.SaveProgress(ctx, e.adminID, catalog.Progress{Ref: catalog.Ref{LibraryID: lib.ID, Path: "A/Two"}, Position: 350, Duration: 500}); err != nil {
		t.Fatal(err)
	}
	b = getBundle(t, e, path, adminTok)
	if ids := characterIDsOf(b); !slices.Equal(ids, []string{"early", "middle"}) {
		t.Fatalf("chapter 4: characters = %v, want [early middle]", ids)
	}
	if len(b.Work.Recaps) != 2 || b.Work.RecapSummary != nil {
		t.Fatalf("chapter 4: recaps = %+v, summary = %+v", b.Work.Recaps, b.Work.RecapSummary)
	}

	// Denied: the admin's progress is not the reader's. The reader still sees
	// only the opening cast.
	if ids := characterIDsOf(getBundle(t, e, path, readerTok)); !slices.Equal(ids, []string{"early"}) {
		t.Fatalf("another user's progress leaked: characters = %v", ids)
	}

	// Finished reveals the whole current work.
	if _, err := e.cat.SaveProgress(ctx, reader.ID, catalog.Progress{Ref: catalog.Ref{LibraryID: lib.ID, Path: "A/Two"}, Position: 10, Duration: 500, Finished: true}); err != nil {
		t.Fatal(err)
	}
	b = getBundle(t, e, path, readerTok)
	if ids := characterIDsOf(b); len(ids) != 3 || b.Work.RecapSummary == nil || b.Work.RecapSummary.Ending != "Two ends." {
		t.Fatalf("finished: characters = %v, summary = %+v", ids, b.Work.RecapSummary)
	}

	// The gating never reached the shared cached envelope: a plain request is
	// whole.
	if ids := characterIDsOf(getBundle(t, e, "/api/v1/libraries/"+strconv.FormatInt(lib.ID, 10)+"/meta?path="+escape("A/Two"), readerTok)); len(ids) != 3 {
		t.Fatalf("plain request after gating: characters = %v", ids)
	}
}

// TestMetaBundleCapability: meta_bundle follows the metadata switch.
func TestMetaBundleCapability(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		e := newMetaEnv(t, enabled, 0)
		_, si := e.do(t, "GET", "/api/v1/server", "", "")
		for _, flag := range []string{"meta_bundle", "meta_covers"} {
			if want := `"` + flag + `":` + strconv.FormatBool(enabled); !strings.Contains(si, want) {
				t.Fatalf("enabled=%v: /server missing %s: %s", enabled, want, si)
			}
		}
	}
}

// TestMetaPersistentCache: the api wires the catalog's meta_cache behind the
// service, so a lookup leaves a row a restarted server reads.
func TestMetaPersistentCache(t *testing.T) {
	e := newMetaEnv(t, true, 0)
	libID := seedBook(t, e, "Andy Weir/The Martian", "B00FLIJJSY")
	adminTok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	path := "/api/v1/libraries/" + strconv.FormatInt(libID, 10) + "/meta?path=" + escape("Andy Weir/The Martian")
	if resp, body := e.do(t, "GET", path, adminTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("meta = %d %s", resp.StatusCode, body)
	}
	row, err := e.cat.GetMetaCache(context.Background(), "a:B00FLIJJSY")
	if err != nil || row == nil || !strings.Contains(string(row.Payload), `"the-martian"`) || row.Source != e.cfg.Metadata.BaseURL {
		t.Fatalf("meta_cache row = %+v, %v", row, err)
	}
}
