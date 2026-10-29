package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// bridgeEnv: the fixture library, an admin's session and API key, a user's
// session, and a Bridge paired through the API and beating.
type bridgeEnv struct {
	*testEnv
	lib                         *catalog.Library
	adminTok, adminKey, userTok string
	bridgeTok                   string
}

func newBridgeEnv(t *testing.T) *bridgeEnv {
	t.Helper()
	return newBridgeEnvOn(t, newTestEnv(t))
}

// newBridgeEnvOn is newBridgeEnv over a test env the caller built (one with
// metadata on, say).
func newBridgeEnvOn(t *testing.T, e *testEnv) *bridgeEnv {
	t.Helper()
	ctx := context.Background()
	b := &bridgeEnv{testEnv: e, lib: newFixtureLibrary(t, e)}
	b.adminTok, _ = e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "console", 0)
	b.adminKey, _ = e.auth.IssueToken(ctx, e.adminID, auth.KindAPI, "cron", 0)
	_, b.userTok = newUserWithSession(t, e, "sam", false)
	b.bridgeTok = b.pair(t)
	b.beat(t, bridge.KindVerify)
	return b
}

// pair makes a code in the console and exchanges it as the Bridge.
func (b *bridgeEnv) pair(t *testing.T) string {
	t.Helper()
	resp, body := b.do(t, "POST", "/api/v1/admin/bridge/pair", b.adminTok, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("pair code: %d %s", resp.StatusCode, body)
	}
	var code struct{ Code, ExpiresAt, ServerURL string }
	decodeInto(t, body, &code)
	resp, body = b.do(t, "POST", "/api/v1/bridge/pair", "", fmt.Sprintf(`{"code":%q}`, code.Code))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("exchange: %d %s", resp.StatusCode, body)
	}
	var tok struct {
		Token      string `json:"token"`
		ServerName string `json:"server_name"`
	}
	decodeInto(t, body, &tok)
	if tok.Token == "" || tok.ServerName == "" {
		t.Fatalf("exchange = %s", body)
	}
	return tok.Token
}

func (b *bridgeEnv) beat(t *testing.T, kinds ...string) {
	t.Helper()
	k, _ := json.Marshal(kinds)
	body := fmt.Sprintf(`{"version":"0.1.0","kinds":%s,"libraries":[{"library_id":%d,"mount_path":"/media/books","mapped":true,"writable":true,"checked_path":%q}],"ui_url":"http://bridge.lan:8095/"}`,
		k, b.lib.ID, mistbornBook)
	resp, out := b.do(t, "PUT", "/api/v1/bridge/status", b.bridgeTok, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", resp.StatusCode, out)
	}
}

func decodeInto(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
}

func (b *bridgeEnv) queue(t *testing.T, path string, params string) bridge.Job {
	t.Helper()
	body := fmt.Sprintf(`{"kind":"bridge.verify","library_id":%d,"path":%q,"params":%s}`, b.lib.ID, path, params)
	resp, out := b.do(t, "POST", "/api/v1/admin/bridge/jobs", b.adminTok, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("queue: %d %s", resp.StatusCode, out)
	}
	var j bridge.Job
	decodeInto(t, out, &j)
	return j
}

