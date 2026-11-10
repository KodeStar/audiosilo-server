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
// confident match (UK and US ASINs), anything else nothing; lookup knows the US
// ASIN only.
func matchRunMetaserve(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("asin") != "B0US000001" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"work":{"id":"the-martian","title":"The Martian","authors":[{"id":"andy-weir","name":"Andy Weir"}]},"recording_id":"bray"}`))
	})
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
	t.Parallel()
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

// TestMatchRunsRefresh: a refresh run takes the books with an identifier, finds
// them by it and fills their details; it keeps the identifier, and refuses the ids
// scope (it would write nothing).
func TestMatchRunsRefresh(t *testing.T) {
	t.Parallel()
	e, adminTok, _, libID := newMatchRunEnv(t, "uk")
	ctx := context.Background()
	if err := e.cat.BackfillASIN(context.Background(), libID, "Someone/Obscure", "B0US000001"); err != nil {
		t.Fatal(err)
	}
	resp, body := e.do(t, "POST", "/api/v1/admin/match-runs", adminTok, `{"mode":"refresh"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start = %d %s", resp.StatusCode, body)
	}
	run := decodeRun(t, body)
	if run.Total != 1 || run.Mode != catalog.MatchModeRefresh {
		t.Fatalf("started = %+v", run)
	}
	if _, body := e.do(t, "GET", "/api/v1/admin/audit", adminTok, ""); !strings.Contains(body, "book.match_refresh") {
		t.Errorf("audit = %s, want the refresh logged as one", body)
	}
	e.api.matchRuns.Wait()
	runURL := "/api/v1/admin/match-runs/" + strconv.FormatInt(run.ID, 10)
	if _, body = e.do(t, "GET", runURL, adminTok, ""); decodeRun(t, body).Counts.Auto != 1 {
		t.Fatalf("run = %s", body)
	}
	if resp, body := e.do(t, "POST", runURL+"/apply", adminTok, `{"scope":"ids"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("ids scope = %d %s, want 400", resp.StatusCode, body)
	}
	if resp, body = e.do(t, "POST", runURL+"/apply", adminTok, `{"scope":"fill"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("apply = %d %s", resp.StatusCode, body)
	}
	e.api.matchRuns.Wait()
	book, err := e.cat.GetBookByPath(ctx, libID, "Someone/Obscure")
	if err != nil || book.ASIN != "B0US000001" || book.Narrator != "R. C. Bray" || book.Title != "Obscure" {
		t.Fatalf("book after refresh = %+v %v, want the narrator filled, its title and ASIN kept", book, err)
	}
}

// An apply that excludes every automatic item (an older console's Done in an
// empty refresh) finishes the run and changes no book.
func TestMatchRunsFinishEmptyRefresh(t *testing.T) {
	t.Parallel()
	e, tok, _, libID := newMatchRunEnv(t, "uk")
	ctx := context.Background()
	const path = "Someone/Obscure"
	if err := e.cat.BackfillASIN(context.Background(), libID, path, "B0US000001"); err != nil {
		t.Fatal(err)
	}
	resp, body := e.do(t, "POST", "/api/v1/admin/match-runs", tok, `{"mode":"refresh"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start = %d %s", resp.StatusCode, body)
	}
	run := decodeRun(t, body)
	e.api.matchRuns.Wait()
	items, _, err := e.cat.ListMatchRunItems(ctx, run.ID, catalog.OutcomeAuto, 0, 50)
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %+v %v", items, err)
	}
	runURL := "/api/v1/admin/match-runs/" + strconv.FormatInt(run.ID, 10)
	resp, body = e.do(t, "POST", runURL+"/apply", tok, `{"scope":"fill","exclude":[`+strconv.FormatInt(items[0].ID, 10)+`]}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("finish = %d %s", resp.StatusCode, body)
	}
	e.api.matchRuns.Wait()
	_, body = e.do(t, "GET", runURL, tok, "")
	if got := decodeRun(t, body); got.Status != catalog.MatchApplied || got.ApplyTotal != 0 || got.Counts.Applied != 0 {
		t.Fatalf("finished run = %+v", got)
	}
	book, err := e.cat.GetBookByPath(ctx, libID, path)
	if err != nil || book.ASIN != "B0US000001" || book.Title != "Obscure" || book.Narrator != "" {
		t.Fatalf("excluded book changed = %+v %v", book, err)
	}
}

