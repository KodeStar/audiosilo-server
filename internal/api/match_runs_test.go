package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
)

// matchRunMetaserve answers works/match by the book's title: "Martian" is a
// confident match (UK and US ASINs), anything else nothing.
func matchRunMetaserve(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/works/match", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(strings.Join(r.URL.Query()["title"], " "), "Martian") {
			_, _ = w.Write([]byte(`{"results":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"kind":"work","id":"the-martian","score":96,"reasons":{"title":1}}]}`))
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"the-martian","title":"The Martian","authors":[{"id":"andy-weir","name":"Andy Weir"}],
			"description":"Stranded.","series":[],"recordings":[{"id":"bray","narrators":[{"id":"rcb","name":"R. C. Bray"}],
			"runtime_min":634,"asin":[{"region":"uk","asin":"B0UK000001"},{"region":"us","asin":"B0US000001"}],"isbn":[]}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func newMatchRunEnv(t *testing.T, region string) (*testEnv, string, string, int64) {
	t.Helper()
	base := matchRunMetaserve(t)
	e := newTestEnvWith(t, func(c *config.Config) {
		c.Metadata.Enabled, c.Metadata.BaseURL, c.Metadata.Region = true, base, region
	})
	adminTok, memberTok, _ := adminAndMember(t, e)
	ctx := context.Background()
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []catalog.Book{
		{RelPath: "Andy Weir/The Martian", Title: "The Martian"},
		{RelPath: "Someone/Obscure", Title: "Obscure"},
	} {
		b.LibraryID, b.IsFolder, b.Author, b.Duration, b.AddedAt = lib.ID, true, "Andy Weir", 634*60, "2020-01-01"
		if _, err := e.cat.UpsertBook(ctx, &b); err != nil {
			t.Fatal(err)
		}
	}
	return e, adminTok, memberTok, lib.ID
}

func decodeRun(t *testing.T, body string) catalog.MatchRun {
	t.Helper()
	var run catalog.MatchRun
	if err := json.Unmarshal([]byte(body), &run); err != nil {
		t.Fatalf("decode run %q: %v", body, err)
	}
	return run
}

func TestMatchRunsAPI(t *testing.T) {
	e, adminTok, _, libID := newMatchRunEnv(t, "uk")

	resp, body := e.do(t, "POST", "/api/v1/admin/match-runs", adminTok, `{"library_id":`+strconv.FormatInt(libID, 10)+`}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start = %d %s", resp.StatusCode, body)
	}
	run := decodeRun(t, body)
	if run.Total != 2 || run.Mode != catalog.MatchModeMatch || run.Region != "uk" || run.LibraryName != "Main" {
		t.Fatalf("started = %+v", run)
	}
	e.api.matchRuns.Wait()

	runURL := "/api/v1/admin/match-runs/" + strconv.FormatInt(run.ID, 10)
	_, body = e.do(t, "GET", runURL, adminTok, "")
	if run = decodeRun(t, body); run.Status != catalog.MatchReady || run.Counts.Auto != 1 || run.Counts.None != 1 {
		t.Fatalf("run = %s", body)
	}
	_, body = e.do(t, "GET", "/api/v1/admin/match-runs", adminTok, "")
	if !strings.Contains(body, `"region":"uk"`) || !strings.Contains(body, `"id":`+strconv.FormatInt(run.ID, 10)) {
		t.Fatalf("list = %s", body)
	}

	resp, body = e.do(t, "GET", runURL+"/items?outcome=auto", adminTok, "")
	var page struct {
		Items []struct {
			ID       int64  `json:"id"`
			Path     string `json:"path"`
			Proposal struct {
				Values     map[string]string `json:"values"`
				ASINRegion string            `json:"asin_region"`
			} `json:"proposal"`
			Changes map[string]struct{ Fields []string } `json:"changes"`
		} `json:"items"`
		NextAfter int64 `json:"next_after"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("items = %d %s", resp.StatusCode, body)
	}
	if len(page.Items) != 1 || page.Items[0].Path != "Andy Weir/The Martian" || page.Items[0].Proposal.Values["asin"] != "B0UK000001" ||
		page.Items[0].Proposal.ASINRegion != "uk" || len(page.Items[0].Changes["fill"].Fields) == 0 || page.NextAfter != 0 {
		t.Fatalf("items = %s", body)
	}
	for _, bad := range []string{"?outcome=maybe", "?after=x"} {
		if resp, _ := e.do(t, "GET", runURL+"/items"+bad, adminTok, ""); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("items%s = %d, want 400", bad, resp.StatusCode)
		}
	}

	if resp, _ := e.do(t, "POST", runURL+"/apply", adminTok, `{"scope":"everything"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad scope = %d, want 400", resp.StatusCode)
	}
	if resp, body = e.do(t, "POST", runURL+"/apply", adminTok, `{"scope":"fill"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("apply = %d %s", resp.StatusCode, body)
	}
	e.api.matchRuns.Wait()
	book, err := e.cat.GetBookByPath(context.Background(), libID, "Andy Weir/The Martian")
	if err != nil || book.ASIN != "B0UK000001" || book.Narrator != "R. C. Bray" || book.Title != "The Martian" {
		t.Fatalf("book after apply = %+v %v", book, err)
	}
	// Applied is final; the apply was audited.
	if resp, body := e.do(t, "POST", runURL+"/apply", adminTok, `{"scope":"fill"}`); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(body, codeMatchRunNotReady) {
		t.Fatalf("second apply = %d %s", resp.StatusCode, body)
	}
	if _, body := e.do(t, "GET", "/api/v1/admin/audit", adminTok, ""); !strings.Contains(body, "book.match_apply") ||
		!strings.Contains(body, "book.match_run") {
		t.Fatalf("audit = %s", body)
	}
	// Not working: nothing to cancel.
	if resp, body := e.do(t, "POST", runURL+"/cancel", adminTok, ""); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(body, codeMatchRunNotRunning) {
		t.Fatalf("cancel = %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/admin/match-runs/999", adminTok, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown run = %d, want 404", resp.StatusCode)
	}
}

func TestMatchRunsStartRefusals(t *testing.T) {
	e, adminTok, _, _ := newMatchRunEnv(t, "")
	for body, want := range map[string]int{
		`{"mode":"guess"}`:    http.StatusBadRequest,
		`{"library_id":999}`:  http.StatusNotFound,
		`{"library_id":-1}`:   http.StatusBadRequest,
		`{"mode":"repick"}`:   http.StatusBadRequest, // no preferred marketplace
		`{"unknown_field":1}`: http.StatusBadRequest,
	} {
		resp, got := e.do(t, "POST", "/api/v1/admin/match-runs", adminTok, body)
		if resp.StatusCode != want {
			t.Errorf("%s = %d %s, want %d", body, resp.StatusCode, got, want)
		}
		if body == `{"mode":"repick"}` && !strings.Contains(got, codeNoRegion) {
			t.Errorf("repick without a region = %s, want %s", got, codeNoRegion)
		}
	}
}

// TestMatchRunsAdminOnly: every route refuses a member (403), and all of them
// 404 metadata_off while community metadata is off.
func TestMatchRunsAdminOnly(t *testing.T) {
	routes := []struct{ method, path, body string }{
		{"GET", "/api/v1/admin/match-runs", ""},
		{"POST", "/api/v1/admin/match-runs", `{}`},
		{"GET", "/api/v1/admin/match-runs/1", ""},
		{"GET", "/api/v1/admin/match-runs/1/items", ""},
		{"POST", "/api/v1/admin/match-runs/1/apply", `{"scope":"ids"}`},
		{"POST", "/api/v1/admin/match-runs/1/cancel", ""},
	}
	e, adminTok, memberTok, _ := newMatchRunEnv(t, "uk")
	for _, r := range routes {
		if resp, _ := e.do(t, r.method, r.path, memberTok, r.body); resp.StatusCode != http.StatusForbidden {
			t.Errorf("member %s %s = %d, want 403", r.method, r.path, resp.StatusCode)
		}
		if resp, _ := e.do(t, r.method, r.path, "", r.body); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", r.method, r.path, resp.StatusCode)
		}
	}
	if resp, body := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"metadata":{"enabled":false}}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("turn metadata off = %d %s", resp.StatusCode, body)
	}
	for _, r := range routes {
		if resp, body := e.do(t, r.method, r.path, adminTok, r.body); resp.StatusCode != http.StatusNotFound ||
			!strings.Contains(body, codeMetadataOff) {
			t.Errorf("metadata off %s %s = %d %s", r.method, r.path, resp.StatusCode, body)
		}
	}
}

