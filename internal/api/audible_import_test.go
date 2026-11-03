package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Bridge Phase 4b: backing up Audible titles (audible.import).

// backupEnv is a bridgeEnv with the audible module on, a Bridge that runs
// audible.poll and audible.import and can write to the library, and testAccount
// mapped to the admin with titles from one complete poll (a match to library
// 0 is a match to the env's library).
func backupEnv(t *testing.T, titles ...string) *bridgeEnv {
	t.Helper()
	b := newBridgeEnv(t)
	b.beat(t, bridge.KindVerify, bridge.KindAudiblePoll, bridge.KindAudibleImport)
	setAudible(t, b.testEnv, b.adminTok, true)
	if code, out := b.putAccount(t, testAccount, b.adminID, b.lib.ID); code != http.StatusOK {
		t.Fatalf("put account: %d %s", code, out)
	}
	for i := range titles {
		titles[i] = strings.Replace(titles[i], `"library_id":0,`, fmt.Sprintf(`"library_id":%d,`, b.lib.ID), 1)
	}
	if code, out := b.putTitles(t, testAccount, "p1", true, titles...); code != http.StatusOK {
		t.Fatalf("titles: %d %s", code, out)
	}
	return b
}

func backupPath(asin string) string {
	return "/api/v1/admin/audible/titles/" + testAccount + "/" + asin + "/backup"
}

