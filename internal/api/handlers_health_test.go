package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
)

// Phase 3: the Health and Jobs endpoints.

// Every Health/Jobs endpoint refuses a signed-in member (403) and an anonymous
// caller (401), and answers an admin.
func TestHealthEndpointsRequireAdmin(t *testing.T) {
	e := newTestEnv(t)
	adminTok, memberTok, _ := adminAndMember(t, e)
	libID, base := seedCatalog(t, e)
	ctx := context.Background()
	runID, err := e.cat.StartScanRun(ctx, libID, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	ignore := `{"kind":"no_cover","books":[{"library_id":` + strconv.FormatInt(libID, 10) + `,"path":"Andy Weir/Artemis"}]}`
	for _, tc := range []struct {
		method, path, body string
		ok                 int
	}{
		{"GET", "/api/v1/admin/issues", "", 200},
		{"GET", "/api/v1/admin/issues/duplicates", "", 200},
		{"POST", "/api/v1/admin/issues/ignore", ignore, 204},
		{"DELETE", "/api/v1/admin/issues/ignore", ignore, 204},
		// The seeded books have no files on disk: the admin's answer is the 404.
		{"POST", base + "/book/rescan?path=" + escape("Andy Weir/Artemis"), "", 404},
		{"GET", "/api/v1/admin/jobs", "", 200},
		{"POST", "/api/v1/admin/scan", "", 202},
		{"DELETE", "/api/v1/admin/jobs/12345", "", 404},
		{"GET", "/api/v1/admin/scan-runs", "", 200},
		{"GET", "/api/v1/admin/scan-runs/" + strconv.FormatInt(runID, 10), "", 200},
	} {
		if resp, _ := e.do(t, tc.method, tc.path, "", tc.body); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", tc.method, tc.path, resp.StatusCode)
		}
		if resp, _ := e.do(t, tc.method, tc.path, memberTok, tc.body); resp.StatusCode != http.StatusForbidden {
			t.Errorf("member %s %s = %d, want 403", tc.method, tc.path, resp.StatusCode)
		}
		if resp, body := e.do(t, tc.method, tc.path, adminTok, tc.body); resp.StatusCode != tc.ok {
			t.Errorf("admin %s %s = %d %s, want %d", tc.method, tc.path, resp.StatusCode, body, tc.ok)
		}
	}
}

type issuesResp struct {
	Categories []catalog.IssueCount `json:"categories"`
	Offline    []offlineLibrary     `json:"offline"`
	CheckedAt  string               `json:"checked_at"`
}

