package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/audible"
	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/modules"
)

// Bridge Phase 4c: an Audible account's places both ways (POST
// /bridge/audible/accounts/{account}/listening, GET /bridge/progress), the
// console's sync toggle and "Don't sync this book", and the audible.sync
// scheduler.

const (
	audibleListeningURL = "/api/v1/bridge/audible/accounts/" + testAccount + "/listening"
	mistbornASIN        = "B000000001"
	cradleASIN          = "B000000002"
)

// audibleSyncEnv is a Bridge env with testAccount mapped to pat (a listener
// with the whole library), its titles matched to the fixture's two books (and
// one not on this server), the audible module on and the account's sync on.
type audibleSyncEnv struct {
	*bridgeEnv
	patID  int64
	patTok string
}

func newAudibleSyncEnv(t *testing.T) *audibleSyncEnv {
	t.Helper()
	b := newBridgeEnv(t)
	s := &audibleSyncEnv{bridgeEnv: b}
	s.patID, s.patTok = newUserWithSession(t, b.testEnv, "pat", false)
	if err := b.cat.GrantWholeLibrary(context.Background(), s.patID, b.lib.ID); err != nil {
		t.Fatal(err)
	}
	if code, out := b.putAccount(t, testAccount, s.patID, b.lib.ID); code != http.StatusOK {
		t.Fatalf("put account: %d %s", code, out)
	}
	if code, out := b.putTitles(t, testAccount, "p1", true,
		title(mistbornASIN, "The Final Empire", "available", b.lib.ID, mistbornBook),
		title(cradleASIN, "Unsouled", "available", b.lib.ID, cradleBook),
		title("B000000003", "Elsewhere", "available", 0, "")); code != http.StatusOK {
		t.Fatalf("put titles: %d %s", code, out)
	}
	setAudible(t, b.testEnv, b.adminTok, true)
	s.setSync(t, true)
	return s
}

// setSync turns the account's sync on or off in the console.
func (s *audibleSyncEnv) setSync(t *testing.T, on bool) {
	t.Helper()
	body := fmt.Sprintf(`{"sync":%t}`, on)
	if resp, out := s.do(t, "PATCH", "/api/v1/admin/audible/accounts/"+testAccount, s.adminTok, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("sync %t: %d %s", on, resp.StatusCode, out)
	}
}

// place is Audible's place as the Bridge posts it: a span of no length at pos
// on path, at Audible's last_updated (ago before now).
func place(id string, lib int64, path string, pos float64, ago time.Duration) string {
	at := time.Now().Add(-ago).UTC().Format(time.RFC3339)
	return fmt.Sprintf(`{"id":%q,"kind":"listen","library_id":%d,"path":%q,"from_pos":%v,"to_pos":%v,`+
		`"started_at":%q,"ended_at":%q,"start":"jump"}`, id, lib, path, pos, pos, at, at)
}

// post sends a batch of the account's places as the Bridge, sent_at to the
// nanosecond (a whole second would shift its events by up to a second: now -
// sent_at, reordering them against a span posted within that second).
func (s *audibleSyncEnv) post(t *testing.T, events ...string) (int, string) {
	t.Helper()
	body := fmt.Sprintf(`{"sent_at":%q,"events":[%s]}`, time.Now().UTC().Format(time.RFC3339Nano), strings.Join(events, ","))
	resp, out := s.do(t, "POST", audibleListeningURL, s.bridgeTok, body)
	return resp.StatusCode, out
}

// postResults is post's results, which must be a 200.
func (s *audibleSyncEnv) postResults(t *testing.T, events ...string) []catalog.SyncResult {
	t.Helper()
	code, out := s.post(t, events...)
	if code != http.StatusOK {
		t.Fatalf("listening: %d %s", code, out)
	}
	var a catalog.SyncAnswer
	decodeInto(t, out, &a)
	return a.Results
}

// patListens posts a span as pat's phone (Sync v2).
func (s *audibleSyncEnv) patListens(t *testing.T, id, path string, from, to float64, base string) catalog.SyncResult {
	t.Helper()
	body := fmt.Sprintf(`{"device_id":"pat-phone","sent_at":%q,"events":[%s]}`, time.Now().UTC().Format(time.RFC3339Nano),
		listenJSON(id, s.lib.ID, path, from, to, base))
	resp, out := s.do(t, "POST", syncURL, s.patTok, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pat's sync: %d %s", resp.StatusCode, out)
	}
	var a catalog.SyncAnswer
	decodeInto(t, out, &a)
	return a.Results[0]
}

