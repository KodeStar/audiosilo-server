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
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"the-martian","title":"The Martian","subtitle":"","authors":[{"id":"andy-weir","name":"Andy Weir"}],"language":"en","first_published":"2011","description":"Stranded.","series":[{"id":"mars","name":"Mars","position":"1"}],"recordings":[{"id":"rec1","narrators":[{"id":"r-c-bray","name":"R. C. Bray"}],"abridged":false,"runtime_min":634,"release_date":"2013-03-22","publisher":"Podium Audio","cover_url":"https://c/1.jpg","chapter_count":12}]}`))
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
	mock := httptest.NewServer((&mockMetaserve{lookupCode: lookupCode}).handler())
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
	for _, want := range []string{`"matched":true`, `"the-martian"`, `"R. C. Bray"`, `"Podium Audio"`, `/work?id=the-martian`, `"artemis"`} {
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
