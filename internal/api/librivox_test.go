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

// LibriVox L6: Add from LibriVox (handlers_librivox.go).

// librivoxWorkBody is the mock's works/the-martian with its one recording a
// LibriVox project (47) and a store recording beside it, which the work route
// leaves out.
var librivoxWorkBody = strings.Replace(martianWorkBody, `"recordings":[{"id":"rec1",`,
	`"recordings":[{"id":"store1","narrators":[],"runtime_min":600,"asin":[{"region":"us","asin":"B000000009"}]},`+
		`{"id":"rec1","librivox_id":47,"links":{"librivox":"https://librivox.org/the-martian/"},`, 1)

// librivoxEnv is a bridgeEnv with metadata on (the mock serves the LibriVox
// work above; lookup answers the-martian's rec1 for any id) and a Bridge that
// runs librivox.fetch and can write to the library.
func librivoxEnv(t *testing.T, m *mockMetaserve) *bridgeEnv {
	t.Helper()
	if m == nil {
		m = &mockMetaserve{work: librivoxWorkBody}
	}
	b := newBridgeEnvOn(t, newMetaEnvMock(t, true, m))
	b.beat(t, bridge.KindVerify, bridge.KindLibrivoxFetch)
	return b
}

func (b *bridgeEnv) setMetadata(t *testing.T, on bool) {
	t.Helper()
	body := fmt.Sprintf(`{"metadata":{"enabled":%t}}`, on)
	if resp, out := b.do(t, "PATCH", "/api/v1/admin/settings", b.adminTok, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("metadata %t: %d %s", on, resp.StatusCode, out)
	}
}

func librivoxBody(libID int64, project int) string {
	return fmt.Sprintf(`{"library_id":%d,"librivox_id":%d}`, libID, project)
}

// Security-critical: the three routes are admin-only (a listener's session or
// API key 403), refuse no token and the bridge, cast and feed tokens (401),
// and answer 404 metadata_off while the lookup is off, before anything is
// queued.
func TestLibrivoxRoutesAuth(t *testing.T) {
	t.Parallel()
	b := librivoxEnv(t, nil)
	ctx := context.Background()
	cast, _ := b.auth.IssueToken(ctx, b.adminID, auth.KindCast, "cast", 0)
	feed, _ := b.auth.IssueToken(ctx, b.adminID, auth.KindFeed, "feed", 0)
	var userID int64
	if err := b.db.QueryRowContext(ctx, `SELECT id FROM users WHERE username = 'sam'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	userKey, _ := b.auth.IssueToken(ctx, userID, auth.KindAPI, "key", 0)
	routes := []struct{ method, path, body string }{
		{"GET", "/api/v1/admin/librivox/search?q=martian", ""},
		{"GET", "/api/v1/admin/librivox/work?id=the-martian", ""},
		{"POST", "/api/v1/admin/librivox/fetch", librivoxBody(b.lib.ID, 47)},
	}
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
				sep := map[bool]string{true: "&", false: "?"}[strings.Contains(r.path, "?")]
				if resp, _ := b.do(t, r.method, r.path+sep+"token="+url.QueryEscape(tok), "", r.body); resp.StatusCode != http.StatusUnauthorized {
					t.Errorf("%s %s with ?token=%s: %d, want 401", r.method, r.path, name, resp.StatusCode)
				}
			}
		}
	}
	b.setMetadata(t, false)
	for _, r := range routes {
		if resp, body := b.do(t, r.method, r.path, b.adminTok, r.body); resp.StatusCode != http.StatusNotFound || codeOf(body) != codeMetadataOff {
			t.Errorf("%s %s while off: %d %s, want 404 metadata_off", r.method, r.path, resp.StatusCode, body)
		}
	}
	b.setMetadata(t, true)
	var jobs int
	if err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM bridge_jobs WHERE kind = ?`, bridge.KindLibrivoxFetch).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("a refused caller queued %d fetches (%v)", jobs, err)
	}
	// The admin's session and API key get through.
	for name, tok := range map[string]string{"session": b.adminTok, "api key": b.adminKey} {
		for _, r := range routes {
			if resp, body := b.do(t, r.method, r.path, tok, r.body); resp.StatusCode >= 300 {
				t.Errorf("%s %s as the admin's %s: %d %s", r.method, r.path, name, resp.StatusCode, body)
			}
		}
	}
}