// The whole loop through the API: pair, beat, the capability, queue, claim,
// progress, done.
func TestBridgeLoop(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)

	resp, body := b.do(t, "GET", "/api/v1/server", b.userTok, "")
	var info struct {
		Capabilities map[string]bool `json:"capabilities"`
		Bridge       *bridgeInfo     `json:"bridge"`
	}
	decodeInto(t, body, &info)
	if resp.StatusCode != http.StatusOK || !info.Capabilities["bridge"] || info.Bridge == nil ||
		info.Bridge.Version != "0.1.0" || strings.Join(info.Bridge.Features, ",") != "bridge" {
		t.Fatalf("server info = %s", body)
	}

	job := b.queue(t, mistbornBook, `{"hold_seconds":1}`)
	if job.State != bridge.StateQueued || job.Path == nil || *job.Path != mistbornBook {
		t.Fatalf("queued = %+v", job)
	}
	// A second press returns the same job.
	resp, body = b.do(t, "POST", "/api/v1/admin/bridge/jobs", b.adminTok,
		fmt.Sprintf(`{"kind":"bridge.verify","library_id":%d,"path":%q}`, b.lib.ID, mistbornBook+"/"))
	var again bridge.Job
	decodeInto(t, body, &again)
	if resp.StatusCode != http.StatusOK || again.ID != job.ID {
		t.Fatalf("second press: %d %s", resp.StatusCode, body)
	}

	resp, body = b.do(t, "GET", "/api/v1/bridge/jobs/next?wait=1", b.bridgeTok, "")
	var claimed bridge.Job
	decodeInto(t, body, &claimed)
	if resp.StatusCode != http.StatusOK || claimed.ID != job.ID || claimed.State != bridge.StateClaimed || claimed.Attempts != 1 {
		t.Fatalf("claim: %d %s", resp.StatusCode, body)
	}
	resp, _ = b.do(t, "GET", "/api/v1/bridge/jobs/next?wait=0", b.bridgeTok, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("empty claim: %d, want 204", resp.StatusCode)
	}

	patch := func(body string) (*http.Response, bridge.Job) {
		resp, out := b.do(t, "PATCH", fmt.Sprintf("/api/v1/bridge/jobs/%d", job.ID), b.bridgeTok, body)
		var j bridge.Job
		if resp.StatusCode == http.StatusOK {
			decodeInto(t, out, &j)
		}
		return resp, j
	}
	if resp, j := patch(`{"attempt":1,"state":"running","progress":{"stage":"stat"}}`); resp.StatusCode != http.StatusOK || j.State != bridge.StateRunning {
		t.Fatalf("progress: %d %+v", resp.StatusCode, j)
	}
	if resp, _ := patch(`{"attempt":2,"state":"done"}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a stale claim's report: %d, want 409", resp.StatusCode)
	}
	if resp, _ := patch(`{"attempt":1,"state":"queued"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a bad transition: %d, want 400", resp.StatusCode)
	}
	if resp, j := patch(`{"attempt":1,"state":"done","result":{"files":2}}`); resp.StatusCode != http.StatusOK || j.State != bridge.StateDone {
		t.Fatalf("done: %d %+v", resp.StatusCode, j)
	}

	resp, body = b.do(t, "GET", fmt.Sprintf("/api/v1/admin/bridge/jobs?library_id=%d&path=%s", b.lib.ID, url.QueryEscape(mistbornBook)), b.adminTok, "")
	var list struct{ Jobs []bridge.Job }
	decodeInto(t, body, &list)
	if resp.StatusCode != http.StatusOK || len(list.Jobs) != 1 || list.Jobs[0].State != bridge.StateDone ||
		string(list.Jobs[0].Result) != `{"files":2}` || list.Jobs[0].RequestedBy == nil {
		t.Fatalf("list: %d %s", resp.StatusCode, body)
	}

	resp, body = b.do(t, "GET", "/api/v1/admin/bridge", b.adminTok, "")
	var st adminBridge
	decodeInto(t, body, &st)
	if resp.StatusCode != http.StatusOK || !st.Paired || !st.Online || st.PairedBy == nil || st.UIURL != "http://bridge.lan:8095/" ||
		len(st.Libraries) != 1 || st.Libraries[0].Mapping == nil || !st.Libraries[0].Mapping.Writable {
		t.Fatalf("admin bridge: %d %s", resp.StatusCode, body)
	}
}

