package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
)

// escape url-escapes a ?path= value (a query param, matching the other tests).
func escape(s string) string { return url.QueryEscape(s) }

// mockMetaserve is a minimal metaserve stand-in for the /meta handler tests.
type mockMetaserve struct {
	lookupCode int // non-zero overrides the lookup response status
	workCode   int // non-zero overrides the works/{id} response status
}

func (m *mockMetaserve) handler() http.Handler {
	mux := http.NewServeMux()
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
		_, _ = w.Write([]byte(`{"id":"the-martian","title":"The Martian","subtitle":"","authors":[{"id":"andy-weir","name":"Andy Weir"}],"language":"en","first_published":"2011","description":"Stranded.","series":[{"id":"mars","name":"Mars","position":"1"}],"recordings":[{"id":"rec1","narrators":[{"id":"r-c-bray","name":"R. C. Bray"}],"abridged":false,"runtime_min":634,"release_date":"2013-03-22","publisher":"Podium Audio","cover_url":"https://c/1.jpg","chapter_count":12}],"characters":[{"id":"mark-watney","name":"Mark Watney","role":"protagonist","reveal":{"chapter":1},"description":"Stranded astronaut."}],"recaps":[{"through":{"chapter":3},"scope":"book","text":"Watney takes stock."}],"recap_summary":{"in_short":"Left behind on Mars.","ending":"Rescued by the Hermes crew."}}`))
	})
	mux.HandleFunc("GET /api/v1/series/{id}", func(w http.ResponseWriter, _ *http.Request) {
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