// The whole flow: search, the work's LibriVox recordings, Add (one job per
// library and project, audited once), the Bridge's done job records the placed
// book as the recording (replacing a stale row at that path, another path's
// untouched), the work then lists it in the library, and another Add is 409
// in_library.
func TestLibrivoxFetchFlow(t *testing.T) {
	t.Parallel()
	b := librivoxEnv(t, nil)
	ctx := context.Background()

	resp, out := b.do(t, "GET", "/api/v1/admin/librivox/search?q=martian&lang=en&limit=5", b.adminTok, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, `"results":[{"work_id":"the-martian","title":"The Martian"`) {
		t.Fatalf("search = %d %s", resp.StatusCode, out)
	}

	work := func() string {
		t.Helper()
		resp, out := b.do(t, "GET", "/api/v1/admin/librivox/work?id=the-martian", b.adminTok, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("work = %d %s", resp.StatusCode, out)
		}
		return out
	}
	out = work()
	for _, want := range []string{`"work":{"id":"the-martian","title":"The Martian"`, `"recording_id":"rec1","librivox_id":47`,
		`"url":"https://librivox.org/the-martian/"`, `"in_library":[],"job":null`} {
		if !strings.Contains(out, want) {
			t.Fatalf("work missing %s: %s", want, out)
		}
	}
	if strings.Contains(out, "store1") {
		t.Fatalf("a store recording was listed: %s", out)
	}

	// A stale match at the book's path (a deleted book's leftover) and a
	// neighbour's work-level match of the same work: neither is "in library".
	if _, err := b.db.ExecContext(ctx, `INSERT INTO book_matches(library_id, path, work_id, recording_id, how, set_at)
		VALUES(?, ?, 'old-work', 'old-rec', 'auto', 't'), (?, ?, 'the-martian', '', 'accepted', 't')`,
		b.lib.ID, mistbornBook, b.lib.ID, cradleBook); err != nil {
		t.Fatal(err)
	}

	resp, out = b.do(t, "POST", "/api/v1/admin/librivox/fetch", b.adminTok, librivoxBody(b.lib.ID, 47))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("fetch = %d %s", resp.StatusCode, out)
	}
	var queued struct{ Job bridge.Job }
	decodeInto(t, out, &queued)
	want := fmt.Sprintf(`{"librivox_id":47,"work_id":"the-martian","recording_id":"rec1","library_id":%d}`, b.lib.ID)
	if queued.Job.Kind != bridge.KindLibrivoxFetch || string(queued.Job.Params) != want || queued.Job.RequestedBy == nil {
		t.Fatalf("job = %+v %s", queued.Job, queued.Job.Params)
	}
	resp, out = b.do(t, "POST", "/api/v1/admin/librivox/fetch", b.adminTok, librivoxBody(b.lib.ID, 47))
	var again struct{ Job bridge.Job }
	decodeInto(t, out, &again)
	if resp.StatusCode != http.StatusAccepted || again.Job.ID != queued.Job.ID {
		t.Fatalf("a second Add = %d %s, want the open job", resp.StatusCode, out)
	}
	var audited int
	if err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE action = 'bridge.job' AND details LIKE '%librivox.fetch%'`).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("audited %d times (%v), want once", audited, err)
	}
	if out := work(); !strings.Contains(out, fmt.Sprintf(`"in_library":[],"job":{"id":%d,"state":"queued"}`, queued.Job.ID)) {
		t.Fatalf("work while queued: %s", out)
	}

	j := b.claim(t, bridge.KindLibrivoxFetch)
	if j.ID != queued.Job.ID {
		t.Fatalf("claimed %d, want %d", j.ID, queued.Job.ID)
	}
	b.finish(t, j, bridge.StateDone,
		fmt.Sprintf(`{"library_id":%d,"path":%q,"librivox_id":47,"files":3,"bytes":1000}`, b.lib.ID, mistbornBook), "")
	m, err := b.cat.BookMatchAt(ctx, b.lib.ID, mistbornBook)
	if err != nil || m == nil || m.WorkID != "the-martian" || m.RecordingID != "rec1" || m.How != catalog.MatchHowAccepted {
		t.Fatalf("placed book's match = %+v, %v", m, err)
	}
	var setBy int64
	if err := b.db.QueryRowContext(ctx, `SELECT set_by FROM book_matches WHERE library_id = ? AND path = ?`, b.lib.ID, mistbornBook).Scan(&setBy); err != nil || setBy != b.adminID {
		t.Fatalf("set_by = %d (%v), want the admin %d", setBy, err, b.adminID)
	}
	if m, _ := b.cat.BookMatchAt(ctx, b.lib.ID, cradleBook); m == nil || m.WorkID != "the-martian" || m.RecordingID != "" {
		t.Fatalf("the neighbour's match changed: %+v", m)
	}
	out = work()
	if !strings.Contains(out, fmt.Sprintf(`"in_library":[{"library_id":%d,"path":%q}],"job":null`, b.lib.ID, mistbornBook)) {
		t.Fatalf("work after the fetch: %s", out)
	}
	resp, out = b.do(t, "POST", "/api/v1/admin/librivox/fetch", b.adminTok, librivoxBody(b.lib.ID, 47))
	if resp.StatusCode != http.StatusConflict || codeOf(out) != "in_library" ||
		!strings.Contains(out, fmt.Sprintf(`"library_id":%d`, b.lib.ID)) || !strings.Contains(out, `"path":"`+mistbornBook+`"`) {
		t.Fatalf("Add again = %d %s, want 409 in_library", resp.StatusCode, out)
	}
}

// The refusals: a malformed body, an unknown or podcast library, a project the
// metadata doesn't know, the metadata service down, a Bridge that doesn't run
// the kind; and the search's own checks.
func TestLibrivoxRefusals(t *testing.T) {
	t.Parallel()
	b := librivoxEnv(t, nil)
	post := func(body string) (int, string) {
		resp, out := b.do(t, "POST", "/api/v1/admin/librivox/fetch", b.adminTok, body)
		return resp.StatusCode, out
	}
	for _, body := range []string{`{}`, `{"library_id":1}`, fmt.Sprintf(`{"library_id":%d,"librivox_id":-4}`, b.lib.ID),
		fmt.Sprintf(`{"library_id":%d,"librivox_id":47,"path":"x"}`, b.lib.ID), `nope`} {
		if code, out := post(body); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", body, code, out)
		}
	}
	if code, out := post(librivoxBody(9999, 47)); code != http.StatusNotFound {
		t.Errorf("unknown library: %d %s", code, out)
	}
	pods, err := b.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Pods", Root: t.TempDir(), Kind: catalog.KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	if code, out := post(librivoxBody(pods.ID, 47)); code != http.StatusConflict || codeOf(out) != "not_book_library" {
		t.Errorf("podcast library: %d %s", code, out)
	}
	b.beat(t, bridge.KindVerify)
	if code, out := post(librivoxBody(b.lib.ID, 47)); code != http.StatusConflict || codeOf(out) != "kind_unavailable" {
		t.Errorf("no Bridge running it: %d %s", code, out)
	}

	for name, tc := range map[string]struct {
		code int
		want int
		wcod string
	}{
		"unknown project": {http.StatusNotFound, http.StatusNotFound, "not_found"},
		"service down":    {http.StatusInternalServerError, http.StatusBadGateway, ""},
	} {
		e := librivoxEnv(t, &mockMetaserve{work: librivoxWorkBody, lookupCode: tc.code})
		resp, out := e.do(t, "POST", "/api/v1/admin/librivox/fetch", e.adminTok, librivoxBody(e.lib.ID, 47))
		if resp.StatusCode != tc.want || codeOf(out) != tc.wcod {
			t.Errorf("%s: %d %s", name, resp.StatusCode, out)
		}
	}

	for _, q := range []string{"", "?q=", "?q=x&lang=english!", "?q=x&limit=0", "?q=x&limit=51", "?q=x&limit=ten",
		"?q=" + strings.Repeat("x", maxMatchQuery+1)} {
		if resp, out := b.do(t, "GET", "/api/v1/admin/librivox/search"+q, b.adminTok, ""); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("search %q: %d %s, want 400", q, resp.StatusCode, out)
		}
	}
	if resp, out := b.do(t, "GET", "/api/v1/admin/librivox/search?q=down", b.adminTok, ""); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("search while down: %d %s", resp.StatusCode, out)
	}
	if resp, out := b.do(t, "GET", "/api/v1/admin/librivox/work?id=unknown", b.adminTok, ""); resp.StatusCode != http.StatusNotFound || codeOf(out) != "not_found" {
		t.Errorf("unknown work: %d %s", resp.StatusCode, out)
	}
	for _, q := range []string{"", "?id=", "?id=a%0Ab"} {
		if resp, out := b.do(t, "GET", "/api/v1/admin/librivox/work"+q, b.adminTok, ""); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("work %q: %d %s, want 400", q, resp.StatusCode, out)
		}
	}
}