// The capability is on only while the heartbeat is under two minutes old.
func TestBridgeCapabilityGoesStale(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	old := time.Now().Add(-bridge.OnlineWindow - time.Second).UTC().Format(time.RFC3339)
	if _, err := b.db.ExecContext(context.Background(), `UPDATE bridge_status SET heartbeat_at = ?`, old); err != nil {
		t.Fatal(err)
	}
	b.api.bridge.Forget() // the heartbeat cache would answer for a few seconds more
	_, body := b.do(t, "GET", "/api/v1/server", b.userTok, "")
	var info map[string]any
	decodeInto(t, body, &info)
	if caps := info["capabilities"].(map[string]any); caps["bridge"] != false {
		t.Fatalf("stale bridge capability = %v", caps["bridge"])
	}
	if _, ok := info["bridge"]; ok {
		t.Fatalf("stale bridge block: %s", body)
	}
	// A stale Bridge's kinds still take jobs: they wait for it.
	b.queue(t, mistbornBook, `{}`)
}

func TestBridgeCancelAndLeaseExpiry(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	queued := b.queue(t, mistbornBook, `{}`)
	resp, body := b.do(t, "POST", fmt.Sprintf("/api/v1/admin/bridge/jobs/%d/cancel", queued.ID), b.adminTok, "")
	var j bridge.Job
	decodeInto(t, body, &j)
	if resp.StatusCode != http.StatusOK || j.State != bridge.StateCancelled {
		t.Fatalf("cancel queued: %d %s", resp.StatusCode, body)
	}
	if resp, _ := b.do(t, "POST", fmt.Sprintf("/api/v1/admin/bridge/jobs/%d/cancel", queued.ID), b.adminTok, ""); resp.StatusCode != http.StatusConflict {
		t.Fatalf("cancel twice: %d, want 409", resp.StatusCode)
	}

	job := b.queue(t, cradleBook, `{}`)
	if resp, _ := b.do(t, "GET", "/api/v1/bridge/jobs/next", b.bridgeTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("claim: %d", resp.StatusCode)
	}
	// The Bridge dies: its lease runs out, and the next look requeues the job.
	past := time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
	if _, err := b.db.ExecContext(context.Background(), `UPDATE bridge_jobs SET lease_until = ? WHERE id = ?`, past, job.ID); err != nil {
		t.Fatal(err)
	}
	resp, body = b.do(t, "GET", fmt.Sprintf("/api/v1/admin/bridge/jobs/%d", job.ID), b.adminTok, "")
	decodeInto(t, body, &j)
	if resp.StatusCode != http.StatusOK || j.State != bridge.StateQueued {
		t.Fatalf("after the lease: %d %s", resp.StatusCode, body)
	}
	resp, body = b.do(t, "GET", "/api/v1/bridge/jobs/next", b.bridgeTok, "")
	decodeInto(t, body, &j)
	if resp.StatusCode != http.StatusOK || j.ID != job.ID || j.Attempts != 2 {
		t.Fatalf("reclaim: %d %s", resp.StatusCode, body)
	}
}

// A claim can narrow the kinds it takes (the Bridge's free lanes): a kind it
// doesn't ask for stays queued, and asking for none it runs answers 204 at once.
func TestBridgeClaimNarrowsKinds(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	job := b.queue(t, mistbornBook, `{}`)
	start := time.Now()
	resp, _ := b.do(t, "GET", "/api/v1/bridge/jobs/next?wait=5&kind=sidecars.generate", b.bridgeTok, "")
	if resp.StatusCode != http.StatusNoContent || time.Since(start) > 2*time.Second {
		t.Fatalf("a kind it doesn't run: %d after %v, want an immediate 204", resp.StatusCode, time.Since(start))
	}
	resp, body := b.do(t, "GET", "/api/v1/bridge/jobs/next?wait=1&kind=sidecars.generate&kind=bridge.verify", b.bridgeTok, "")
	var j bridge.Job
	decodeInto(t, body, &j)
	if resp.StatusCode != http.StatusOK || j.ID != job.ID {
		t.Fatalf("asking for its kind: %d %s", resp.StatusCode, body)
	}
}