// The summary leaves "not matched" out while metadata is off, lists offline
// libraries with what they keep, and the issue lists honour ignores.
func TestIssuesSummaryAndIgnore(t *testing.T) {
	e := newTestEnvWith(t, func(c *config.Config) { c.Metadata.Enabled = false })
	adminTok, _, _ := adminAndMember(t, e)
	libID, _ := seedCatalog(t, e)
	ctx := context.Background()
	// Its folder holds something, so it reads as online (an empty one with books
	// indexed is what an unmounted share looks like).
	main, _ := e.cat.GetLibrary(ctx, libID)
	if err := os.WriteFile(filepath.Join(main.Root, "x.m4b"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gone, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "NAS", Root: filepath.Join(t.TempDir(), "unmounted")})
	if _, err := e.cat.UpsertBook(ctx, &catalog.Book{LibraryID: gone.ID, RelPath: "Kept/Book", Title: "Kept"}); err != nil {
		t.Fatal(err)
	}
	uid := e.adminID
	if _, err := e.cat.SaveProgress(ctx, uid, catalog.Progress{Ref: catalog.Ref{LibraryID: gone.ID, Path: "Kept/Book"}, Position: 5}); err != nil {
		t.Fatal(err)
	}

	resp, body := e.do(t, "GET", "/api/v1/admin/issues", adminTok, "")
	if resp.StatusCode != 200 {
		t.Fatalf("issues = %d %s", resp.StatusCode, body)
	}
	var got issuesResp
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	for _, c := range got.Categories {
		if c.Kind == catalog.IssueUnmatched {
			t.Fatal("not matched is listed while community metadata is off")
		}
	}
	if len(got.Offline) != 1 || got.Offline[0].Name != "NAS" || got.Offline[0].Books != 1 || got.Offline[0].Listeners != 1 {
		t.Fatalf("offline = %+v", got.Offline)
	}

	// A book in a codec browsers can't play, so the transcode list has one to ignore.
	if _, err := e.cat.UpsertBook(ctx, &catalog.Book{LibraryID: libID, RelPath: "AC3/Book", Title: "AC3", Codec: "ac3"}); err != nil {
		t.Fatal(err)
	}
	list := "/api/v1/admin/books?issue=transcode"
	if _, body := e.do(t, "GET", list, adminTok, ""); !strings.Contains(body, `"path":"AC3/Book"`) {
		t.Fatalf("transcode list = %s", body)
	}
	ignore := `{"kind":"transcode","books":[{"library_id":` + strconv.FormatInt(libID, 10) + `,"path":"AC3/Book/"}]}`
	if resp, body := e.do(t, "POST", "/api/v1/admin/issues/ignore", adminTok, ignore); resp.StatusCode != 204 {
		t.Fatalf("ignore = %d %s", resp.StatusCode, body)
	}
	if _, body := e.do(t, "GET", list, adminTok, ""); strings.Contains(body, "AC3/Book") {
		t.Fatalf("an ignored book is still listed: %s", body)
	}
	if _, body := e.do(t, "GET", list+"&issue_ignored=true", adminTok, ""); !strings.Contains(body, `"path":"AC3/Book"`) {
		t.Fatalf("the ignored list misses it: %s", body)
	}
	if resp, _ := e.do(t, "DELETE", "/api/v1/admin/issues/ignore", adminTok, ignore); resp.StatusCode != 204 {
		t.Fatalf("unignore = %d", resp.StatusCode)
	}
	if _, body := e.do(t, "GET", list, adminTok, ""); !strings.Contains(body, `"path":"AC3/Book"`) {
		t.Fatalf("an un-ignored book isn't back: %s", body)
	}

	many := make([]string, maxBulkBooks+1)
	for i := range many {
		many[i] = `{"library_id":1,"path":"p` + strconv.Itoa(i) + `"}`
	}
	if resp, body := e.do(t, "POST", "/api/v1/admin/issues/ignore", adminTok,
		`{"kind":"transcode","books":[`+strings.Join(many, ",")+`]}`); resp.StatusCode != http.StatusBadRequest ||
		!strings.Contains(body, `"code":"too_large"`) {
		t.Fatalf("oversized ignore = %d %s", resp.StatusCode, body)
	}
	for _, bad := range []struct{ method, path, body string }{
		{"GET", "/api/v1/admin/books?issue=duplicate", ""},
		{"GET", "/api/v1/admin/books?issue=bogus", ""},
		{"GET", "/api/v1/admin/books?issue_ignored=true", ""},
		{"POST", "/api/v1/admin/issues/ignore", `{"kind":"bogus","books":[{"library_id":1,"path":"x"}]}`},
		{"POST", "/api/v1/admin/issues/ignore", `{"kind":"transcode","books":[]}`},
		{"POST", "/api/v1/admin/issues/ignore", `{"kind":"transcode","books":[{"library_id":1,"path":".."}]}`},
	} {
		if resp, body := e.do(t, bad.method, bad.path, adminTok, bad.body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s %s %s = %d %s, want 400", bad.method, bad.path, bad.body, resp.StatusCode, body)
		}
	}
}