func (s *audibleSyncEnv) patProgress(t *testing.T, path string) *catalog.Progress {
	t.Helper()
	p, err := s.cat.GetProgress(context.Background(), s.patID, catalog.Ref{LibraryID: s.lib.ID, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type progressFeed struct {
	Rows []catalog.AudibleProgressRow `json:"rows"`
	Next string                       `json:"next"`
	More bool                         `json:"more"`
}

func (s *audibleSyncEnv) feed(t *testing.T, query string) progressFeed {
	t.Helper()
	resp, out := s.do(t, "GET", "/api/v1/bridge/progress?account="+testAccount+query, s.bridgeTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("progress%s: %d %s", query, resp.StatusCode, out)
	}
	var f progressFeed
	decodeInto(t, out, &f)
	return f
}

func wantSync(t *testing.T, got catalog.SyncResult, status, reason string) {
	t.Helper()
	if got.Status != status || got.Reason != reason {
		t.Fatalf("result %+v, want %s %s", got, status, reason)
	}
}

// Security-critical: both sync routes take the bridge token only (a session,
// an API key, a cast or calendar feed token, a listener's: 401; the bridge
// token as ?token= too), and the bridge token gets through.
func TestAudibleSyncRoutesRefuseOtherCredentials(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	ctx := context.Background()
	cast, _ := s.auth.IssueToken(ctx, s.adminID, auth.KindCast, "cast", 0)
	feed, _ := s.auth.IssueToken(ctx, s.adminID, auth.KindFeed, "feed", 0)
	body := fmt.Sprintf(`{"sent_at":%q,"events":[]}`, time.Now().UTC().Format(time.RFC3339))
	routes := []struct{ method, path, body string }{
		{"POST", audibleListeningURL, body},
		{"GET", "/api/v1/bridge/progress?account=" + testAccount, ""},
	}
	creds := map[string]string{"none": "", "admin session": s.adminTok, "admin api key": s.adminKey,
		"listener": s.userTok, "the account's person": s.patTok, "cast": cast, "feed": feed, "garbage": "nope"}
	for _, r := range routes {
		for name, tok := range creds {
			if resp, out := s.do(t, r.method, r.path, tok, r.body); resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with %s: %d %s, want 401", r.method, r.path, name, resp.StatusCode, out)
			}
		}
		if resp, out := s.do(t, r.method, r.path, s.bridgeTok, r.body); resp.StatusCode != http.StatusOK {
			t.Errorf("%s %s with the bridge token: %d %s", r.method, r.path, resp.StatusCode, out)
		}
		q := "?token="
		if strings.Contains(r.path, "?") {
			q = "&token="
		}
		if resp, _ := s.do(t, r.method, r.path+q+url.QueryEscape(s.bridgeTok), "", r.body); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s with the bridge token as ?token=: %d, want 401", r.method, r.path, resp.StatusCode)
		}
	}
}

// Furthest only, for the account's person: a place behind pat's is
// not_applied "behind" and leaves the row as it was; ahead it applies over
// pat's phone's row as device audible:<account> named after the account, and
// the phone carrying on from what it saw keeps Audible's further place and is
// offered back; Audible's place shows in the phone's offer by name when the
// phone's place wins. A replay is a duplicate. Nothing is recorded as
// listening, nor against the bridge token or the admin who paired it.
func TestAudibleListeningFurthestOnly(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	ctx := context.Background()
	wantSync(t, s.patListens(t, "pat-1", mistbornBook, 0, 100, "0"), "applied", "")
	before := s.patProgress(t, mistbornBook)

	wantSync(t, s.postResults(t, place("aud-behind", s.lib.ID, mistbornBook, 50, 2*time.Minute))[0], "not_applied", "behind")
	if p := s.patProgress(t, mistbornBook); p.Version != before.Version || p.Position != 100 || p.DeviceID != "pat-phone" {
		t.Fatalf("row after a place behind: %+v", p)
	}

	ahead := place("aud-ahead", s.lib.ID, mistbornBook, 500, 5*time.Minute)
	wantSync(t, s.postResults(t, ahead)[0], "applied", "")
	p := s.patProgress(t, mistbornBook)
	if p.Position != 500 || p.DeviceID != "audible:"+testAccount || p.DeviceName != "Audible (Chris (US))" {
		t.Fatalf("row after a place ahead: %+v", p)
	}
	wantSync(t, s.postResults(t, ahead)[0], "duplicate", "")

	// The phone, at the version it saw, carries on past Audible's place: its
	// place is kept and Audible's offered, by name.
	res := s.patListens(t, "pat-2", mistbornBook, 100, 700, fmt.Sprint(before.Version))
	if res.Status != "diverged" || res.Kept != "event" || res.OtherPlace == nil ||
		res.OtherPlace.DeviceID != "audible:"+testAccount || res.OtherPlace.DeviceName != "Audible (Chris (US))" {
		t.Fatalf("phone past Audible: %+v", res)
	}
	if p := s.patProgress(t, mistbornBook); p.OtherPlace == nil || p.OtherPlace.DeviceName != "Audible (Chris (US))" {
		t.Fatalf("row's offer: %+v", p)
	}

	for _, q := range []string{
		`SELECT COUNT(*) FROM listening_sessions WHERE device_name LIKE 'Audible%'`,
		`SELECT COUNT(*) FROM listening_daily WHERE user_id <> ?`,
		`SELECT COUNT(*) FROM listening_events WHERE device_id = 'audible:acct0001' AND token_id <> 0`,
		`SELECT COUNT(*) FROM progress WHERE user_id <> ?`,
	} {
		var n int
		args := []any{}
		if strings.Contains(q, "?") {
			args = append(args, s.patID)
		}
		if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s = %d, %v; want 0", q, n, err)
		}
	}
	var others int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM listening_sessions
		WHERE token_id = 0 OR token_id IN (SELECT id FROM tokens WHERE kind = 'bridge' OR user_id <> ?)`, s.patID).
		Scan(&others); err != nil || others != 0 {
		t.Fatalf("sessions not on pat's own tokens = %d, %v; want 0", others, err)
	}
}

// A finish from Audible applies; a finished row takes no place and no
// unfinish from it.
func TestAudibleListeningFinish(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	at := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	finish := fmt.Sprintf(`{"id":"aud-fin","kind":"finish","library_id":%d,"path":%q,"at":%q}`, s.lib.ID, mistbornBook, at)
	wantSync(t, s.postResults(t, finish)[0], "applied", "")
	if p := s.patProgress(t, mistbornBook); p == nil || !p.Finished {
		t.Fatalf("row after a finish: %+v", p)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	unfinish := fmt.Sprintf(`{"id":"aud-unfin","kind":"unfinish","library_id":%d,"path":%q,"at":%q}`, s.lib.ID, mistbornBook, now)
	res := s.postResults(t, place("aud-after", s.lib.ID, mistbornBook, 3, 0), unfinish)
	wantSync(t, res[0], "not_applied", "behind")
	wantSync(t, res[1], "not_applied", "behind")
	if p := s.patProgress(t, mistbornBook); !p.Finished {
		t.Fatalf("a finished row moved: %+v", p)
	}
}

// Only the account's synced titles' books, inside the person's access: a book
// marked "Don't sync this book" or "Not this book", or matched to no title, is
// not_synced (both ways: the feed leaves it out); a library the person can't
// reach is no_access; another person's row is never read or written.
func TestAudibleListeningOnlySyncedBooks(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	ctx := context.Background()
	// sam (the env's listener) listens to Mistborn: never in the account's feed.
	samID := s.userIDOf(t, "sam")
	if err := s.cat.GrantWholeLibrary(ctx, samID, s.lib.ID); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"device_id":"sam-phone","sent_at":%q,"events":[%s]}`, time.Now().UTC().Format(time.RFC3339),
		listenJSON("sam-1", s.lib.ID, mistbornBook, 0, 30, "0"))
	if resp, out := s.do(t, "POST", syncURL, s.userTok, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("sam's sync: %d %s", resp.StatusCode, out)
	}
	if f := s.feed(t, ""); len(f.Rows) != 0 {
		t.Fatalf("feed with only sam's row = %+v", f.Rows)
	}
	wantSync(t, s.patListens(t, "pat-c", cradleBook, 0, 40, "0"), "applied", "")
	if f := s.feed(t, ""); len(f.Rows) != 1 || f.Rows[0].ASIN != cradleASIN || f.Rows[0].Path != cradleBook {
		t.Fatalf("feed = %+v", f.Rows)
	}

	// Don't sync this book: neither direction.
	if resp, out := s.do(t, "PATCH", "/api/v1/admin/audible/titles/"+testAccount+"/"+cradleASIN, s.adminTok, `{"no_sync":true}`); resp.StatusCode != http.StatusOK || !strings.Contains(out, `"no_sync":true`) {
		t.Fatalf("don't sync: %d %s", resp.StatusCode, out)
	}
	wantSync(t, s.postResults(t, place("aud-c", s.lib.ID, cradleBook, 900, time.Minute))[0], "rejected", "not_synced")
	if f := s.feed(t, ""); len(f.Rows) != 0 {
		t.Fatalf("feed after Don't sync = %+v", f.Rows)
	}
	if p := s.patProgress(t, cradleBook); p.Position != 40 {
		t.Fatalf("a Don't sync book moved: %+v", p)
	}
	// Undone, it syncs again.
	if resp, out := s.do(t, "PATCH", "/api/v1/admin/audible/titles/"+testAccount+"/"+cradleASIN, s.adminTok, `{"no_sync":false}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("sync again: %d %s", resp.StatusCode, out)
	}
	wantSync(t, s.postResults(t, place("aud-c2", s.lib.ID, cradleBook, 900, time.Minute))[0], "applied", "")

	// Ignored (the manager registry's): out of the sync too, both ways.
	if _, err := s.db.ExecContext(ctx, `UPDATE audible_titles SET ignored = 1 WHERE asin = ?`, cradleASIN); err != nil {
		t.Fatal(err)
	}
	wantSync(t, s.postResults(t, place("aud-c3", s.lib.ID, cradleBook, 1900, time.Minute))[0], "rejected", "not_synced")
	p := s.patProgress(t, cradleBook)
	wantSync(t, s.patListens(t, "pat-c3", cradleBook, p.Position, p.Position+60, strconv.FormatInt(p.Version, 10)), "applied", "")
	if f := s.feed(t, ""); len(f.Rows) != 0 {
		t.Fatalf("feed with the title ignored = %+v", f.Rows)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE audible_titles SET ignored = 0 WHERE asin = ?`, cradleASIN); err != nil {
		t.Fatal(err)
	}

	// Not this book.
	if resp, out := s.do(t, "PATCH", "/api/v1/admin/audible/titles/"+testAccount+"/"+mistbornASIN, s.adminTok, `{"rejected":true}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("not this book: %d %s", resp.StatusCode, out)
	}
	wantSync(t, s.postResults(t, place("aud-m", s.lib.ID, mistbornBook, 900, time.Minute))[0], "rejected", "not_synced")
	if p, err := s.cat.GetProgress(ctx, samID, catalog.Ref{LibraryID: s.lib.ID, Path: mistbornBook}); err != nil || p.Position != 30 {
		t.Fatalf("sam's row = %+v, %v", p, err)
	}

	// A path no title is matched to, and a library pat can't reach.
	other, err := s.cat.CreateLibrary(ctx, catalog.Library{Name: "Other", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	res := s.postResults(t,
		place("aud-x", s.lib.ID, "Brandon Sanderson/Elantris", 10, time.Minute),
		place("aud-y", other.ID, cradleBook, 10, time.Minute))
	wantSync(t, res[0], "rejected", "not_synced")
	wantSync(t, res[1], "rejected", "no_access")
}

// userIDOf is the id of the user named name.
func (s *audibleSyncEnv) userIDOf(t *testing.T, name string) int64 {
	t.Helper()
	var id int64
	if err := s.db.QueryRowContext(context.Background(), `SELECT id FROM users WHERE username = ?`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A sync is refused while it can't act for the account: the module or the
// account's sync off (403 sync_off), its person disabled, made a kid or a demo
// since (403 user_unavailable, no row written), the person deleted (the
// account with them) or an unknown account (404 unknown_account); a bad
// envelope is 400 and a GET without an account 400.
func TestAudibleSyncRefusals(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	ctx := context.Background()
	ev := place("aud-r", s.lib.ID, mistbornBook, 900, time.Minute)
	want := func(name string, status int, code string) {
		t.Helper()
		if got, out := s.post(t, ev); got != status || codeOf(out) != code {
			t.Errorf("%s: listening %d %s, want %d %s", name, got, out, status, code)
		}
		resp, out := s.do(t, "GET", "/api/v1/bridge/progress?account="+testAccount, s.bridgeTok, "")
		if resp.StatusCode != status || codeOf(out) != code {
			t.Errorf("%s: progress %d %s, want %d %s", name, resp.StatusCode, out, status, code)
		}
	}
	s.setSync(t, false)
	want("sync off", http.StatusForbidden, "sync_off")
	s.setSync(t, true)
	setAudible(t, s.testEnv, s.adminTok, false)
	want("module off", http.StatusForbidden, "sync_off")
	setAudible(t, s.testEnv, s.adminTok, true)

	for _, c := range []struct{ name, set, undo string }{
		{"disabled", `UPDATE users SET disabled = 1 WHERE id = ?`, `UPDATE users SET disabled = 0 WHERE id = ?`},
		{"a kid", fmt.Sprintf(`UPDATE users SET managed_by = %d, member_kind = 'kid', member_access = 'all' WHERE id = ?`, s.adminID),
			`UPDATE users SET managed_by = NULL, member_kind = NULL, member_access = NULL WHERE id = ?`},
		{"a demo", `UPDATE users SET is_demo = 1 WHERE id = ?`, `UPDATE users SET is_demo = 0 WHERE id = ?`},
	} {
		if _, err := s.db.ExecContext(ctx, c.set, s.patID); err != nil {
			t.Fatal(err)
		}
		want(c.name, http.StatusForbidden, "user_unavailable")
		if _, err := s.db.ExecContext(ctx, c.undo, s.patID); err != nil {
			t.Fatal(err)
		}
	}
	if p := s.patProgress(t, mistbornBook); p != nil {
		t.Fatalf("a refused sync wrote %+v", p)
	}

	for _, c := range []struct{ name, body, code string }{
		{"no sent_at", `{"events":[]}`, ""},
		{"bad sent_at", `{"sent_at":"yesterday","events":[]}`, ""},
		{"a device_id", `{"device_id":"x","sent_at":"2026-11-03T10:00:00Z","events":[]}`, ""},
		{"furthest_only from a body", `{"sent_at":"2026-11-03T10:00:00Z","furthest_only":false,"events":[]}`, ""},
	} {
		if resp, out := s.do(t, "POST", audibleListeningURL, s.bridgeTok, c.body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", c.name, resp.StatusCode, out)
		}
	}
	if resp, out := s.do(t, "GET", "/api/v1/bridge/progress", s.bridgeTok, ""); resp.StatusCode != http.StatusBadRequest || codeOf(out) != "invalid_request" {
		t.Errorf("progress without an account: %d %s", resp.StatusCode, out)
	}
	for _, q := range []string{"&since=not-a-cursor", "&limit=0", "&limit=x"} {
		if resp, out := s.do(t, "GET", "/api/v1/bridge/progress?account="+testAccount+q, s.bridgeTok, ""); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("progress%s: %d %s, want 400", q, resp.StatusCode, out)
		}
	}
	for _, id := range []string{"acct0009", "Not-An-Id"} {
		if resp, out := s.do(t, "POST", "/api/v1/bridge/audible/accounts/"+id+"/listening", s.bridgeTok, `{}`); resp.StatusCode != http.StatusNotFound || codeOf(out) != "unknown_account" {
			t.Errorf("listening for %s: %d %s", id, resp.StatusCode, out)
		}
		if resp, out := s.do(t, "GET", "/api/v1/bridge/progress?account="+id, s.bridgeTok, ""); resp.StatusCode != http.StatusNotFound || codeOf(out) != "unknown_account" {
			t.Errorf("progress for %s: %d %s", id, resp.StatusCode, out)
		}
	}
	if _, err := s.auth.DeleteUser(ctx, s.patID, nil); err != nil {
		t.Fatal(err)
	}
	want("person deleted", http.StatusNotFound, "unknown_account")
}

// FurthestOnly is never a client's: POST /me/listening/sync refuses it as an
// unknown field, on the envelope (400) and on an event (rejected invalid), and
// a listener's place behind the row still applies by its own rules.
func TestListeningSyncHasNoFurthestOnly(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	now := time.Now().UTC().Format(time.RFC3339)
	body := fmt.Sprintf(`{"device_id":"pat-phone","sent_at":%q,"furthest_only":true,"events":[]}`, now)
	if resp, out := s.do(t, "POST", syncURL, s.patTok, body); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("furthest_only on the envelope: %d %s", resp.StatusCode, out)
	}
	ev := strings.Replace(listenJSON("pat-f", s.lib.ID, mistbornBook, 0, 10, "0"), `"start"`, `"furthest_only":true,"start"`, 1)
	body = fmt.Sprintf(`{"device_id":"pat-phone","sent_at":%q,"events":[%s]}`, now, ev)
	resp, out := s.do(t, "POST", syncURL, s.patTok, body)
	var a catalog.SyncAnswer
	decodeInto(t, out, &a)
	if resp.StatusCode != http.StatusOK || a.Results[0].Status != "rejected" || a.Results[0].Reason != "invalid" {
		t.Fatalf("furthest_only on an event: %d %s", resp.StatusCode, out)
	}
	// Moving back on purpose from the version seen still applies for a player.
	wantSync(t, s.patListens(t, "pat-a", mistbornBook, 0, 600, "0"), "applied", "")
	back := strings.Replace(listenJSON("pat-b", s.lib.ID, mistbornBook, 10, 20, "1"), `"resume"`, `"moved_back"`, 1)
	body = fmt.Sprintf(`{"device_id":"pat-phone","sent_at":%q,"events":[%s]}`, now, back)
	if resp, out := s.do(t, "POST", syncURL, s.patTok, body); resp.StatusCode != http.StatusOK || !strings.Contains(out, `"status":"applied"`) {
		t.Fatalf("moved back: %d %s", resp.StatusCode, out)
	}
}

// The feed: the person's rows on the synced books, oldest change first, rows
// the account wrote left out (never echoed back), paged by an opaque cursor
// kept between polls (next is since again when nothing changed), the title's
// ASIN and the book's length on each.
func TestBridgeProgressFeed(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	wantSync(t, s.patListens(t, "pat-m", mistbornBook, 0, 20, "0"), "applied", "")
	wantSync(t, s.patListens(t, "pat-c", cradleBook, 0, 30, "0"), "applied", "")
	page := s.feed(t, "&limit=1")
	if len(page.Rows) != 1 || !page.More || page.Next == "" || page.Rows[0].ASIN != mistbornASIN ||
		page.Rows[0].Position != 20 || page.Rows[0].DeviceID != "pat-phone" || page.Rows[0].Version != 1 ||
		page.Rows[0].Finished {
		t.Fatalf("first page = %+v", page)
	}
	second := s.feed(t, "&limit=1&since="+url.QueryEscape(page.Next))
	if len(second.Rows) != 1 || second.Rows[0].ASIN != cradleASIN || !second.More {
		t.Fatalf("second page = %+v", second)
	}
	end := s.feed(t, "&limit=1&since="+url.QueryEscape(second.Next))
	if len(end.Rows) != 0 || end.More || end.Next != second.Next {
		t.Fatalf("end = %+v (next %q)", end, second.Next)
	}
	// Audible moves Mistborn ahead: its own row is not fed back; the phone's
	// next save is.
	wantSync(t, s.postResults(t, place("aud-m", s.lib.ID, mistbornBook, 400, 5*time.Minute))[0], "applied", "")
	if f := s.feed(t, "&since="+url.QueryEscape(end.Next)); len(f.Rows) != 0 {
		t.Fatalf("Audible's own row fed back: %+v", f.Rows)
	}
	if f := s.feed(t, ""); len(f.Rows) != 1 || f.Rows[0].ASIN != cradleASIN {
		t.Fatalf("whole feed = %+v", f.Rows)
	}
	wantSync(t, s.patListens(t, "pat-m2", mistbornBook, 400, 460, "2"), "applied", "")
	if f := s.feed(t, "&since="+url.QueryEscape(end.Next)); len(f.Rows) != 1 || f.Rows[0].Position != 460 {
		t.Fatalf("feed after the phone's save = %+v", f.Rows)
	}
}

// The console's toggle: a new account syncs off whatever the Bridge sends, a
// mapping again keeps the console's choice, and turning it off cancels the
// account's open sync.
func TestAudibleSyncToggle(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	ctx := context.Background()
	acct, err := s.cat.AudibleAccount(ctx, testAccount)
	if err != nil || !acct.Sync {
		t.Fatalf("after the console turned it on: %+v, %v", acct, err)
	}
	if code, out := s.putAccount(t, testAccount, s.patID, s.lib.ID); code != http.StatusOK || !strings.Contains(out, `"sync":true`) {
		t.Fatalf("mapped again: %d %s", code, out)
	}
	if code, out := s.putAccount(t, "acct0002", s.patID, s.lib.ID); code != http.StatusOK || !strings.Contains(out, `"sync":false`) {
		t.Fatalf("a new account: %d %s", code, out)
	}
	s.beat(t, bridge.KindVerify, bridge.KindAudibleSync)
	d := modules.Deps{Log: slog.New(slog.DiscardHandler), Catalog: s.cat, Bridge: s.api.bridge}
	if err := audible.SchedulerTick(ctx, d, time.Now()); err != nil {
		t.Fatal(err)
	}
	s.setSync(t, false)
	var state, reason string
	if err := s.db.QueryRowContext(ctx, `SELECT state, error FROM bridge_jobs WHERE kind = ?`, bridge.KindAudibleSync).
		Scan(&state, &reason); err != nil || state != bridge.StateCancelled || reason != bridge.CancelAudibleSyncOff ||
		!bridge.IsCancelReason(reason) {
		t.Fatalf("open sync after sync off: %s %q, %v", state, reason, err)
	}
}

// The scheduler queues one audible.sync per account with sync on every 15
// minutes, with the account's synced titles as its params (none for an
// account with sync off, or once Audible refused it, until the Bridge maps it
// again); a done sync is the account's last sync.
func TestAudibleSyncScheduler(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	ctx := context.Background()
	if code, out := s.putAccount(t, "acct0002", s.patID, s.lib.ID); code != http.StatusOK {
		t.Fatalf("second account: %d %s", code, out)
	}
	if code, out := s.putTitles(t, "acct0002", "p2", true, title(mistbornASIN, "The Final Empire", "available", s.lib.ID, mistbornBook)); code != http.StatusOK {
		t.Fatalf("second account's titles: %d %s", code, out)
	}
	d := modules.Deps{Log: slog.New(slog.DiscardHandler), Catalog: s.cat, Bridge: s.api.bridge}
	syncs := func() []bridge.Job {
		t.Helper()
		jobs, _, err := s.api.bridge.List(ctx, bridge.Filter{Kind: bridge.KindAudibleSync})
		if err != nil {
			t.Fatal(err)
		}
		return jobs
	}
	now := time.Now()
	// A Bridge without the kind: nothing.
	if err := audible.SchedulerTick(ctx, d, now); err != nil {
		t.Fatal(err)
	}
	if j := syncs(); len(j) != 0 {
		t.Fatalf("syncs without the kind = %+v", j)
	}
	s.beat(t, bridge.KindVerify, bridge.KindAudibleSync)
	if err := audible.SchedulerTick(ctx, d, now); err != nil {
		t.Fatal(err)
	}
	j := syncs()
	if len(j) != 1 || j[0].State != bridge.StateQueued || j[0].Priority != -10 {
		t.Fatalf("first tick = %+v, want one queued sync (acct0002's sync is off)", j)
	}
	var p bridge.AudibleSyncParams
	if err := json.Unmarshal(j[0].Params, &p); err != nil || p.Account != testAccount || len(p.Titles) != 2 ||
		p.Titles[0] != (bridge.AudibleSyncTitle{ASIN: mistbornASIN, LibraryID: s.lib.ID, Path: mistbornBook, RuntimeS: 36000}) ||
		p.Titles[1].ASIN != cradleASIN {
		t.Fatalf("params = %s, %v", j[0].Params, err)
	}
	if err := audible.SchedulerTick(ctx, d, now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if j := syncs(); len(j) != 1 {
		t.Fatalf("within 15 minutes = %d syncs", len(j))
	}

	// Claimed and done: the account's last sync. The next is due 15 minutes
	// after the last was queued.
	job := s.claim(t, bridge.KindAudibleSync)
	s.finish(t, job, bridge.StateDone, `{"sent":1}`, "")
	if a, _ := s.cat.AudibleAccount(ctx, testAccount); a.LastSyncAt == nil || a.RefusedAt != nil {
		t.Fatalf("after a done sync: %+v", a)
	}
	if err := audible.SchedulerTick(ctx, d, now.Add(16*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if j := syncs(); len(j) != 2 || j[0].State != bridge.StateQueued {
		t.Fatalf("after 16 minutes = %+v", j)
	}

	// Audible refused the account: no sync until it is mapped again.
	job = s.claim(t, bridge.KindAudibleSync)
	s.finish(t, job, bridge.StateFailed, `{"code":"audible_refused","message":"sign in again"}`, "audible_refused: sign in again")
	if a, _ := s.cat.AudibleAccount(ctx, testAccount); a.RefusedAt == nil {
		t.Fatalf("after a refused sync: %+v", a)
	}
	if err := audible.SchedulerTick(ctx, d, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if j := syncs(); len(j) != 2 || j[0].State != bridge.StateFailed {
		t.Fatalf("a refused account synced: %+v", j)
	}
	if code, out := s.putAccount(t, testAccount, s.patID, s.lib.ID); code != http.StatusOK || !strings.Contains(out, `"refused_at":null`) {
		t.Fatalf("mapped again: %d %s", code, out)
	}
	if err := audible.SchedulerTick(ctx, d, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Only the newest ended sync is kept beside the open one.
	if j := syncs(); len(j) != 2 || j[0].State != bridge.StateQueued || j[1].State != bridge.StateFailed {
		t.Fatalf("after mapping again = %+v", j)
	}
	// An account whose titles are all Don't sync queues nothing.
	if _, err := s.db.ExecContext(ctx, `UPDATE audible_titles SET no_sync = 1 WHERE account = 'acct0002'`); err != nil {
		t.Fatal(err)
	}
	if resp, out := s.do(t, "PATCH", "/api/v1/admin/audible/accounts/acct0002", s.adminTok, `{"sync":true}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("acct0002 sync on: %d %s", resp.StatusCode, out)
	}
	if err := audible.SchedulerTick(ctx, d, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, job := range syncs() {
		if acc, _ := bridge.AudibleSyncOf(job); acc == "acct0002" {
			t.Fatalf("a sync with nothing to sync: %+v", job)
		}
	}
}