func TestBridgeQueueChecks(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	for name, c := range map[string]struct {
		body string
		want int
	}{
		"unknown kind":      {`{"kind":"files.delete","library_id":%d}`, http.StatusBadRequest},
		"bad params":        {`{"kind":"bridge.verify","library_id":%d,"params":{"cmd":"rm"}}`, http.StatusBadRequest},
		"no such book":      {`{"kind":"bridge.verify","library_id":%d,"path":"Nobody/Nothing"}`, http.StatusNotFound},
		"hidden path":       {`{"kind":"bridge.verify","library_id":%d,"path":"Brandon Sanderson/Mistborn/.audiosilo"}`, http.StatusNotFound},
		"no such library":   {`{"kind":"bridge.verify","library_id":9%d}`, http.StatusNotFound},
		"library itself ok": {`{"kind":"bridge.verify","library_id":%d}`, http.StatusCreated},
	} {
		resp, body := b.do(t, "POST", "/api/v1/admin/bridge/jobs", b.adminTok, fmt.Sprintf(c.body, b.lib.ID))
		if resp.StatusCode != c.want {
			t.Errorf("%s: %d %s, want %d", name, resp.StatusCode, body, c.want)
		}
	}
	// A kind the Bridge doesn't run is refused even when the server knows it.
	b.beat(t)
	resp, body := b.do(t, "POST", "/api/v1/admin/bridge/jobs", b.adminTok, fmt.Sprintf(`{"kind":"bridge.verify","library_id":%d,"path":%q}`, b.lib.ID, cradleBook))
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "kind_unavailable") {
		t.Fatalf("unoffered kind: %d %s", resp.StatusCode, body)
	}
}