func (b *bridgeEnv) audibleTitle(t *testing.T, asin string) *catalog.AudibleTitle {
	t.Helper()
	x, err := b.cat.AudibleTitle(context.Background(), testAccount, asin)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// Security-critical: the backup routes are admin-only (a listener's session or
// API key 403), refuse the bridge, cast and calendar feed tokens and no token
// (401), and answer 404 module_off while the module is off. The admin's
// session and API key get through, as on every admin route.
func TestAudibleBackupRoutesAuth(t *testing.T) {
	t.Parallel()
	b := backupEnv(t, title("B000000001", "One", "available", 0, ""), title("B000000002", "Two", "available", 0, ""),
		title("B000000003", "Three", "available", 0, ""))
	ctx := context.Background()
	cast, _ := b.auth.IssueToken(ctx, b.adminID, auth.KindCast, "cast", 0)
	feed, _ := b.auth.IssueToken(ctx, b.adminID, auth.KindFeed, "feed", 0)
	var userID int64
	if err := b.db.QueryRowContext(ctx, `SELECT id FROM users WHERE username = 'sam'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	userKey, _ := b.auth.IssueToken(ctx, userID, auth.KindAPI, "key", 0)
	routes := []struct{ method, path, body string }{
		{"PATCH", "/api/v1/admin/audible/accounts/" + testAccount, `{"policy":"manual"}`},
		{"POST", "/api/v1/admin/audible/accounts/" + testAccount + "/backup-all", ""},
		{"PATCH", "/api/v1/admin/audible/titles/" + testAccount + "/B000000001", `{"rejected":false}`},
		{"POST", backupPath("B000000002"), ""},
	}
	setAudible(t, b.testEnv, b.adminTok, false)
	for _, r := range routes {
		if resp, body := b.do(t, r.method, r.path, b.adminTok, r.body); resp.StatusCode != http.StatusNotFound || codeOf(body) != "module_off" {
			t.Errorf("%s %s while off: %d %s, want 404 module_off", r.method, r.path, resp.StatusCode, body)
		}
	}
	if x := b.audibleTitle(t, "B000000002"); x.ImportJobID != nil {
		t.Fatal("a backup queued while off")
	}
	setAudible(t, b.testEnv, b.adminTok, true)
	for _, r := range routes {
		for name, tok := range map[string]string{"user session": b.userTok, "user api key": userKey} {
			if resp, body := b.do(t, r.method, r.path, tok, r.body); resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s as a %s: %d %s, want 403", r.method, r.path, name, resp.StatusCode, body)
			}
		}
		for name, tok := range map[string]string{"none": "", "bridge": b.bridgeTok, "cast": cast, "feed": feed, "garbage": "nope"} {
			if resp, _ := b.do(t, r.method, r.path, tok, r.body); resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with %s: %d, want 401", r.method, r.path, name, resp.StatusCode)
			}
			if tok != "" {
				if resp, _ := b.do(t, r.method, r.path+"?token="+url.QueryEscape(tok), "", r.body); resp.StatusCode != http.StatusUnauthorized {
					t.Errorf("%s %s with ?token=%s: %d, want 401", r.method, r.path, name, resp.StatusCode)
				}
			}
		}
	}
	if x := b.audibleTitle(t, "B000000002"); x.ImportJobID != nil {
		t.Fatal("a refused caller queued a backup")
	}
	// The admin's session and API key get through (a second backup of a title
	// is import_open: past the auth).
	for name, tok := range map[string]string{"session": b.adminTok, "api key": b.adminKey} {
		for _, r := range routes {
			resp, body := b.do(t, r.method, r.path, tok, r.body)
			if resp.StatusCode >= 300 && codeOf(body) != "import_open" {
				t.Errorf("%s %s as the admin's %s: %d %s", r.method, r.path, name, resp.StatusCode, body)
			}
		}
	}
}

// Back up: the job is built from the account and the title, runs on the
// Bridge, and once done its book is the title's match (import); a failed one
// shows its code; one per title; refusals answer their codes.
func TestAudibleBackupFlow(t *testing.T) {
	t.Parallel()
	b := backupEnv(t,
		title("B000000001", "One", "available", 0, ""),
		title("B000000002", "Two", "available", 0, ""),
		title("B000000003", "Here", "available", 0, cradleBook),
		title("B000000004", "Soon", "preorder", 0, ""),
		title("B000000005", "Away", "unavailable", 0, ""))
	ctx := context.Background()
	if resp, out := b.do(t, "PATCH", "/api/v1/admin/audible/accounts/"+testAccount, b.adminTok, `{"template":"{Author}/{Title}"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("template: %d %s", resp.StatusCode, out)
	}
	if resp, out := b.do(t, "PATCH", "/api/v1/admin/audible/titles/"+testAccount+"/B000000001", b.adminTok,
		`{"dest_path":"Brandon Sanderson/New/One.m4b"}`); resp.StatusCode != http.StatusOK || !strings.Contains(out, `"dest_path":"Brandon Sanderson/New/One.m4b"`) {
		t.Fatalf("dest: %d %s", resp.StatusCode, out)
	}
	resp, out := b.do(t, "POST", backupPath("B000000001"), b.adminTok, "")
	var queued struct {
		JobID int64 `json:"job_id"`
	}
	decodeInto(t, out, &queued)
	if resp.StatusCode != http.StatusAccepted || queued.JobID == 0 {
		t.Fatalf("backup: %d %s", resp.StatusCode, out)
	}
	job, err := b.api.bridge.Job(ctx, queued.JobID)
	want := fmt.Sprintf(`{"account":%q,"asin":"B000000001","library_id":%d,"dest_path":"Brandon Sanderson/New/One.m4b","template":"{Author}/{Title}"}`,
		testAccount, b.lib.ID)
	if err != nil || string(job.Params) != want || job.Kind != bridge.KindAudibleImport || *job.LibraryID != b.lib.ID {
		t.Fatalf("job = %+v, %v; want params %s", job, err, want)
	}
	if x := b.audibleTitle(t, "B000000001"); x.Import == nil || *x.Import.JobID != job.ID || x.Import.State != bridge.StateQueued {
		t.Fatalf("title's import = %+v", x.Import)
	}
	// One open backup per title.
	for asin, code := range map[string]string{"B000000001": "import_open", "B000000003": "already_matched", "B000000005": "not_available"} {
		if resp, out := b.do(t, "POST", backupPath(asin), b.adminTok, ""); resp.StatusCode != http.StatusConflict || codeOf(out) != code {
			t.Errorf("backup %s: %d %s, want 409 %s", asin, resp.StatusCode, out, code)
		}
	}
	for path, code := range map[string]string{backupPath("B000000009"): "unknown_title", backupPath("nope"): "unknown_title",
		"/api/v1/admin/audible/titles/unknown01/B000000001/backup": "unknown_account"} {
		if resp, out := b.do(t, "POST", path, b.adminTok, ""); resp.StatusCode != http.StatusNotFound || codeOf(out) != code {
			t.Errorf("POST %s: %d %s, want 404 %s", path, resp.StatusCode, out, code)
		}
	}
	// A preorder is a pending request.
	if resp, out := b.do(t, "POST", backupPath("B000000004"), b.adminTok, ""); resp.StatusCode != http.StatusOK || !strings.Contains(out, `"pending":true`) {
		t.Fatalf("preorder backup: %d %s", resp.StatusCode, out)
	}
	// The Bridge runs it: done with the book's place.
	claimed := b.claim(t, bridge.KindAudibleImport)
	if claimed.ID != job.ID {
		t.Fatalf("claimed %d, want %d", claimed.ID, job.ID)
	}
	result := fmt.Sprintf(`{"library_id":%d,"path":%q,"files":[{"path":"x.m4b","size":10}],"bytes":10,"enriched":true}`, b.lib.ID, mistbornBook)
	b.finish(t, claimed, "done", result, "")
	x := b.audibleTitle(t, "B000000001")
	if x.Match == nil || x.Match.How != catalog.AudibleMatchImport || x.Match.Path != mistbornBook || x.Import.State != bridge.StateDone {
		t.Fatalf("after done: match %+v import %+v", x.Match, x.Import)
	}
	// A failed one keeps the title as it was, its code shown.
	resp, out = b.do(t, "POST", backupPath("B000000002"), b.adminTok, "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("backup 2: %d %s", resp.StatusCode, out)
	}
	b.finish(t, b.claim(t, bridge.KindAudibleImport), "failed", `{"code":"exists","message":"a different file is there"}`, "exists: a different file is there")
	x = b.audibleTitle(t, "B000000002")
	if x.Match != nil || x.Import == nil || x.Import.State != bridge.StateFailed || x.Import.Code != "exists" {
		t.Fatalf("after failed: match %+v import %+v", x.Match, x.Import)
	}
	page := adminTitles(t, b, "account="+testAccount+"&q=Two")
	if len(page.Titles) != 1 || !strings.Contains(fmt.Sprintf("%+v", *page.Titles[0].Import), "Code:exists") {
		t.Fatalf("titles list = %+v", page.Titles)
	}
	// A Bridge without the kind takes none: nothing queued.
	b.beatKinds(t, "", bridge.KindVerify, bridge.KindAudiblePoll)
	if resp, out := b.do(t, "POST", "/api/v1/admin/audible/accounts/"+testAccount+"/backup-all", b.adminTok, ""); resp.StatusCode != http.StatusConflict || codeOf(out) != "kind_unavailable" {
		t.Fatalf("backup-all without the kind: %d %s", resp.StatusCode, out)
	}
	// Tried again after a failure.
	b.beat(t, bridge.KindVerify, bridge.KindAudiblePoll, bridge.KindAudibleImport)
	if resp, out := b.do(t, "POST", backupPath("B000000002"), b.adminTok, ""); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("retry: %d %s", resp.StatusCode, out)
	}
	// Bad inputs.
	for body, code := range map[string]string{`{"dest_path":"../x"}`: "invalid_dest_path", `{"x":1}`: "invalid_request"} {
		if resp, out := b.do(t, "PATCH", "/api/v1/admin/audible/titles/"+testAccount+"/B000000001", b.adminTok, body); resp.StatusCode != http.StatusBadRequest || codeOf(out) != code {
			t.Errorf("patch %s: %d %s, want 400 %s", body, resp.StatusCode, out, code)
		}
	}
	for body, code := range map[string]string{`{"policy":"often"}`: "invalid_policy", `{"template":"{Nope}"}`: "invalid_template"} {
		if resp, out := b.do(t, "PATCH", "/api/v1/admin/audible/accounts/"+testAccount, b.adminTok, body); resp.StatusCode != http.StatusBadRequest || codeOf(out) != code {
			t.Errorf("patch account %s: %d %s, want 400 %s", body, resp.StatusCode, out, code)
		}
	}
}

// claim takes the next job of kind as the Bridge and starts it.
func (b *bridgeEnv) claim(t *testing.T, kind string) bridge.Job {
	t.Helper()
	resp, out := b.do(t, "GET", "/api/v1/bridge/jobs/next?wait=0&kind="+kind, b.bridgeTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim %s: %d %s", kind, resp.StatusCode, out)
	}
	var j bridge.Job
	decodeInto(t, out, &j)
	return j
}

// finish reports j ended in state with result and errText.
func (b *bridgeEnv) finish(t *testing.T, j bridge.Job, state, result, errText string) {
	t.Helper()
	body := fmt.Sprintf(`{"attempt":%d,"state":%q,"result":%s,"error":%q}`, j.Attempts, state, result, errText)
	if resp, out := b.do(t, "PATCH", fmt.Sprintf("/api/v1/bridge/jobs/%d", j.ID), b.bridgeTok, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("finish %d: %d %s", j.ID, resp.StatusCode, out)
	}
}

// contestedTitle is title's unmatched title the Bridge stored with reason
// contested (another title claimed the same book).
func contestedTitle(asin, name string) string {
	return strings.Replace(title(asin, name, "available", 0, ""), `"reason":""`, `"reason":"contested"`, 1)
}

// Back up everything: every title available, not here, not marked, not
// contested and with no backup open, once; the estimate counts the same
// titles. A contested title can still be backed up on its own.
func TestAudibleBackupAll(t *testing.T) {
	t.Parallel()
	b := backupEnv(t,
		title("B000000001", "One", "available", 0, ""),
		title("B000000002", "Two", "available", 0, ""),
		title("B000000003", "Here", "available", 0, cradleBook),
		title("B000000004", "Soon", "preorder", 0, ""),
		title("B000000005", "Skip", "available", 0, ""),
		contestedTitle("B000000006", "Tied"))
	if resp, out := b.do(t, "PATCH", "/api/v1/admin/audible/titles/"+testAccount+"/B000000005", b.adminTok, `{"rejected":true}`); resp.StatusCode != http.StatusOK || !strings.Contains(out, `"rejected":true`) {
		t.Fatalf("Not this book: %d %s", resp.StatusCode, out)
	}
	resp, out := b.do(t, "GET", "/api/v1/admin/audible/accounts", b.adminTok, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, `"backup_estimate":{"count":2,"bytes":576000000}`) ||
		!strings.Contains(out, `"template":""`) || !strings.Contains(out, `"policy_since":"`) {
		t.Fatalf("accounts: %d %s", resp.StatusCode, out)
	}
	if resp, out := b.do(t, "POST", backupPath("B000000001"), b.adminTok, ""); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("one first: %d %s", resp.StatusCode, out)
	}
	resp, out = b.do(t, "POST", "/api/v1/admin/audible/accounts/"+testAccount+"/backup-all", b.adminTok, "")
	if resp.StatusCode != http.StatusOK || out != `{"queued":1}`+"\n" {
		t.Fatalf("backup-all: %d %q", resp.StatusCode, out)
	}
	if resp, out := b.do(t, "POST", "/api/v1/admin/audible/accounts/"+testAccount+"/backup-all", b.adminTok, ""); out != `{"queued":0}`+"\n" {
		t.Fatalf("backup-all again: %d %q", resp.StatusCode, out)
	}
	var n int
	if err := b.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM bridge_jobs WHERE kind = ?`, bridge.KindAudibleImport).Scan(&n); err != nil || n != 2 {
		t.Fatalf("import jobs = %d, %v", n, err)
	}
	if x := b.audibleTitle(t, "B000000005"); x.ImportJobID != nil {
		t.Fatal("Not this book was backed up")
	}
	if x := b.audibleTitle(t, "B000000006"); x.ImportJobID != nil || x.Reason != "contested" {
		t.Fatalf("contested title = %+v: backed up by everything", x)
	}
	if resp, out := b.do(t, "POST", backupPath("B000000006"), b.adminTok, ""); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("back up the contested title on its own: %d %s", resp.StatusCode, out)
	}
	if resp, out := b.do(t, "POST", "/api/v1/admin/audible/accounts/unknown01/backup-all", b.adminTok, ""); resp.StatusCode != http.StatusNotFound || codeOf(out) != "unknown_account" {
		t.Fatalf("unknown account: %d %s", resp.StatusCode, out)
	}
}

// The policy acts after a poll's final call: new purchases only, never the
// library the account had; the Bridge's PUT sets the policy only on create;
// removing the account cancels its open backups.
func TestAudiblePolicyAfterPoll(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	b.beat(t, bridge.KindVerify, bridge.KindAudiblePoll, bridge.KindAudibleImport)
	setAudible(t, b.testEnv, b.adminTok, true)
	body := fmt.Sprintf(`{"label":"x","marketplace":"us","user_id":%d,"library_id":%d,"policy":"all","sync":true}`, b.adminID, b.lib.ID)
	if resp, out := b.do(t, "PUT", "/api/v1/bridge/audible/accounts/"+testAccount, b.bridgeTok, body); resp.StatusCode != http.StatusOK || !strings.Contains(out, `"policy":"all"`) {
		t.Fatalf("create: %d %s", resp.StatusCode, out)
	}
	if code, out := b.putTitles(t, testAccount, "p1", true, title("B000000001", "Old", "available", 0, "")); code != http.StatusOK {
		t.Fatalf("p1: %d %s", code, out)
	}
	if x := b.audibleTitle(t, "B000000001"); x.ImportJobID != nil {
		t.Fatal("the first poll's library was backed up")
	}
	// A non-final call queues nothing; the final call does.
	if code, out := b.putTitles(t, testAccount, "p2", false, title("B000000001", "Old", "available", 0, ""),
		title("B000000002", "New", "available", 0, ""), contestedTitle("B000000009", "Tied")); code != http.StatusOK {
		t.Fatalf("p2a: %d %s", code, out)
	}
	if x := b.audibleTitle(t, "B000000002"); x.ImportJobID != nil {
		t.Fatal("queued before the final call")
	}
	if code, out := b.putTitles(t, testAccount, "p2", true); code != http.StatusOK {
		t.Fatalf("p2b: %d %s", code, out)
	}
	x := b.audibleTitle(t, "B000000002")
	if x.ImportJobID == nil {
		t.Fatal("the new purchase wasn't backed up")
	}
	if c := b.audibleTitle(t, "B000000009"); c.ImportJobID != nil {
		t.Fatal("the policy backed up a contested new purchase")
	}
	job, _ := b.api.bridge.Job(context.Background(), *x.ImportJobID)
	if job.Priority != -10 || job.RequestedBy != nil {
		t.Fatalf("policy job = %+v", job)
	}
	// The Bridge's PUT again with manual leaves the console's policy.
	body = strings.Replace(body, `"all"`, `"manual"`, 1)
	if resp, out := b.do(t, "PUT", "/api/v1/bridge/audible/accounts/"+testAccount, b.bridgeTok, body); resp.StatusCode != http.StatusOK || !strings.Contains(out, `"policy":"all"`) {
		t.Fatalf("re-map: %d %s", resp.StatusCode, out)
	}
	// While the module is off, a poll queues nothing.
	setAudible(t, b.testEnv, b.adminTok, false)
	if code, out := b.putTitles(t, testAccount, "p3", true, title("B000000001", "Old", "available", 0, ""),
		title("B000000002", "New", "available", 0, ""), title("B000000003", "Newer", "available", 0, "")); code != http.StatusOK {
		t.Fatalf("p3: %d %s", code, out)
	}
	if x := b.audibleTitle(t, "B000000003"); x.ImportJobID != nil {
		t.Fatal("queued while the module is off")
	}
	// Removed: its open backup is cancelled.
	if resp, out := b.do(t, "DELETE", "/api/v1/bridge/audible/accounts/"+testAccount, b.bridgeTok, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d %s", resp.StatusCode, out)
	}
	if j, err := b.api.bridge.Job(context.Background(), job.ID); err != nil || j.State != bridge.StateCancelled || j.Error != bridge.CancelAudibleRemoved {
		t.Fatalf("backup after removal = %+v, %v", j, err)
	}
}

// A backup's match moves with its book (MoveDurableState), as a poll's does.
func TestAudibleImportMatchMoves(t *testing.T) {
	t.Parallel()
	b := backupEnv(t, title("B000000001", "One", "available", 0, ""))
	ctx := context.Background()
	if resp, out := b.do(t, "POST", backupPath("B000000001"), b.adminTok, ""); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("backup: %d %s", resp.StatusCode, out)
	}
	b.finish(t, b.claim(t, bridge.KindAudibleImport), "done",
		fmt.Sprintf(`{"library_id":%d,"path":%q,"files":[],"bytes":1,"enriched":true}`, b.lib.ID, mistbornBook), "")
	if err := b.cat.MoveDurableState(ctx, b.lib.ID, mistbornBook, "Brandon Sanderson/Mistborn Moved"); err != nil {
		t.Fatal(err)
	}
	x := b.audibleTitle(t, "B000000001")
	if x.Match == nil || x.Match.Path != "Brandon Sanderson/Mistborn Moved" || x.Match.How != catalog.AudibleMatchImport {
		t.Fatalf("match after the move = %+v", x.Match)
	}
}