// An apply's exclusion list can exceed the manual-pick limit (an older console
// excluded every unchanged refresh book; the server now leaves those out itself:
// TestRefreshApplyLeavesOutUnchanged), within the 1 MiB body.
func TestMatchRunLargeExclusions(t *testing.T) {
	t.Parallel()
	e, tok, _, libID := newMatchRunEnv(t, "uk")
	if err := e.cat.BackfillASIN(context.Background(), libID, "Someone/Obscure", "B0US000001"); err != nil {
		t.Fatal(err)
	}
	resp, body := e.do(t, "POST", "/api/v1/admin/match-runs", tok, `{"mode":"refresh"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start = %d %s", resp.StatusCode, body)
	}
	run := decodeRun(t, body)
	e.api.matchRuns.Wait()
	applyURL := "/api/v1/admin/match-runs/" + strconv.FormatInt(run.ID, 10) + "/apply"
	ids := make([]int64, maxMatchPicks+1)
	for i := range ids {
		ids[i] = int64(i + 1000000) // Other items are ignored, as for smaller lists.
	}
	raw, err := json.Marshal(map[string]any{"scope": "fill", "include": ids})
	if err != nil {
		t.Fatal(err)
	}
	if resp, body := e.do(t, "POST", applyURL, tok, string(raw)); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, codeTooLarge) {
		t.Fatalf("too many manual picks = %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(t, "POST", applyURL, tok, strings.Repeat(" ", 1<<20)+`{"scope":"fill"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversize body = %d %s", resp.StatusCode, body)
	}
	raw, err = json.Marshal(map[string]any{"scope": "fill", "exclude": ids})
	if err != nil {
		t.Fatal(err)
	}
	resp, body = e.do(t, "POST", applyURL, tok, string(raw))
	if resp.StatusCode != http.StatusAccepted || decodeRun(t, body).ApplyTotal != 1 {
		t.Fatalf("large exclusion list = %d %s", resp.StatusCode, body)
	}
	e.api.matchRuns.Wait()
	book, err := e.cat.GetBookByPath(context.Background(), libID, "Someone/Obscure")
	if err != nil || book.Narrator != "R. C. Bray" {
		t.Fatalf("book after refresh = %+v %v", book, err)
	}
}

func TestMatchRunsStartRefusals(t *testing.T) {
	t.Parallel()
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
	// Every field is optional, the body too: a match over every library.
	if resp, got := e.do(t, "POST", "/api/v1/admin/match-runs", adminTok, ""); resp.StatusCode != http.StatusAccepted {
		t.Errorf("no body = %d %s, want 202", resp.StatusCode, got)
	}
	e.api.matchRuns.Wait()
}

// TestMatchRunsAdminOnly: every route refuses a member (403), and all of them
// 404 metadata_off while community metadata is off.
func TestMatchRunsAdminOnly(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	e, adminTok, _, libID := newMatchRunEnv(t, "")
	if resp, body := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"metadata":{"region":"xx"}}`); resp.StatusCode != http.StatusBadRequest ||
		!strings.Contains(body, `"field":"metadata.region"`) {
		t.Fatalf("bad region = %d %s", resp.StatusCode, body)
	}
	matchURL := "/api/v1/admin/libraries/" + strconv.FormatInt(libID, 10) + "/book/match?path=" + escape("Andy Weir/The Martian")
	_, body := e.do(t, "GET", matchURL, adminTok, "")
	if !strings.Contains(body, `"asins":["B0US000001","B0UK000001"]`) || !strings.Contains(body, `"asin_region":"us"`) {
		t.Fatalf("no preference: US first = %s", body)
	}
	if resp, body := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"metadata":{"region":"UK"}}`); resp.StatusCode != http.StatusOK ||
		!strings.Contains(body, `"region":"uk"`) {
		t.Fatalf("set region = %d %s", resp.StatusCode, body)
	}
	_, body = e.do(t, "GET", matchURL, adminTok, "")
	if !strings.Contains(body, `"asins":["B0UK000001","B0US000001"]`) || !strings.Contains(body, `"asin_region":"uk"`) ||
		!strings.Contains(body, `"default_recording_id":"bray"`) ||
		!strings.Contains(body, `"asin_refs":[{"region":"uk","asin":"B0UK000001"},{"region":"us","asin":"B0US000001"}]`) {
		t.Fatalf("uk preferred = %s", body)
	}
}