func TestBridgeBookForMappingCheck(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	resp, body := b.do(t, "GET", fmt.Sprintf("/api/v1/bridge/libraries/%d/book?path=%s", b.lib.ID, url.QueryEscape(mistbornBook)), b.bridgeTok, "")
	var out struct {
		Path  string
		Files []bridgeFile
	}
	decodeInto(t, body, &out)
	if resp.StatusCode != http.StatusOK || out.Path != mistbornBook || len(out.Files) == 0 {
		t.Fatalf("book: %d %s", resp.StatusCode, body)
	}
	info, err := os.Stat(b.lib.Root + "/" + out.Files[0].Path)
	if err != nil || out.Files[0].Size != info.Size() || out.Files[0].ModTime != info.ModTime().Unix() {
		t.Fatalf("file = %+v, disk = %v %v", out.Files[0], info, err)
	}
	// No path: a sample book.
	resp, body = b.do(t, "GET", fmt.Sprintf("/api/v1/bridge/libraries/%d/book", b.lib.ID), b.bridgeTok, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"files"`) {
		t.Fatalf("sample: %d %s", resp.StatusCode, body)
	}
	// Never an on-demand index: an unindexed path is 404.
	resp, _ = b.do(t, "GET", fmt.Sprintf("/api/v1/bridge/libraries/%d/book?path=Nobody", b.lib.ID), b.bridgeTok, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unindexed: %d", resp.StatusCode)
	}
}

// The mapping check's sample skips a book whose files are gone from the
// server's disk (deleted before a scan pruned it), so one stale entry doesn't
// make the whole library look unmapped.
func TestBridgeSampleBookSkipsAMissingBook(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	root := copyFixtures(t, "Brandon Sanderson/Mistborn/01 - The Final Empire.m4b",
		"Will Wight/Cradle/01 - Unsouled.m4b", "Will Wight/Cradle/02 - Soulsmith.m4b")
	if _, err := b.db.ExecContext(context.Background(), `UPDATE libraries SET root = ? WHERE id = ?`, root, b.lib.ID); err != nil {
		t.Fatal(err)
	}
	sample := func() string {
		t.Helper()
		resp, body := b.do(t, "GET", fmt.Sprintf("/api/v1/bridge/libraries/%d/book", b.lib.ID), b.bridgeTok, "")
		var out struct{ Path string }
		decodeInto(t, body, &out)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("sample: %d %s", resp.StatusCode, body)
		}
		return out.Path
	}
	first := sample()
	if err := os.RemoveAll(filepath.Join(root, filepath.FromSlash(first))); err != nil {
		t.Fatal(err)
	}
	if next := sample(); next == first || next == "" {
		t.Fatalf("the sample after %q's files went = %q, want another book", first, next)
	}
}

// A single-file book has no book_files rows: the mapping check gets the book
// itself as its one file, with its size.
func TestBridgeBookSingleFile(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	root := copyFixtures(t, "Brandon Sanderson/Mistborn/01 - The Final Empire.m4b")
	if err := os.Rename(filepath.Join(root, "Brandon Sanderson", "Mistborn", "01 - The Final Empire.m4b"), filepath.Join(root, "Loose.m4b")); err != nil {
		t.Fatal(err)
	}
	lib, err := b.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Loose", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	scanFixture(t, b.testEnv, lib)
	resp, body := b.do(t, "GET", fmt.Sprintf("/api/v1/bridge/libraries/%d/book", lib.ID), b.bridgeTok, "")
	var out struct {
		Path  string
		Files []bridgeFile
	}
	decodeInto(t, body, &out)
	info, _ := os.Stat(filepath.Join(root, "Loose.m4b"))
	if resp.StatusCode != http.StatusOK || out.Path != "Loose.m4b" || len(out.Files) != 1 ||
		out.Files[0].Path != "Loose.m4b" || out.Files[0].Size != info.Size() {
		t.Fatalf("single-file book: %d %s", resp.StatusCode, body)
	}
}

func TestBridgeRescan(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	resp, body := b.do(t, "POST", "/api/v1/bridge/rescan", b.bridgeTok, fmt.Sprintf(`{"library_id":%d,"path":%q}`, b.lib.ID, mistbornBook))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, mistbornBook) {
		t.Fatalf("rescan: %d %s", resp.StatusCode, body)
	}
	for _, p := range []string{"../etc", mistbornBook + "/.audiosilo", "Nobody"} {
		resp, body := b.do(t, "POST", "/api/v1/bridge/rescan", b.bridgeTok, fmt.Sprintf(`{"library_id":%d,"path":%q}`, b.lib.ID, p))
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("rescan %q: %d %s, want 404", p, resp.StatusCode, body)
		}
	}
}

// Security-critical: the Bridge's routes take a bridge token and nothing else.
func TestBridgeRoutesRefuseOtherCredentials(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	routes := []struct{ method, path, body string }{
		{"PUT", "/api/v1/bridge/status", `{"version":"x"}`},
		{"GET", fmt.Sprintf("/api/v1/bridge/libraries/%d/book", b.lib.ID), ""},
		{"GET", "/api/v1/bridge/jobs/next", ""},
		{"PATCH", "/api/v1/bridge/jobs/1", `{"attempt":1}`},
		{"POST", "/api/v1/bridge/rescan", fmt.Sprintf(`{"library_id":%d,"path":%q}`, b.lib.ID, mistbornBook)},
	}
	for _, r := range routes {
		for name, tok := range map[string]string{"none": "", "admin session": b.adminTok, "admin api key": b.adminKey, "user": b.userTok, "garbage": "nope"} {
			if resp, body := b.do(t, r.method, r.path, tok, r.body); resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with %s: %d %s, want 401", r.method, r.path, name, resp.StatusCode, body)
			}
		}
		// Allowed: the bridge token gets past authentication.
		if resp, body := b.do(t, r.method, r.path, b.bridgeTok, r.body); resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			t.Errorf("%s %s with the bridge token: %d %s", r.method, r.path, resp.StatusCode, body)
		}
	}
	// Unpaired, the token is dead everywhere.
	if resp, _ := b.do(t, "DELETE", "/api/v1/admin/bridge", b.adminTok, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unpair: %d", resp.StatusCode)
	}
	if resp, _ := b.do(t, "PUT", "/api/v1/bridge/status", b.bridgeTok, `{}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("heartbeat after unpair: %d, want 401", resp.StatusCode)
	}
}