// syncJobs are the audible.sync jobs, newest first.
func (s *audibleSyncEnv) syncJobs(t *testing.T) []bridge.Job {
	t.Helper()
	jobs, _, err := s.api.bridge.List(context.Background(), bridge.Filter{Kind: bridge.KindAudibleSync})
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

// queueSync has the scheduler queue the account's sync at now.
func (s *audibleSyncEnv) queueSync(t *testing.T, now time.Time) {
	t.Helper()
	s.beat(t, bridge.KindVerify, bridge.KindAudibleSync)
	d := modules.Deps{Log: slog.New(slog.DiscardHandler), Catalog: s.cat, Bridge: s.api.bridge}
	if err := audible.SchedulerTick(context.Background(), d, now); err != nil {
		t.Fatal(err)
	}
}

// Mapped to another person, an account's sync is off (the console turned it
// on for the person it showed then): its last sync forgotten, its open sync
// cancelled, both routes sync_off until the console turns it on again.
func TestAudibleSyncRemapTurnsSyncOff(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	ctx := context.Background()
	s.queueSync(t, time.Now())
	s.finish(t, s.claim(t, bridge.KindAudibleSync), bridge.StateDone, `{"sent":1}`, "")
	s.queueSync(t, time.Now().Add(16*time.Minute))
	quinnID, _ := newUserWithSession(t, s.testEnv, "quinn", false)
	if code, out := s.putAccount(t, testAccount, quinnID, s.lib.ID); code != http.StatusOK || !strings.Contains(out, `"sync":false`) {
		t.Fatalf("mapped to quinn: %d %s", code, out)
	}
	if a, err := s.cat.AudibleAccount(ctx, testAccount); err != nil || a.Sync || a.LastSyncAt != nil {
		t.Fatalf("after the re-map: %+v, %v", a, err)
	}
	if j := s.syncJobs(t); len(j) != 2 || j[0].State != bridge.StateCancelled || j[0].Error != bridge.CancelAudibleSyncOff {
		t.Fatalf("syncs after the re-map = %+v", j)
	}
	if resp, out := s.do(t, "GET", "/api/v1/bridge/progress?account="+testAccount, s.bridgeTok, ""); resp.StatusCode != http.StatusForbidden ||
		!strings.Contains(out, "sync_off") {
		t.Fatalf("feed after the re-map: %d %s", resp.StatusCode, out)
	}
	if code, out := s.post(t, place("aud-q", s.lib.ID, mistbornBook, 400, time.Minute)); code != http.StatusForbidden {
		t.Fatalf("listening after the re-map: %d %s", code, out)
	}
}

// A paused account (its library gone) syncs neither way: both routes answer
// sync_off, as the scheduler queues none for it.
func TestAudibleSyncPaused(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	if _, err := s.db.ExecContext(context.Background(), `UPDATE audible_accounts SET library_id = NULL WHERE account = ?`, testAccount); err != nil {
		t.Fatal(err)
	}
	if resp, out := s.do(t, "GET", "/api/v1/bridge/progress?account="+testAccount, s.bridgeTok, ""); resp.StatusCode != http.StatusForbidden ||
		!strings.Contains(out, "sync_off") {
		t.Fatalf("paused feed: %d %s", resp.StatusCode, out)
	}
	if code, out := s.post(t, place("aud-p", s.lib.ID, mistbornBook, 400, time.Minute)); code != http.StatusForbidden || !strings.Contains(out, "sync_off") {
		t.Fatalf("paused listening: %d %s", code, out)
	}
}

// Only a sync that read Audible's places is the account's last sync: one that
// waited on Audible, or found the sync off, is not. A complete poll ends a
// refusal (Audible answered the account), so syncs are due again.
func TestAudibleSyncLastSyncAndRefusal(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	ctx := context.Background()
	now := time.Now()
	for i, result := range []string{`{"waiting":true,"wait_until":"2030-01-01T00:00:00Z"}`, `{"sync_off":true}`} {
		s.queueSync(t, now.Add(time.Duration(i)*16*time.Minute))
		s.finish(t, s.claim(t, bridge.KindAudibleSync), bridge.StateDone, result, "")
		if a, _ := s.cat.AudibleAccount(ctx, testAccount); a.LastSyncAt != nil {
			t.Fatalf("after a done sync %s: last sync %v", result, *a.LastSyncAt)
		}
	}
	s.queueSync(t, now.Add(32*time.Minute))
	s.finish(t, s.claim(t, bridge.KindAudibleSync), bridge.StateFailed, `{"code":"audible_refused"}`, "audible_refused")
	if a, _ := s.cat.AudibleAccount(ctx, testAccount); a.RefusedAt == nil {
		t.Fatalf("after a refused sync: %+v", a)
	}
	if code, out := s.putTitles(t, testAccount, "p2", false, title(mistbornASIN, "The Final Empire", "available", s.lib.ID, mistbornBook)); code != http.StatusOK {
		t.Fatalf("partial poll: %d %s", code, out)
	}
	if a, _ := s.cat.AudibleAccount(ctx, testAccount); a.RefusedAt == nil {
		t.Fatalf("a partial poll ended the refusal: %+v", a)
	}
	if code, out := s.putTitles(t, testAccount, "p2", true,
		title(mistbornASIN, "The Final Empire", "available", s.lib.ID, mistbornBook),
		title(cradleASIN, "Unsouled", "available", s.lib.ID, cradleBook)); code != http.StatusOK {
		t.Fatalf("complete poll: %d %s", code, out)
	}
	if a, _ := s.cat.AudibleAccount(ctx, testAccount); a.RefusedAt != nil {
		t.Fatalf("a complete poll kept the refusal: %+v", a)
	}
	s.queueSync(t, now.Add(48*time.Minute))
	if j := s.syncJobs(t); len(j) == 0 || j[0].State != bridge.StateQueued {
		t.Fatalf("no sync after the refusal ended: %+v", j)
	}
}

// The person's access is read at every call: once their share is revoked the
// book's place leaves the feed and Audible's place on it is no_access.
func TestAudibleSyncRevokedShare(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	ctx := context.Background()
	wantSync(t, s.patListens(t, "pat-m", mistbornBook, 0, 20, "0"), "applied", "")
	if f := s.feed(t, ""); len(f.Rows) != 1 {
		t.Fatalf("feed before the revoke = %+v", f.Rows)
	}
	var share int64
	if err := s.db.QueryRowContext(ctx, `SELECT share_id FROM user_share_access WHERE user_id = ?`, s.patID).Scan(&share); err != nil {
		t.Fatal(err)
	}
	if err := s.cat.RevokeShare(ctx, s.patID, share); err != nil {
		t.Fatal(err)
	}
	if f := s.feed(t, ""); len(f.Rows) != 0 {
		t.Fatalf("feed after the revoke = %+v", f.Rows)
	}
	wantSync(t, s.postResults(t, place("aud-m", s.lib.ID, mistbornBook, 400, time.Minute))[0], "rejected", catalog.SyncNoAccess)
}

// The account's open sync goes with it: removed (CancelAudibleRemoved), or
// paused by its library's deletion (CancelAudiblePaused).
func TestAudibleSyncCancelledWithItsAccount(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, reason string
		end          func(s *audibleSyncEnv) error
	}{
		{"removed", bridge.CancelAudibleRemoved, func(s *audibleSyncEnv) error {
			_, err := s.cat.DeleteAudibleAccount(context.Background(), testAccount)
			return err
		}},
		{"paused", bridge.CancelAudiblePaused, func(s *audibleSyncEnv) error {
			return s.cat.DeleteLibrary(context.Background(), s.lib.ID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newAudibleSyncEnv(t)
			s.queueSync(t, time.Now())
			if err := tc.end(s); err != nil {
				t.Fatal(err)
			}
			if j := s.syncJobs(t); len(j) != 1 || j[0].State != bridge.StateCancelled || j[0].Error != tc.reason {
				t.Fatalf("sync after the account %s = %+v", tc.name, j)
			}
		})
	}
}

// Two titles on one book (an edition owned twice) are two feed rows, one per
// ASIN, paged one by one.
func TestBridgeProgressFeedRowPerTitle(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	const second = "B000000009"
	if code, out := s.putTitles(t, testAccount, "p2", true,
		title(mistbornASIN, "The Final Empire", "available", s.lib.ID, mistbornBook),
		title(second, "The Final Empire (2)", "available", s.lib.ID, mistbornBook),
		title(cradleASIN, "Unsouled", "available", s.lib.ID, cradleBook)); code != http.StatusOK {
		t.Fatalf("titles: %d %s", code, out)
	}
	wantSync(t, s.patListens(t, "pat-m", mistbornBook, 0, 20, "0"), "applied", "")
	first := s.feed(t, "&limit=1")
	next := s.feed(t, "&limit=1&since="+url.QueryEscape(first.Next))
	end := s.feed(t, "&limit=1&since="+url.QueryEscape(next.Next))
	if len(first.Rows) != 1 || first.Rows[0].ASIN != mistbornASIN || len(next.Rows) != 1 || next.Rows[0].ASIN != second ||
		next.Rows[0].Path != mistbornBook || len(end.Rows) != 0 {
		t.Fatalf("pages = %+v, %+v, %+v", first.Rows, next.Rows, end.Rows)
	}
}

// An Audible account's device id is the Bridge's route's alone: Sync v2
// refuses a batch naming one, and a legacy save naming one (a row echoed back)
// is stored under none, so the feed still sends it. An edit of a place Audible
// wrote is no longer Audible's either: it reaches the feed.
func TestAudibleDeviceIDIsTheBridges(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	body := fmt.Sprintf(`{"device_id":"audible:%s","sent_at":%q,"events":[%s]}`, testAccount,
		time.Now().UTC().Format(time.RFC3339Nano), listenJSON("pat-x", s.lib.ID, mistbornBook, 0, 20, "0"))
	if resp, out := s.do(t, "POST", syncURL, s.patTok, body); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Sync v2 as the Audible device: %d %s", resp.StatusCode, out)
	}
	prog := "/api/v1/libraries/" + strconv.FormatInt(s.lib.ID, 10) + "/progress?path="
	put := fmt.Sprintf(`{"position":50,"duration":36000,"device_id":"audible:%s","updated_at":%q}`, testAccount,
		time.Now().UTC().Format(time.RFC3339Nano))
	if resp, out := s.do(t, "PUT", prog+url.QueryEscape(cradleBook), s.patTok, put); resp.StatusCode != http.StatusOK {
		t.Fatalf("legacy save: %d %s", resp.StatusCode, out)
	}
	if p := s.patProgress(t, cradleBook); p == nil || p.DeviceID != "" || p.Position != 50 {
		t.Fatalf("legacy save as the Audible device: %+v", p)
	}
	wantSync(t, s.postResults(t, place("aud-m", s.lib.ID, mistbornBook, 400, time.Minute))[0], "applied", "")
	if f := s.feed(t, ""); len(f.Rows) != 1 || f.Rows[0].ASIN != cradleASIN {
		t.Fatalf("feed before the edit = %+v", f.Rows)
	}
	if resp, out := s.do(t, "PATCH", prog+url.QueryEscape(mistbornBook), s.patTok, `{"position":100}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("edit: %d %s", resp.StatusCode, out)
	}
	if p := s.patProgress(t, mistbornBook); p.DeviceID != "" || p.DeviceName != "" || p.Position != 100 {
		t.Fatalf("an edit of Audible's place: %+v", p)
	}
	f := s.feed(t, "")
	if len(f.Rows) != 2 || f.Rows[1].ASIN != mistbornASIN || f.Rows[1].Position != 100 {
		t.Fatalf("feed after the edit = %+v", f.Rows)
	}
}

// The feed follows the order this server wrote the rows in (change_seq), not
// their updated_at: a row written after the cursor with an earlier time (a
// device's offline backlog, a diverged apply keeping the later time) is still
// sent.
func TestBridgeProgressFeedWriteOrder(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	wantSync(t, s.patListens(t, "pat-m", mistbornBook, 0, 20, "0"), "applied", "")
	wantSync(t, s.patListens(t, "pat-c", cradleBook, 0, 30, "0"), "applied", "")
	end := s.feed(t, "")
	if len(end.Rows) != 2 {
		t.Fatalf("feed = %+v", end.Rows)
	}
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE progress SET position = 90, version = version + 1, updated_at = '2020-01-01T00:00:00.000Z'
		  WHERE user_id = ? AND rel_path = ?`, s.patID, mistbornBook); err != nil {
		t.Fatal(err)
	}
	f := s.feed(t, "&since="+url.QueryEscape(end.Next))
	if len(f.Rows) != 1 || f.Rows[0].ASIN != mistbornASIN || f.Rows[0].Position != 90 ||
		f.Rows[0].UpdatedAt != "2020-01-01T00:00:00.000Z" {
		t.Fatalf("feed after a late write with an early time = %+v", f.Rows)
	}
	if again := s.feed(t, "&since="+url.QueryEscape(f.Next)); len(again.Rows) != 0 {
		t.Fatalf("feed after it = %+v", again.Rows)
	}
}