// TestClearCommunityMatchesAPI: DELETE /admin/community-matches undoes an applied
// run's matches (the book is unmatched again, the run gone) and is audited; it
// works with community metadata off; a bad or unknown library is refused; and it is
// admin-only.
func TestClearCommunityMatchesAPI(t *testing.T) {
	t.Parallel()
	e, adminTok, memberTok, libID := newMatchRunEnv(t, "uk")
	lib := strconv.FormatInt(libID, 10)
	resp, body := e.do(t, "POST", "/api/v1/admin/match-runs", adminTok, `{"library_id":`+lib+`}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start = %d %s", resp.StatusCode, body)
	}
	run := decodeRun(t, body)
	e.api.matchRuns.Wait()
	if resp, body := e.do(t, "POST", "/api/v1/admin/match-runs/"+strconv.FormatInt(run.ID, 10)+"/apply", adminTok,
		`{"scope":"fill"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("apply = %d %s", resp.StatusCode, body)
	}
	e.api.matchRuns.Wait()

	clearURL := "/api/v1/admin/community-matches"
	for _, tok := range []string{memberTok, ""} {
		want := http.StatusForbidden
		if tok == "" {
			want = http.StatusUnauthorized
		}
		if resp, _ := e.do(t, "DELETE", clearURL, tok, ""); resp.StatusCode != want {
			t.Errorf("clear as %q = %d, want %d", tok, resp.StatusCode, want)
		}
	}
	for q, want := range map[string]int{"?library_id=x": http.StatusBadRequest, "?library_id=-1": http.StatusBadRequest,
		"?library_id=999": http.StatusNotFound} {
		if resp, _ := e.do(t, "DELETE", clearURL+q, adminTok, ""); resp.StatusCode != want {
			t.Errorf("clear%s = %d, want %d", q, resp.StatusCode, want)
		}
	}
	if book, _ := e.cat.GetBookByPath(context.Background(), libID, "Andy Weir/The Martian"); book.ASIN == "" {
		t.Fatal("a refused clear changed the book")
	}

	// Metadata off: the clear sends nothing out, so it still works.
	if resp, body := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"metadata":{"enabled":false}}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("turn metadata off = %d %s", resp.StatusCode, body)
	}
	resp, body = e.do(t, "DELETE", clearURL+"?library_id="+lib, adminTok, "")
	var cleared catalog.ClearedMatches
	if err := json.Unmarshal([]byte(body), &cleared); err != nil || resp.StatusCode != http.StatusOK ||
		cleared != (catalog.ClearedMatches{Books: 1, Covers: 0, Runs: 1}) {
		t.Fatalf("clear = %d %s", resp.StatusCode, body)
	}
	book, err := e.cat.GetBookByPath(context.Background(), libID, "Andy Weir/The Martian")
	if err != nil || book.ASIN != "" || book.Narrator != "" {
		t.Fatalf("book after clear = %+v %v, want the community values gone", book, err)
	}
	if runs, err := e.cat.ListMatchRuns(context.Background()); err != nil || len(runs) != 0 {
		t.Fatalf("runs after clear = %d %v", len(runs), err)
	}
	if _, body := e.do(t, "GET", "/api/v1/admin/audit", adminTok, ""); !strings.Contains(body, "book.match_clear") {
		t.Fatalf("audit = %s", body)
	}
}

// TestClearCommunityMatchesNoService: with no metadata service configured there
// are no runs, but a clear still works (community values outlive the service).
func TestClearCommunityMatchesNoService(t *testing.T) {
	t.Parallel()
	e := newTestEnvWith(t, func(c *config.Config) { c.Metadata.BaseURL = "" })
	adminTok, _, _ := adminAndMember(t, e)
	if e.api.meta != nil {
		t.Fatal("the test env has a metadata service")
	}
	if resp, body := e.do(t, "DELETE", "/api/v1/admin/community-matches", adminTok, ""); resp.StatusCode != http.StatusOK ||
		!strings.Contains(body, `"books":0`) {
		t.Fatalf("clear = %d %s", resp.StatusCode, body)
	}
}