// Security-critical: the console's Bridge routes are admin-only, and the bridge
// token isn't an admin credential.
func TestBridgeAdminRoutesAreAdminOnly(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	routes := []struct{ method, path, body string }{
		{"GET", "/api/v1/admin/bridge", ""},
		{"POST", "/api/v1/admin/bridge/pair", ""},
		{"GET", "/api/v1/admin/bridge/jobs", ""},
		{"POST", "/api/v1/admin/bridge/jobs", fmt.Sprintf(`{"kind":"bridge.verify","library_id":%d}`, b.lib.ID)},
		{"GET", "/api/v1/admin/bridge/jobs/1", ""},
		{"POST", "/api/v1/admin/bridge/jobs/1/cancel", ""},
		{"GET", fmt.Sprintf("/api/v1/admin/bridge/estimate?library_id=%d&path=%s", b.lib.ID, url.QueryEscape(mistbornBook)), ""},
		{"DELETE", "/api/v1/admin/bridge", ""}, // last: it unpairs
	}
	for _, r := range routes {
		if resp, _ := b.do(t, r.method, r.path, b.userTok, r.body); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s as a user: %d, want 403", r.method, r.path, resp.StatusCode)
		}
		for name, tok := range map[string]string{"none": "", "bridge": b.bridgeTok} {
			if resp, _ := b.do(t, r.method, r.path, tok, r.body); resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with %s: %d, want 401", r.method, r.path, name, resp.StatusCode)
			}
		}
		if resp, body := b.do(t, r.method, r.path, b.adminTok, r.body); resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusConflict {
			t.Errorf("%s %s as the admin: %d %s", r.method, r.path, resp.StatusCode, body)
		}
	}
	// An API key may not make a pairing code (a key never mints a credential).
	if resp, _ := b.do(t, "POST", "/api/v1/admin/bridge/pair", b.adminKey, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("pair code with an api key: %d, want 403", resp.StatusCode)
	}
}

// Security-critical: a bridge token is refused on every other authenticated
// route. Routes opt in to the kinds they take, so this holds by construction;
// the test reads the route table so a new route can't quietly break it.
func TestBridgeTokenRefusedOnEveryOtherRoute(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	// Two requests per route: more than one address's burst.
	b.api.ipLimiter = newRateLimiter(100000, 100000)
	b.api.mediaLimiter = newRateLimiter(100000, 100000)
	src, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatal(err)
	}
	route := regexp.MustCompile(`mux\.Handle\("(GET|POST|PUT|PATCH|DELETE) (/api/v1/[^"]*)",\s*a\.(require\w+)\(`)
	matches := route.FindAllStringSubmatch(string(src), -1)
	if len(matches) < 150 {
		t.Fatalf("found only %d authenticated routes: the pattern no longer matches api.go", len(matches))
	}
	ids := regexp.MustCompile(`\{[a-z_]+\}`)
	checked := 0
	for _, m := range matches {
		method, path, wrapper := m[1], m[2], m[3]
		if wrapper == "requireBridge" {
			continue
		}
		p := ids.ReplaceAllString(path, "1") + "?path=" + url.QueryEscape(mistbornBook)
		resp, body := b.do(t, method, p, b.bridgeTok, `{}`)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s (%s) with the bridge token: %d %s, want 401", method, path, wrapper, resp.StatusCode, body)
		}
		// The same request with the token as ?token= (media routes read it there).
		resp, _ = b.do(t, method, p+"&token="+url.QueryEscape(b.bridgeTok), "", `{}`)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s (%s) with ?token=bridge: %d, want 401", method, path, wrapper, resp.StatusCode)
		}
		checked++
	}
	t.Logf("checked %d routes", checked)
}