// A title whose Audible runtime disagrees with the indexed book's duration is
// matched to another recording: neither its places nor its finish apply
// (rejected duration_mismatch). A book whose duration agrees, or is unknown,
// syncs as usual.
func TestAudibleListeningRuntimeMismatch(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	ctx := context.Background()
	for path, d := range map[string]float64{mistbornBook: 20000, cradleBook: 36030} {
		if _, err := s.db.ExecContext(ctx, `UPDATE books SET duration = ? WHERE library_id = ? AND rel_path = ?`,
			d, s.lib.ID, path); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	finish := fmt.Sprintf(`{"id":"aud-fin","kind":"finish","library_id":%d,"path":%q,"at":%q}`, s.lib.ID, mistbornBook, at)
	res := s.postResults(t, place("aud-m", s.lib.ID, mistbornBook, 400, time.Minute), finish,
		place("aud-c", s.lib.ID, cradleBook, 500, time.Minute))
	wantSync(t, res[0], "rejected", "duration_mismatch")
	wantSync(t, res[1], "rejected", "duration_mismatch")
	wantSync(t, res[2], "applied", "")
	if p := s.patProgress(t, mistbornBook); p != nil {
		t.Fatalf("a mismatched title wrote %+v", p)
	}
}

// A console change to which titles take part in an account's sync queues its
// audible.sync at once with the titles now (an open one the Bridge hasn't
// claimed takes them), so the Bridge stops pushing a left-out title without
// waiting for the next turn; with no Bridge running audible.sync, nothing is
// queued.
func TestAudibleSyncQueuedOnTitleChange(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	ctx := context.Background()
	patch := func(asin, body string) {
		t.Helper()
		if resp, out := s.do(t, "PATCH", "/api/v1/admin/audible/titles/"+testAccount+"/"+asin, s.adminTok, body); resp.StatusCode != http.StatusOK {
			t.Fatalf("patch %s: %d %s", asin, resp.StatusCode, out)
		}
	}
	syncs := func() []bridge.AudibleSyncParams {
		t.Helper()
		var out []bridge.AudibleSyncParams
		rows, err := s.db.QueryContext(ctx, `SELECT params FROM bridge_jobs WHERE kind = ? AND state = ?`,
			bridge.KindAudibleSync, bridge.StateQueued)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			var p bridge.AudibleSyncParams
			if err := rows.Scan(&raw); err != nil || json.Unmarshal(raw, &p) != nil {
				t.Fatal(err)
			}
			out = append(out, p)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	patch(mistbornASIN, `{"no_sync":true}`)
	if got := syncs(); len(got) != 0 {
		t.Fatalf("queued with no Bridge running audible.sync: %+v", got)
	}
	s.beat(t, bridge.KindVerify, bridge.KindAudibleSync)
	patch(cradleASIN, `{"rejected":true}`)
	if got := syncs(); len(got) != 1 || len(got[0].Titles) != 0 {
		t.Fatalf("after Not this book = %+v", got)
	}
	patch(mistbornASIN, `{"no_sync":false}`)
	if got := syncs(); len(got) != 1 || len(got[0].Titles) != 1 || got[0].Titles[0].ASIN != mistbornASIN {
		t.Fatalf("after Sync again = %+v", got)
	}
	patch(mistbornASIN, `{"dest_path":"Other/Place"}`) // a destination changes no sync
	if got := syncs(); len(got) != 1 {
		t.Fatalf("after a destination = %+v", got)
	}
}

// A sync failing with audible_sign_in (Audible refused the sign-in for now,
// the Bridge tries again) is an ordinary failure: the account is not marked
// refused and its next sync falls due as usual.
func TestAudibleSyncSignInIsTransient(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	ctx := context.Background()
	s.beat(t, bridge.KindVerify, bridge.KindAudibleSync)
	d := modules.Deps{Log: slog.New(slog.DiscardHandler), Catalog: s.cat, Bridge: s.api.bridge}
	if err := audible.SchedulerTick(ctx, d, time.Now()); err != nil {
		t.Fatal(err)
	}
	job := s.claim(t, bridge.KindAudibleSync)
	s.finish(t, job, bridge.StateFailed, `{"code":"audible_sign_in","message":"try later"}`, "audible_sign_in: try later")
	if a, _ := s.cat.AudibleAccount(ctx, testAccount); a.RefusedAt != nil {
		t.Fatalf("a transient sign-in refusal marked the account refused: %+v", a)
	}
	if due, err := s.cat.DueAudibleSyncs(ctx, time.Now().Add(time.Hour), 10); err != nil || len(due) != 1 {
		t.Fatalf("due after it = %v, %v", due, err)
	}
}

// change_seq is drawn from one counter and never reused: with the row holding
// the highest number deleted (a reset), the next write still comes after a
// cursor that had read it.
func TestBridgeProgressFeedSeqNeverReused(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	wantSync(t, s.patListens(t, "pat-m", mistbornBook, 0, 20, "0"), "applied", "")
	wantSync(t, s.patListens(t, "pat-c", cradleBook, 0, 30, "0"), "applied", "")
	end := s.feed(t, "")
	if len(end.Rows) != 2 || end.Rows[1].ASIN != cradleASIN {
		t.Fatalf("feed = %+v", end.Rows)
	}
	if _, err := s.db.ExecContext(context.Background(), `DELETE FROM progress WHERE user_id = ? AND rel_path = ?`,
		s.patID, cradleBook); err != nil {
		t.Fatal(err)
	}
	p := s.patProgress(t, mistbornBook)
	wantSync(t, s.patListens(t, "pat-m2", mistbornBook, p.Position, p.Position+40, strconv.FormatInt(p.Version, 10)), "applied", "")
	f := s.feed(t, "&since="+url.QueryEscape(end.Next))
	if len(f.Rows) != 1 || f.Rows[0].ASIN != mistbornASIN || f.Rows[0].Position != p.Position+40 {
		t.Fatalf("feed after the highest row was deleted = %+v", f.Rows)
	}
}

// The feed leaves out a row whose title disagrees with the book in length (the
// listening route's duration_mismatch: another edition or volume), so the
// Bridge never pushes it; a second title on the book that agrees gets its own
// row and the one that disagrees still none. The cursor still moves past a
// row left out.
func TestBridgeProgressFeedRuntimeMismatch(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `UPDATE books SET duration = 20000 WHERE library_id = ? AND rel_path = ?`,
		s.lib.ID, mistbornBook); err != nil {
		t.Fatal(err)
	}
	wantSync(t, s.patListens(t, "pat-m", mistbornBook, 0, 20, "0"), "applied", "")
	f := s.feed(t, "&limit=1")
	if len(f.Rows) != 0 || !f.More || f.Next == "" {
		t.Fatalf("a mismatched book's page = %+v", f)
	}
	if rest := s.feed(t, "&since="+url.QueryEscape(f.Next)); len(rest.Rows) != 0 {
		t.Fatalf("after it = %+v", rest.Rows)
	}
	const second = "B000000009"
	if code, out := s.putTitles(t, testAccount, "p2", true,
		title(mistbornASIN, "The Final Empire", "available", s.lib.ID, mistbornBook),
		title(second, "The Final Empire (abridged)", "available", s.lib.ID, mistbornBook),
		title(cradleASIN, "Unsouled", "available", s.lib.ID, cradleBook)); code != http.StatusOK {
		t.Fatalf("titles: %d %s", code, out)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE audible_titles SET runtime_s = 20000 WHERE asin = ?`, second); err != nil {
		t.Fatal(err)
	}
	if all := s.feed(t, ""); len(all.Rows) != 1 || all.Rows[0].ASIN != second || all.Rows[0].Path != mistbornBook {
		t.Fatalf("with a title that agrees = %+v", all.Rows)
	}
}

// With the book's indexed duration unknown (a failed probe) the row's own
// stored duration is the length a title is checked against; both unknown
// agree.
func TestBridgeProgressFeedRuntimeUnknownBook(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `UPDATE books SET duration = 0 WHERE library_id = ?`, s.lib.ID); err != nil {
		t.Fatal(err)
	}
	wantSync(t, s.patListens(t, "pat-m", mistbornBook, 0, 20, "0"), "applied", "")
	wantSync(t, s.patListens(t, "pat-c", cradleBook, 0, 30, "0"), "applied", "")
	for path, d := range map[string]float64{mistbornBook: 20000, cradleBook: 0} {
		if _, err := s.db.ExecContext(ctx, `UPDATE progress SET duration = ? WHERE user_id = ? AND library_id = ? AND rel_path = ?`,
			d, s.patID, s.lib.ID, path); err != nil {
			t.Fatal(err)
		}
	}
	if f := s.feed(t, ""); len(f.Rows) != 1 || f.Rows[0].ASIN != cradleASIN {
		t.Fatalf("feed with the books' durations unknown = %+v", f.Rows)
	}
}

// A title newly matched to a book offers the book's existing place again (its
// row moves past the cursor); a poll that changes no match offers nothing.
func TestBridgeProgressFeedRematch(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	wantSync(t, s.patListens(t, "pat-c", cradleBook, 0, 30, "0"), "applied", "")
	end := s.feed(t, "")
	if len(end.Rows) != 1 {
		t.Fatalf("feed = %+v", end.Rows)
	}
	// Mistborn's title moves to Cradle's book.
	if code, out := s.putTitles(t, testAccount, "p2", true,
		title(mistbornASIN, "The Final Empire", "available", s.lib.ID, cradleBook),
		title(cradleASIN, "Unsouled", "available", s.lib.ID, cradleBook)); code != http.StatusOK {
		t.Fatalf("titles: %d %s", code, out)
	}
	f := s.feed(t, "&since="+url.QueryEscape(end.Next))
	if len(f.Rows) != 2 || f.Rows[0].Path != cradleBook || f.Rows[1].Path != cradleBook {
		t.Fatalf("feed after a re-match = %+v", f.Rows)
	}
	if code, out := s.putTitles(t, testAccount, "p3", true,
		title(mistbornASIN, "The Final Empire", "available", s.lib.ID, cradleBook),
		title(cradleASIN, "Unsouled", "available", s.lib.ID, cradleBook)); code != http.StatusOK {
		t.Fatalf("titles: %d %s", code, out)
	}
	if again := s.feed(t, "&since="+url.QueryEscape(f.Next)); len(again.Rows) != 0 {
		t.Fatalf("feed after a poll with no new match = %+v", again.Rows)
	}
}