// TestMetadataRegionSetting: the preferred marketplace is a live console setting,
// stored as the community data names it, refused when it isn't one, and it orders
// the match dialog's ASINs.
func TestMetadataRegionSetting(t *testing.T) {
	e, adminTok, _, libID := newMatchRunEnv(t, "")
	if resp, body := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"metadata":{"region":"xx"}}`); resp.StatusCode != http.StatusBadRequest ||
		!strings.Contains(body, `"field":"metadata.region"`) {
		t.Fatalf("bad region = %d %s", resp.StatusCode, body)
	}
	matchURL := "/api/v1/admin/libraries/" + strconv.FormatInt(libID, 10) + "/book/match?path=" + escape("Andy Weir/The Martian")
	_, body := e.do(t, "GET", matchURL, adminTok, "")
	if !strings.Contains(body, `"asins":["B0US000001","B0UK000001"]`) || !strings.Contains(body, `"region":""`) {
		t.Fatalf("no preference: US first = %s", body)
	}
	if resp, body := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"metadata":{"region":"UK"}}`); resp.StatusCode != http.StatusOK ||
		!strings.Contains(body, `"region":"uk"`) {
		t.Fatalf("set region = %d %s", resp.StatusCode, body)
	}
	_, body = e.do(t, "GET", matchURL, adminTok, "")
	if !strings.Contains(body, `"asins":["B0UK000001","B0US000001"]`) || !strings.Contains(body, `"region":"uk"`) ||
		!strings.Contains(body, `"asin_refs":[{"region":"uk","asin":"B0UK000001"},{"region":"us","asin":"B0US000001"}]`) {
		t.Fatalf("uk preferred = %s", body)
	}
}