func TestBridgeExchangeIsRateLimited(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	for i := range 10 {
		if resp, _ := b.do(t, "POST", "/api/v1/bridge/pair", "", `{"code":"WRONG-WRONG"}`); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong code %d: %d, want 401", i, resp.StatusCode)
		}
	}
	if resp, _ := b.do(t, "POST", "/api/v1/bridge/pair", "", `{"code":"WRONG-WRONG"}`); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("eleventh wrong code: %d, want 429", resp.StatusCode)
	}
}

// An open job follows its book on a move.
func TestBridgeJobsMoveWithTheBook(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	job := b.queue(t, mistbornBook, `{}`)
	ctx := context.Background()
	if err := b.cat.MoveDurableState(ctx, b.lib.ID, mistbornBook, "Brandon Sanderson/Mistborn Era 1"); err != nil {
		t.Fatal(err)
	}
	got, err := b.api.bridge.Job(ctx, job.ID)
	if err != nil || got.Path == nil || *got.Path != "Brandon Sanderson/Mistborn Era 1" || got.State != bridge.StateQueued {
		t.Fatalf("moved job = %+v, %v", got, err)
	}
	// A second press at the new path finds it (its dedupe key moved too).
	moved := "Brandon Sanderson/Mistborn Era 1"
	again, created, err := b.api.bridge.Enqueue(ctx, bridge.NewJob{Kind: bridge.KindVerify, LibraryID: &b.lib.ID, Path: &moved, Params: json.RawMessage(`{"hold_seconds":0}`)})
	if err != nil || created || again.ID != job.ID {
		t.Fatalf("press at the new path = %+v, %v, %v", again, created, err)
	}
}

// Hidden folders can't be opened or indexed by name (the scan never walks
// them, so a request must not either).
func TestHiddenFoldersAreNotIndexedOnDemand(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	root := copyFixtures(t, cradlePart)
	hidden := root + "/Will Wight/Cradle/.audiosilo"
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	copyFixture(t, root, cradlePart, "Will Wight/Cradle/.audiosilo/chunk.m4b")
	lib, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Copy", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	scanFixture(t, e, lib)
	tok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	for _, p := range []string{"/item?path=", "/fs?path="} {
		for _, rel := range []string{"Will Wight/Cradle/.audiosilo/chunk.m4b", "Will Wight/Cradle/.audiosilo"} {
			resp, body := e.do(t, "GET", fmt.Sprintf("/api/v1/libraries/%d%s%s", lib.ID, p, url.QueryEscape(rel)), tok, "")
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("GET %s%s: %d %s, want 404", p, rel, resp.StatusCode, body)
			}
		}
	}
	// Allowed: the book itself still opens.
	if resp, body := e.do(t, "GET", fmt.Sprintf("/api/v1/libraries/%d/item?path=%s", lib.ID, url.QueryEscape(cradlePart)), tok, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("the book: %d %s", resp.StatusCode, body)
	}
	if counts, err := e.cat.CountBooksByLibrary(context.Background()); err != nil || counts[lib.ID] != 1 {
		t.Errorf("books = %v, %v; want 1 (no phantom)", counts, err)
	}
}

// claimAsync starts a long-poll claim (raw, off the test goroutine) and returns
// the channel its status code arrives on (0 for a transport error).
func claimAsync(base, token string, wait int) <-chan int {
	got := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/v1/bridge/jobs/next?wait=%d", base, wait), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			got <- 0
			return
		}
		resp.Body.Close()
		got <- resp.StatusCode
	}()
	return got
}

// A paired Bridge always has a claim waiting; it must not hold up the server's
// graceful stop (Shutdown never cancels a request), so the wait ends with the
// server's own context.
func TestBridgeClaimEndsWithTheServer(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	base, stop := context.WithCancel(context.Background())
	defer stop()
	b.api.SetBaseContext(base)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	active := make(chan struct{}, 1)
	srv := &http.Server{Handler: b.api.Handler(), ConnState: func(_ net.Conn, st http.ConnState) {
		if st == http.StateActive {
			select {
			case active <- struct{}{}:
			default:
			}
		}
	}}
	go func() { _ = srv.Serve(ln) }()
	got := claimAsync("http://"+ln.Addr().String(), b.bridgeTok, 20)
	<-active                           // the claim is in
	time.Sleep(100 * time.Millisecond) // and waiting
	stop()                             // the launcher's context ends, then it shuts the server down
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shut); err != nil {
		t.Fatalf("graceful shutdown with a claim waiting: %v", err)
	}
	if code := <-got; code != http.StatusNoContent {
		t.Fatalf("the waiting claim answered %d, want 204", code)
	}
}