// Library scan settings: validated with codes; a schedule change doesn't rescan,
// an ignore-rule change does; the admin list carries them, the player's doesn't.
func TestLibraryScanSettingsEndpoints(t *testing.T) {
	e := newTestEnv(t)
	adminTok, memberTok, _ := adminAndMember(t, e)
	ctx := context.Background()
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: t.TempDir()})
	url := "/api/v1/admin/libraries/" + strconv.FormatInt(lib.ID, 10)

	for body, code := range map[string]string{
		`{"scan_schedule":"every:5h"}`:    codeInvalidSchedule,
		`{"ignore_patterns":["[broken"]}`: codeInvalidPattern,
	} {
		resp, got := e.do(t, "PATCH", url, adminTok, body)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(got, `"code":"`+code+`"`) {
			t.Errorf("PATCH %s = %d %s, want 400 %s", body, resp.StatusCode, got, code)
		}
	}
	if resp, _ := e.do(t, "PATCH", url, memberTok, `{"scan_schedule":"every:6h"}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member PATCH = %d, want 403", resp.StatusCode)
	}

	if resp, body := e.do(t, "PATCH", url, adminTok, `{"scan_schedule":"daily:03:00"}`); resp.StatusCode != 200 ||
		strings.Contains(body, `"job"`) || !strings.Contains(body, `"name":"Main"`) {
		t.Fatalf("schedule PATCH = %d %s, want the library and no job", resp.StatusCode, body)
	}
	if _, queued := e.api.scanner.Jobs(); len(queued) != 0 {
		t.Fatalf("a schedule change queued a scan: %+v", queued)
	}
	if resp, body := e.do(t, "PATCH", url, adminTok, `{"ignore_patterns":[" *.tmp ","","Extras/"]}`); resp.StatusCode != 200 ||
		!strings.Contains(body, `"job":{`) || !strings.Contains(body, `"trigger":"change"`) {
		t.Fatalf("ignore PATCH = %d %s, want the queued job", resp.StatusCode, body)
	}
	if _, queued := e.api.scanner.Jobs(); len(queued) != 1 || queued[0].Trigger != "change" {
		t.Fatalf("an ignore change didn't queue a rescan: %+v", queued)
	}

	_, body := e.do(t, "GET", "/api/v1/admin/libraries", adminTok, "")
	for _, want := range []string{`"scan_schedule":"daily:03:00"`, `"ignore_patterns":["*.tmp","Extras/"]`, `"next_scan_at":"`, `"queued":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("admin libraries missing %s: %s", want, body)
		}
	}
	_, body = e.do(t, "GET", "/api/v1/libraries", adminTok, "")
	if strings.Contains(body, "ignore_patterns") || strings.Contains(body, "scan_schedule") {
		t.Fatalf("the player's library list carries admin settings: %s", body)
	}

	// Create takes them too; a bad one is refused before anything is made.
	if resp, body := e.do(t, "POST", "/api/v1/admin/libraries", adminTok,
		`{"name":"Kids","root":"`+t.TempDir()+`","scan_schedule":"weekly"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create with a bad schedule = %d %s", resp.StatusCode, body)
	}
	if libs, _ := e.cat.ListLibraries(ctx); len(libs) != 1 {
		t.Fatalf("a refused create made a library: %+v", libs)
	}
}

// POST /admin/scan queues every library, one job each.
func TestScanAll(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	ctx := context.Background()
	for _, n := range []string{"A", "B"} {
		if _, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: n, Root: t.TempDir()}); err != nil {
			t.Fatal(err)
		}
	}
	resp, body := e.do(t, "POST", "/api/v1/admin/scan", adminTok, "")
	if resp.StatusCode != http.StatusAccepted || strings.Count(body, `"kind":"scan"`) != 2 {
		t.Fatalf("scan all = %d %s", resp.StatusCode, body)
	}
	if _, queued := e.api.scanner.Jobs(); len(queued) != 2 {
		t.Fatalf("queued = %+v", queued)
	}
}

// POST .../scan returns the queued job; the job lists, and cancels once.
func TestJobsEndpoints(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	ctx := context.Background()
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: t.TempDir(), ScanSchedule: "every:6h"})
	resp, body := e.do(t, "POST", "/api/v1/admin/libraries/"+strconv.FormatInt(lib.ID, 10)+"/scan", adminTok, "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("scan = %d %s", resp.StatusCode, body)
	}
	var started struct {
		Job struct {
			ID        int64  `json:"id"`
			Trigger   string `json:"trigger"`
			StartedBy *int64 `json:"started_by"`
		} `json:"job"`
	}
	_ = json.Unmarshal([]byte(body), &started)
	if started.Job.ID == 0 || started.Job.Trigger != "manual" || started.Job.StartedBy == nil || *started.Job.StartedBy != e.adminID {
		t.Fatalf("job = %s", body)
	}
	_, body = e.do(t, "GET", "/api/v1/admin/jobs", adminTok, "")
	if !strings.Contains(body, `"running":null`) || !strings.Contains(body, `"library_name":"Main"`) ||
		!strings.Contains(body, `"schedule":"every:6h"`) {
		t.Fatalf("jobs = %s", body)
	}
	cancel := "/api/v1/admin/jobs/" + strconv.FormatInt(started.Job.ID, 10)
	if resp, _ := e.do(t, "DELETE", cancel, adminTok, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("cancel = %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "DELETE", cancel, adminTok, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second cancel = %d, want 404", resp.StatusCode)
	}
}

// Scan history pages with next_before; a single run carries its log.
func TestScanRunsEndpoints(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	ctx := context.Background()
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: t.TempDir()})
	var last int64
	for range 3 {
		id, _ := e.cat.StartScanRun(ctx, lib.ID, "schedule", nil)
		_ = e.cat.FinishScanRun(ctx, id, catalog.RunOK, catalog.ScanCounts{Added: 2},
			[]catalog.RunEvent{{Kind: "removed", Path: "Old/Book"}})
		last = id
	}
	_, body := e.do(t, "GET", "/api/v1/admin/scan-runs?limit=2", adminTok, "")
	var page struct {
		Runs       []catalog.ScanRun `json:"runs"`
		NextBefore int64             `json:"next_before"`
	}
	_ = json.Unmarshal([]byte(body), &page)
	if len(page.Runs) != 2 || page.Runs[0].ID != last || page.NextBefore != page.Runs[1].ID || page.Runs[0].Log != nil {
		t.Fatalf("page = %s", body)
	}
	_, body = e.do(t, "GET", "/api/v1/admin/scan-runs?before="+strconv.FormatInt(page.NextBefore, 10), adminTok, "")
	if strings.Count(body, `"id":`) != 1 || strings.Contains(body, "next_before") {
		t.Fatalf("second page = %s", body)
	}
	_, body = e.do(t, "GET", "/api/v1/admin/scan-runs/"+strconv.FormatInt(last, 10), adminTok, "")
	if !strings.Contains(body, `"path":"Old/Book"`) {
		t.Fatalf("run = %s", body)
	}
	for _, bad := range []string{"?library_id=x", "?before=-1"} {
		if resp, _ := e.do(t, "GET", "/api/v1/admin/scan-runs"+bad, adminTok, ""); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("scan-runs%s = %d, want 400", bad, resp.StatusCode)
		}
	}
	if resp, _ := e.do(t, "GET", "/api/v1/admin/scan-runs/999999", adminTok, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing run = %d, want 404", resp.StatusCode)
	}
}

// Rescanning one book re-reads its files now and returns its page; a path with no
// book any more is a 404 not_indexable.
func TestRescanBook(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	ctx := context.Background()
	root := t.TempDir()
	src, err := os.ReadFile(filepath.Join("..", "..", "testdata", "library", "Brandon Sanderson", "Mistborn", "01 - The Final Empire.m4b"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Author", "Book"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Author", "Book", "book.m4b"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	url := "/api/v1/admin/libraries/" + strconv.FormatInt(lib.ID, 10) + "/book/rescan?path="
	resp, body := e.do(t, "POST", url+escape("Author/Book"), adminTok, "")
	if resp.StatusCode != 200 || !strings.Contains(body, `"path":"Author/Book"`) {
		t.Fatalf("rescan = %d %s", resp.StatusCode, body)
	}
	resp, body = e.do(t, "POST", url+escape("Author/Gone"), adminTok, "")
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, `"code":"not_indexable"`) {
		t.Fatalf("rescan of a missing book = %d %s", resp.StatusCode, body)
	}
	// Denied: a path out of the library never reaches the filesystem.
	if resp, body := e.do(t, "POST", url+escape("../../etc"), adminTok, ""); resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Fatalf("rescan of a path outside the library = %d %s, want a 4xx", resp.StatusCode, body)
	}
}