// Security-critical: pairing another Bridge revokes the first one's token, and a
// claim it had waiting (authenticated before) hands it nothing: 401, the job
// stays queued for the new Bridge.
func TestBridgeReplacedBridgeCannotClaim(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	old := b.bridgeTok
	got := claimAsync(b.srv.URL, old, 10)
	time.Sleep(200 * time.Millisecond) // the old Bridge's claim is waiting
	b.bridgeTok = b.pair(t)
	b.beat(t, bridge.KindVerify)
	job := b.queue(t, mistbornBook, `{}`)
	if code := <-got; code != http.StatusUnauthorized {
		t.Fatalf("denied: the replaced Bridge's waiting claim answered %d, want 401", code)
	}
	resp, body := b.do(t, "GET", "/api/v1/bridge/jobs/next", b.bridgeTok, "")
	var j bridge.Job
	decodeInto(t, body, &j)
	if resp.StatusCode != http.StatusOK || j.ID != job.ID || j.Attempts != 1 {
		t.Fatalf("allowed: the new Bridge's claim: %d %s", resp.StatusCode, body)
	}
}

// A heartbeat naming no kind the server knows still answers features as a list
// (the console reads its length), and a report on a job that is gone is 409
// not_held, the code that makes the Bridge drop it.
func TestBridgeEmptyFeaturesAndGoneJob(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	b.beat(t)
	for _, path := range []string{"/api/v1/admin/bridge", "/api/v1/server"} {
		_, body := b.do(t, "GET", path, b.adminTok, "")
		if !strings.Contains(body, `"features":[]`) {
			t.Errorf("GET %s: %s, want features []", path, body)
		}
	}
	resp, body := b.do(t, "PATCH", "/api/v1/bridge/jobs/999", b.bridgeTok, `{"attempt":1,"state":"failed"}`)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "not_held") {
		t.Fatalf("report on a gone job: %d %s, want 409 not_held", resp.StatusCode, body)
	}
}

// GET /server answers on an API built without SetBridge (no Bridge, no flag).
func TestServerInfoWithoutBridge(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	a := New(config.Default(t.TempDir()), auth.New(db, time.Now), cat, library.NewScanner(cat, "", slog.Default()), "", slog.Default())
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/api/v1/server")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var info struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil || resp.StatusCode != http.StatusOK || info.Capabilities["bridge"] {
		t.Fatalf("GET /server = %d, %+v, %v", resp.StatusCode, info, err)
	}
}

// Security-critical: a 401 stops the Bridge calling in (it retries every few
// minutes), so only a refused token is one; a token the server can't check (its database failing)
// is a 503 the Bridge retries.
func TestBridgeTokenCheckFailureIsNotUnauthorized(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	if resp, _ := b.do(t, "PUT", "/api/v1/bridge/status", "nope", `{"version":"0.1.0"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("denied: a refused token = %d, want 401", resp.StatusCode)
	}
	// A heartbeat leaves the content generation (server#258), so the bridge
	// token is still in the token cache; a content write empties it, and the
	// next request checks the token against the closed database.
	if _, err := b.db.ExecContext(context.Background(), `UPDATE libraries SET name = name`); err != nil {
		t.Fatal(err)
	}
	if err := b.db.Close(); err != nil {
		t.Fatal(err)
	}
	if resp, body := b.do(t, "PUT", "/api/v1/bridge/status", b.bridgeTok, `{"version":"0.1.0"}`); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a token the server couldn't check = %d %s, want 503", resp.StatusCode, body)
	}
}
