package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/audible"
	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/modules"
)

// Bridge Phase 4a: Audible accounts, titles, the Bridge's user and book lists,
// the ASIN backfill and the console's routes.

const testAccount = "acct0001"

// setAudible turns the audible module on or off through the settings save.
func setAudible(t *testing.T, e *testEnv, adminTok string, on bool) {
	t.Helper()
	body := fmt.Sprintf(`{"modules":{"audible":%t}}`, on)
	if resp, out := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("audible %t: %d %s", on, resp.StatusCode, out)
	}
}

// putAccount maps testAccount (or id) to user and library as the Bridge.
func (b *bridgeEnv) putAccount(t *testing.T, id string, user, lib int64) (int, string) {
	t.Helper()
	body := fmt.Sprintf(`{"label":"Chris (US)","marketplace":"us","user_id":%d,"library_id":%d,"policy":"manual","sync":true}`, user, lib)
	resp, out := b.do(t, "PUT", "/api/v1/bridge/audible/accounts/"+id, b.bridgeTok, body)
	return resp.StatusCode, out
}

// putTitles sends one titles call as the Bridge.
func (b *bridgeEnv) putTitles(t *testing.T, id, poll string, final bool, titles ...string) (int, string) {
	t.Helper()
	body := fmt.Sprintf(`{"poll_id":%q,"final":%t,"titles":[%s]}`, poll, final, strings.Join(titles, ","))
	resp, out := b.do(t, "PUT", "/api/v1/bridge/audible/accounts/"+id+"/titles", b.bridgeTok, body)
	return resp.StatusCode, out
}

// title is one title's JSON, matched to path in lib when path isn't "".
func title(asin, name, state string, lib int64, path string) string {
	match := "null"
	if path != "" {
		match = fmt.Sprintf(`{"library_id":%d,"path":%q,"how":"asin"}`, lib, path)
	}
	return fmt.Sprintf(`{"asin":%q,"title":%q,"authors":["Brandon Sanderson"],"series":[{"name":"Mistborn","position":"1"}],
		"release_date":"2026-11-20","runtime_s":36000,"state":%q,"reason":"","match":%s}`, asin, name, state, match)
}

func adminTitles(t *testing.T, b *bridgeEnv, query string) catalog.AudibleTitlePage {
	t.Helper()
	resp, out := b.do(t, "GET", "/api/v1/admin/audible/titles?"+query, b.adminTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("titles ?%s: %d %s", query, resp.StatusCode, out)
	}
	var p catalog.AudibleTitlePage
	decodeInto(t, out, &p)
	return p
}

// Security-critical: the Bridge's Audible routes take a bridge token and
// nothing else (a session, an API key, a cast or calendar feed token, a
// listener's, none: 401), and the bridge token gets through.
func TestAudibleBridgeRoutesRefuseOtherCredentials(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	ctx := context.Background()
	cast, _ := b.auth.IssueToken(ctx, b.adminID, auth.KindCast, "cast", 0)
	feed, _ := b.auth.IssueToken(ctx, b.adminID, auth.KindFeed, "feed", 0)
	routes := []struct{ method, path, body string }{
		{"GET", "/api/v1/bridge/users", ""},
		{"PUT", "/api/v1/bridge/audible/accounts/" + testAccount,
			fmt.Sprintf(`{"label":"x","marketplace":"us","user_id":%d,"library_id":%d,"policy":"manual","sync":true}`, b.adminID, b.lib.ID)},
		{"PUT", "/api/v1/bridge/audible/accounts/" + testAccount + "/titles", `{"poll_id":"p1","final":false,"titles":[]}`},
		{"GET", fmt.Sprintf("/api/v1/bridge/libraries/%d/books", b.lib.ID), ""},
		{"PUT", fmt.Sprintf("/api/v1/bridge/libraries/%d/enrichment?path=%s", b.lib.ID, url.QueryEscape(mistbornBook)), `{"asin":"B000000001"}`},
		{"DELETE", "/api/v1/bridge/audible/accounts/" + testAccount, ""},
	}
	creds := map[string]string{"none": "", "admin session": b.adminTok, "admin api key": b.adminKey,
		"user": b.userTok, "cast": cast, "feed": feed, "garbage": "nope"}
	for _, r := range routes {
		for name, tok := range creds {
			if resp, body := b.do(t, r.method, r.path, tok, r.body); resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with %s: %d %s, want 401", r.method, r.path, name, resp.StatusCode, body)
			}
			if tok != "" {
				q := "?token="
				if strings.Contains(r.path, "?") {
					q = "&token="
				}
				if resp, _ := b.do(t, r.method, r.path+q+url.QueryEscape(tok), "", r.body); resp.StatusCode != http.StatusUnauthorized {
					t.Errorf("%s %s with ?token=%s: %d, want 401", r.method, r.path, name, resp.StatusCode)
				}
			}
		}
		if resp, body := b.do(t, r.method, r.path, b.bridgeTok, r.body); resp.StatusCode >= 300 {
			t.Errorf("%s %s with the bridge token: %d %s", r.method, r.path, resp.StatusCode, body)
		}
		// Header only: the bridge token as ?token= is refused too.
		q := "?token="
		if strings.Contains(r.path, "?") {
			q = "&token="
		}
		if resp, _ := b.do(t, r.method, r.path+q+url.QueryEscape(b.bridgeTok), "", r.body); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s with the bridge token as ?token=: %d, want 401", r.method, r.path, resp.StatusCode)
		}
	}
}

// Security-critical: the console's Audible routes are admin-only, refuse the
// bridge token, and answer 404 module_off while the module is off.
func TestAudibleAdminRoutes(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	b.beat(t, bridge.KindVerify, bridge.KindAudiblePoll)
	if code, out := b.putAccount(t, testAccount, b.adminID, b.lib.ID); code != http.StatusOK {
		t.Fatalf("put account: %d %s", code, out)
	}
	routes := []struct{ method, path string }{
		{"GET", "/api/v1/admin/audible/accounts"},
		{"GET", "/api/v1/admin/audible/titles"},
		{"POST", "/api/v1/admin/audible/accounts/" + testAccount + "/poll"},
	}
	// Off: 404 module_off for the admin.
	for _, r := range routes {
		if resp, body := b.do(t, r.method, r.path, b.adminTok, ""); resp.StatusCode != http.StatusNotFound || codeOf(body) != "module_off" {
			t.Errorf("%s %s while off: %d %s, want 404 module_off", r.method, r.path, resp.StatusCode, body)
		}
	}
	setAudible(t, b.testEnv, b.adminTok, true)
	for _, r := range routes {
		if resp, _ := b.do(t, r.method, r.path, b.userTok, ""); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s as a user: %d, want 403", r.method, r.path, resp.StatusCode)
		}
		for name, tok := range map[string]string{"none": "", "bridge": b.bridgeTok} {
			if resp, _ := b.do(t, r.method, r.path, tok, ""); resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with %s: %d, want 401", r.method, r.path, name, resp.StatusCode)
			}
		}
		for name, tok := range map[string]string{"session": b.adminTok, "api key": b.adminKey} {
			if resp, body := b.do(t, r.method, r.path, tok, ""); resp.StatusCode >= 300 {
				t.Errorf("%s %s as the admin's %s: %d %s", r.method, r.path, name, resp.StatusCode, body)
			}
		}
	}
}

// Security-critical: the console removes an account the Bridge can no longer
// remove (admin only, the bridge token refused, whether or not the module is
// on): its titles go and its open poll is cancelled; an unknown one is 204.
func TestAudibleAdminAccountDelete(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	ctx := context.Background()
	b.beat(t, bridge.KindVerify, bridge.KindAudiblePoll)
	for _, id := range []string{testAccount, "acct0002"} {
		if code, out := b.putAccount(t, id, b.adminID, b.lib.ID); code != http.StatusOK {
			t.Fatalf("put %s: %d %s", id, code, out)
		}
		if code, out := b.putTitles(t, id, "p1", true, title("B000000001", "The Final Empire", "available", b.lib.ID, mistbornBook)); code != http.StatusOK {
			t.Fatalf("titles %s: %d %s", id, code, out)
		}
	}
	setAudible(t, b.testEnv, b.adminTok, true)
	if resp, out := b.do(t, "POST", "/api/v1/admin/audible/accounts/"+testAccount+"/poll", b.adminTok, ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("Check now: %d %s", resp.StatusCode, out)
	}
	// Turned off: removal still works (cleanup after the module is off).
	setAudible(t, b.testEnv, b.adminTok, false)
	path := "/api/v1/admin/audible/accounts/" + testAccount
	alexID, alexTok := newUserWithSession(t, b.testEnv, "alex", false)
	userKey, _ := b.auth.IssueToken(ctx, alexID, auth.KindAPI, "key", 0)
	for name, tok := range map[string]string{"user session": alexTok, "user api key": userKey} {
		if resp, _ := b.do(t, "DELETE", path, tok, ""); resp.StatusCode != http.StatusForbidden {
			t.Errorf("delete as %s: %d, want 403", name, resp.StatusCode)
		}
	}
	for name, tok := range map[string]string{"none": "", "bridge": b.bridgeTok} {
		if resp, _ := b.do(t, "DELETE", path, tok, ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("delete with %s: %d, want 401", name, resp.StatusCode)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := b.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM audible_accounts WHERE account = ?`, testAccount); n != 1 {
		t.Fatalf("a refused delete removed the account (%d)", n)
	}
	if resp, out := b.do(t, "DELETE", path, b.adminTok, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete as the admin: %d %s", resp.StatusCode, out)
	}
	if n := count(`SELECT COUNT(*) FROM audible_accounts WHERE account = ?`, testAccount); n != 0 {
		t.Fatalf("account left: %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM audible_titles WHERE account = ?`, testAccount); n != 0 {
		t.Fatalf("titles left: %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM bridge_jobs WHERE kind = ? AND state = 'cancelled'`, bridge.KindAudiblePoll); n != 1 {
		t.Fatalf("cancelled polls = %d, want 1", n)
	}
	if n := count(`SELECT COUNT(*) FROM bridge_jobs WHERE kind = ? AND state IN ('queued','running')`, bridge.KindAudiblePoll); n != 0 {
		t.Fatalf("open polls left = %d", n)
	}
	// The other account is untouched.
	if n := count(`SELECT COUNT(*) FROM audible_titles WHERE account = 'acct0002'`); n != 1 {
		t.Fatalf("other account's titles = %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM audit_events WHERE action = 'bridge.audible.remove' AND details LIKE ?`, "%"+testAccount+"%"); n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
	// Again (now unknown), and the API key: 204, nothing audited.
	if resp, out := b.do(t, "DELETE", path, b.adminKey, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete unknown: %d %s", resp.StatusCode, out)
	}
	if n := count(`SELECT COUNT(*) FROM audit_events WHERE action = 'bridge.audible.remove'`); n != 1 {
		t.Fatalf("an unknown account was audited (%d rows)", n)
	}
	// An id the Bridge could never have made: 404 unknown_account, as the Bridge's route.
	if resp, out := b.do(t, "DELETE", "/api/v1/admin/audible/accounts/NOPE", b.adminTok, ""); resp.StatusCode != http.StatusNotFound || codeOf(out) != "unknown_account" {
		t.Fatalf("bad id: %d %s", resp.StatusCode, out)
	}
}

// GET /bridge/users lists the enabled adults and admins (a grown-up member
// too), never a kid, a demo or a disabled account, and nothing but names.
func TestBridgeUsers(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	ctx := context.Background()
	ids := map[string]int64{}
	for _, name := range []string{"grown", "kiddo", "gone"} {
		ids[name], _ = newUserWithSession(t, b.testEnv, name, false)
	}
	ids["visitor"], _ = newUserWithSession(t, b.testEnv, "visitor", true)
	for _, q := range []struct {
		sql string
		id  int64
	}{
		{`UPDATE users SET managed_by=?1,member_kind='grownup',member_access='all',display_name='Grown Up' WHERE id=?2`, ids["grown"]},
		{`UPDATE users SET managed_by=?1,member_kind='kid',member_access='all' WHERE id=?2`, ids["kiddo"]},
		{`UPDATE users SET disabled=1 WHERE id=?2 AND ?1 > 0`, ids["gone"]},
	} {
		if _, err := b.db.ExecContext(ctx, q.sql, b.adminID, q.id); err != nil {
			t.Fatal(err)
		}
	}
	resp, out := b.do(t, "GET", "/api/v1/bridge/users", b.bridgeTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("users: %d %s", resp.StatusCode, out)
	}
	var got struct {
		Users []map[string]any `json:"users"`
	}
	decodeInto(t, out, &got)
	listed := map[string]map[string]any{}
	for _, u := range got.Users {
		listed[u["username"].(string)] = u
		if len(u) != 4 {
			t.Errorf("user %v carries more than id, username, display_name, admin", u)
		}
	}
	for _, name := range []string{"admin", "sam", "grown"} {
		if listed[name] == nil {
			t.Errorf("%s not listed: %s", name, out)
		}
	}
	for _, name := range []string{"kiddo", "gone", "visitor"} {
		if listed[name] != nil {
			t.Errorf("%s listed: %s", name, out)
		}
	}
	if listed["admin"]["admin"] != true || listed["sam"]["admin"] != false || listed["grown"]["display_name"] != "Grown Up" {
		t.Errorf("users = %s", out)
	}
	// Each one refused is refused as an account's user too (404 unknown_user).
	for _, name := range []string{"kiddo", "gone", "visitor"} {
		if code, out := b.putAccount(t, testAccount, ids[name], b.lib.ID); code != http.StatusNotFound || codeOf(out) != "unknown_user" {
			t.Errorf("account for %s: %d %s, want 404 unknown_user", name, code, out)
		}
	}
	if code, out := b.putAccount(t, testAccount, ids["grown"], b.lib.ID); code != http.StatusOK {
		t.Errorf("account for a grown-up member: %d %s", code, out)
	}
}

// PUT /bridge/audible/accounts/{account}: the mapping, its answer, and each
// refusal.
func TestAudibleAccountPut(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	pods, err := b.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Pods", Root: t.TempDir(), Kind: catalog.KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	code, out := b.putAccount(t, testAccount, b.adminID, b.lib.ID)
	if code != http.StatusOK {
		t.Fatalf("put: %d %s", code, out)
	}
	var got struct {
		Account catalog.AudibleAccount `json:"account"`
	}
	decodeInto(t, out, &got)
	a := got.Account
	if a.ID != testAccount || a.Label != "Chris (US)" || a.Marketplace != "us" || a.User.ID != b.adminID ||
		a.User.DisplayName == "" || a.LibraryID == nil || *a.LibraryID != b.lib.ID || a.Policy != "manual" || a.Sync ||
		a.PollHours != 6 || a.Paused {
		t.Fatalf("account = %s", out)
	}
	if strings.Contains(out, "token") || strings.Contains(out, "password") {
		t.Fatalf("account carries a secret: %s", out)
	}
	for _, c := range []struct {
		name, id, body string
		status         int
		code           string
	}{
		{"bad policy", testAccount, fmt.Sprintf(`{"marketplace":"us","user_id":%d,"library_id":%d,"policy":"some","sync":true}`, b.adminID, b.lib.ID), 400, "invalid_policy"},
		{"bad marketplace", testAccount, fmt.Sprintf(`{"marketplace":"zz","user_id":%d,"library_id":%d,"policy":"all","sync":true}`, b.adminID, b.lib.ID), 400, "invalid_marketplace"},
		{"podcast library", testAccount, fmt.Sprintf(`{"marketplace":"uk","user_id":%d,"library_id":%d,"policy":"all","sync":true}`, b.adminID, pods.ID), 400, "not_books"},
		{"no user", testAccount, fmt.Sprintf(`{"marketplace":"uk","user_id":999,"library_id":%d,"policy":"all","sync":true}`, b.lib.ID), 404, "unknown_user"},
		{"no library", testAccount, fmt.Sprintf(`{"marketplace":"uk","user_id":%d,"library_id":999,"policy":"all","sync":true}`, b.adminID), 404, "unknown_library"},
		{"unknown field", testAccount, `{"marketplace":"uk","password":"x"}`, 400, "invalid_request"},
		{"bad id", "Not-An-Id", `{}`, 404, "unknown_account"},
		{"short id", "abc", `{}`, 404, "unknown_account"},
	} {
		resp, body := b.do(t, "PUT", "/api/v1/bridge/audible/accounts/"+c.id, b.bridgeTok, c.body)
		if resp.StatusCode != c.status || codeOf(body) != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.name, resp.StatusCode, body, c.status, c.code)
		}
	}
	// A refusal changed nothing; a re-map keeps created_at.
	if code, out := b.putAccount(t, testAccount, b.adminID, b.lib.ID); code != http.StatusOK || !strings.Contains(out, a.CreatedAt) {
		t.Fatalf("re-map: %d %s", code, out)
	}
}

// A poll's titles: pages, the final call's gone, the console's columns kept,
// the list's filters, search and paging, the counts, and a removal.
func TestAudibleTitlesPoll(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	ctx := context.Background()
	setAudible(t, b.testEnv, b.adminTok, true)
	if code, out := b.putTitles(t, testAccount, "p1", false); code != http.StatusNotFound || codeOf(out) != "unknown_account" {
		t.Fatalf("titles before the account: %d %s", code, out)
	}
	if code, out := b.putAccount(t, testAccount, b.adminID, b.lib.ID); code != http.StatusOK {
		t.Fatalf("put account: %d %s", code, out)
	}
	if code, out := b.putTitles(t, testAccount, "p1", false,
		title("B000000001", "The Final Empire", "available", b.lib.ID, mistbornBook),
		title("b000000002", "The Well of Ascension", "available", 0, "")); code != http.StatusOK || out != `{"stored":2}`+"\n" && out != `{"stored":2}` {
		t.Fatalf("page 1: %d %q", code, out)
	}
	if code, out := b.putTitles(t, testAccount, "p1", true,
		title("B000000003", "The Hero of Ages", "preorder", 0, ""),
		title("B000000004", "Unknown\tThing", "unavailable", 999, "Gone/Book")); code != http.StatusOK {
		t.Fatalf("page 2: %d %s", code, out)
	}
	all := adminTitles(t, b, "account="+testAccount)
	if len(all.Titles) != 4 || all.Next != "" {
		t.Fatalf("all = %+v", all)
	}
	byASIN := map[string]catalog.AudibleTitle{}
	for _, x := range all.Titles {
		byASIN[x.ASIN] = x
	}
	if m := byASIN["B000000001"].Match; m == nil || m.Path != mistbornBook || m.How != "asin" {
		t.Errorf("matched title = %+v", byASIN["B000000001"])
	}
	if byASIN["B000000002"].Title != "The Well of Ascension" {
		t.Errorf("an ASIN is stored upper case: %+v", all.Titles)
	}
	if x := byASIN["B000000004"]; x.Match != nil || x.Title != "Unknown Thing" {
		t.Errorf("a match to no library is none, text cleaned: %+v", x)
	}
	// The console's columns survive the next poll; a title it misses goes.
	if _, err := b.db.ExecContext(ctx, `UPDATE audible_titles SET rejected=1, dest_path='X/Y' WHERE asin='B000000002'`); err != nil {
		t.Fatal(err)
	}
	if code, out := b.putTitles(t, testAccount, "p2", true,
		title("B000000001", "The Final Empire", "available", b.lib.ID, mistbornBook),
		title("B000000002", "The Well of Ascension", "available", 0, ""),
		title("B000000003", "The Hero of Ages", "available", 0, "")); code != http.StatusOK {
		t.Fatalf("poll 2: %d %s", code, out)
	}
	all = adminTitles(t, b, "account="+testAccount)
	byASIN = map[string]catalog.AudibleTitle{}
	for _, x := range all.Titles {
		byASIN[x.ASIN] = x
	}
	if x := byASIN["B000000002"]; !x.Rejected || x.DestPath != "X/Y" {
		t.Errorf("console columns lost: %+v", x)
	}
	if byASIN["B000000004"].State != catalog.AudibleGone || byASIN["B000000003"].State != catalog.AudibleAvailable {
		t.Errorf("states = %+v", all.Titles)
	}
	// Filters, search, paging.
	if p := adminTitles(t, b, "state=gone"); len(p.Titles) != 1 || p.Titles[0].ASIN != "B000000004" {
		t.Errorf("state=gone = %+v", p.Titles)
	}
	if p := adminTitles(t, b, "matched=1"); len(p.Titles) != 1 || p.Titles[0].ASIN != "B000000001" {
		t.Errorf("matched=1 = %+v", p.Titles)
	}
	if p := adminTitles(t, b, "matched=0&state=available"); len(p.Titles) != 2 {
		t.Errorf("matched=0 available = %+v", p.Titles)
	}
	if p := adminTitles(t, b, "q=well"); len(p.Titles) != 1 || p.Titles[0].ASIN != "B000000002" {
		t.Errorf("q=well = %+v", p.Titles)
	}
	if p := adminTitles(t, b, "q=b000000003"); len(p.Titles) != 1 {
		t.Errorf("q=asin = %+v", p.Titles)
	}
	var seen []string
	after := ""
	for range 5 {
		p := adminTitles(t, b, "limit=3&after="+url.QueryEscape(after))
		for _, x := range p.Titles {
			seen = append(seen, x.Title)
		}
		if after = p.Next; after == "" {
			break
		}
	}
	if strings.Join(seen, "|") != "The Final Empire|The Hero of Ages|The Well of Ascension|Unknown Thing" {
		t.Errorf("pages = %q", seen)
	}
	for _, q := range []string{"state=nope", "matched=maybe", "limit=0", "after=!!!"} {
		if resp, body := b.do(t, "GET", "/api/v1/admin/audible/titles?"+q, b.adminTok, ""); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("?%s: %d %s, want 400", q, resp.StatusCode, body)
		}
	}
	// Bad titles calls are refused whole: a final call without its titles
	// array too, which must never read as "the account owns nothing" (the
	// counts below show nothing went gone).
	for name, body := range map[string]string{
		"no titles":    `{"poll_id":"p3","final":true}`,
		"null titles":  `{"poll_id":"p3","final":true,"titles":null}`,
		"bad asin":     `{"poll_id":"p3","final":false,"titles":[{"asin":"nope","state":"available"}]}`,
		"bad state":    `{"poll_id":"p3","final":false,"titles":[{"asin":"B000000009","state":"gone"}]}`,
		"bad date":     `{"poll_id":"p3","final":false,"titles":[{"asin":"B000000009","state":"available","release_date":"soon"}]}`,
		"bad poll":     `{"poll_id":"","final":false,"titles":[]}`,
		"dirty match":  fmt.Sprintf(`{"poll_id":"p3","titles":[{"asin":"B000000009","state":"available","match":{"library_id":%d,"path":"a/../b","how":"asin"}}]}`, b.lib.ID),
		"bad how":      fmt.Sprintf(`{"poll_id":"p3","titles":[{"asin":"B000000009","state":"available","match":{"library_id":%d,"path":"a","how":"guess"}}]}`, b.lib.ID),
		"unknown":      `{"poll_id":"p3","titles":[],"extra":1}`,
		"credential?":  `{"poll_id":"p3","titles":[{"asin":"B000000009","state":"available","adp_token":"x"}]}`,
		"too negative": `{"poll_id":"p3","titles":[{"asin":"B000000009","state":"available","runtime_s":-1}]}`,
	} {
		resp, out := b.do(t, "PUT", "/api/v1/bridge/audible/accounts/"+testAccount+"/titles", b.bridgeTok, body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, resp.StatusCode, out)
		}
	}
	many := make([]string, catalog.MaxAudibleTitles+1)
	for i := range many {
		many[i] = title(fmt.Sprintf("B%09d", i), "x", "available", 0, "")
	}
	if code, _ := b.putTitles(t, testAccount, "p4", false, many...); code != http.StatusBadRequest {
		t.Errorf("501 titles: %d, want 400", code)
	}
	// The accounts list counts them.
	resp, out := b.do(t, "GET", "/api/v1/admin/audible/accounts", b.adminTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("accounts: %d %s", resp.StatusCode, out)
	}
	var accts struct {
		Accounts []catalog.AudibleAccount `json:"accounts"`
	}
	decodeInto(t, out, &accts)
	if len(accts.Accounts) != 1 || accts.Accounts[0].LastPollAt == nil ||
		*accts.Accounts[0].Titles != (catalog.AudibleCounts{Total: 4, Available: 3, Gone: 1, Matched: 1}) {
		t.Fatalf("accounts = %s", out)
	}
	// Removed: its titles go; a second removal is fine.
	for range 2 {
		if resp, out := b.do(t, "DELETE", "/api/v1/bridge/audible/accounts/"+testAccount, b.bridgeTok, ""); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("delete: %d %s", resp.StatusCode, out)
		}
	}
	if p := adminTitles(t, b, ""); len(p.Titles) != 0 {
		t.Fatalf("titles after removal = %+v", p.Titles)
	}
}

// What Audible says is bounded, never refused: an anthology's long author and
// series lists keep their first 20 and 10, and a match to the library root's
// book (".", a `book` override there) is a match. The search reads each author
// as written, never the stored list's JSON, and LIKE's wildcards are literal.
func TestAudibleTitlesBoundedAndSearched(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	setAudible(t, b.testEnv, b.adminTok, true)
	if code, out := b.putAccount(t, testAccount, b.adminID, b.lib.ID); code != http.StatusOK {
		t.Fatalf("put account: %d %s", code, out)
	}
	authors := make([]string, 30)
	for i := range authors {
		authors[i] = fmt.Sprintf(`"Author %02d"`, i)
	}
	series := make([]string, 12)
	for i := range series {
		series[i] = fmt.Sprintf(`{"name":"Series %02d","position":"%d"}`, i, i)
	}
	anthology := fmt.Sprintf(`{"asin":"B000000010","title":"The Big Anthology","authors":[%s],"series":[%s],"state":"available"}`,
		strings.Join(authors, ","), strings.Join(series, ","))
	duo := fmt.Sprintf(`{"asin":"B000000011","title":"Cat and Mouse","authors":["Tom & Jerry","<Anon>"],"state":"available",
		"match":{"library_id":%d,"path":".","how":"fuzzy"}}`, b.lib.ID)
	if code, out := b.putTitles(t, testAccount, "p1", true, anthology, duo); code != http.StatusOK {
		t.Fatalf("titles: %d %s", code, out)
	}
	big := adminTitles(t, b, "q=anthology").Titles
	if len(big) != 1 || len(big[0].Authors) != 20 || big[0].Authors[19] != "Author 19" ||
		len(big[0].Series) != 10 || big[0].Series[9].Name != "Series 09" {
		t.Fatalf("anthology = %+v", big)
	}
	if p := adminTitles(t, b, "matched=1").Titles; len(p) != 1 || p[0].Match == nil || p[0].Match.Path != "." {
		t.Fatalf("root book match = %+v", p)
	}
	for q, want := range map[string]string{
		"Tom & J":   "B000000011", // json.Marshal stores "&" escaped
		"<anon>":    "B000000011",
		"author 07": "B000000010",
		"author 25": "", // past the first 20
		`"`:         "", // the stored list's quotes and brackets
		"[":         "",
		"%":         "", // LIKE's wildcards
		"_":         "",
	} {
		got := ""
		switch p := adminTitles(t, b, "q="+url.QueryEscape(q)).Titles; len(p) {
		case 0:
		case 1:
			got = p[0].ASIN
		default:
			got = fmt.Sprintf("%d titles", len(p))
		}
		if got != want {
			t.Errorf("q=%q found %q, want %q", q, got, want)
		}
	}
}

// A library deleted: titles matched to it lose the match, accounts mapped to
// it pause (no poll queued for them, Check now 409 account_paused, an open one
// cancelled) until the Bridge maps them again; a book moved or joined carries
// its match; an account whose person can no longer have one is never due.
func TestAudibleLibraryDeleteAndMove(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	ctx := context.Background()
	b.beat(t, bridge.KindVerify, bridge.KindAudiblePoll)
	setAudible(t, b.testEnv, b.adminTok, true)
	other, err := b.cat.CreateLibrary(ctx, catalog.Library{Name: "Other", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if code, out := b.putAccount(t, testAccount, b.adminID, other.ID); code != http.StatusOK {
		t.Fatalf("put account: %d %s", code, out)
	}
	if code, out := b.putAccount(t, "acct0002", b.adminID, b.lib.ID); code != http.StatusOK {
		t.Fatalf("put account 2: %d %s", code, out)
	}
	if code, out := b.putTitles(t, testAccount, "p1", true,
		title("B000000001", "On the other", "available", other.ID, "A/B"),
		title("B000000002", "On main", "available", b.lib.ID, mistbornBook),
		title("B000000003", "On a disc", "available", b.lib.ID, "Some Author/Big Book/CD1")); code != http.StatusOK {
		t.Fatalf("titles: %d %s", code, out)
	}
	poll, created, err := b.api.bridge.Enqueue(ctx, bridge.AudiblePoll(testAccount, "", 0))
	if err != nil || !created {
		t.Fatalf("poll = %+v, %v, %v", poll, created, err)
	}
	// Moved or joined: the match follows the book.
	if err := b.cat.MoveDurableState(ctx, b.lib.ID, mistbornBook, "Brandon Sanderson/Mistborn Era One"); err != nil {
		t.Fatal(err)
	}
	if err := b.cat.JoinDurableState(ctx, b.lib.ID, "Some Author/Big Book", []catalog.JoinPart{
		{Path: "Some Author/Big Book/CD1"}, {Path: "Some Author/Big Book/CD2", Offset: 3600, Last: true},
	}, 7200); err != nil {
		t.Fatal(err)
	}
	if err := b.cat.DeleteLibrary(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	p := adminTitles(t, b, "account="+testAccount)
	for _, x := range p.Titles {
		switch x.ASIN {
		case "B000000001":
			if x.Match != nil {
				t.Errorf("match to a deleted library kept: %+v", x)
			}
		case "B000000002":
			if x.Match == nil || x.Match.Path != "Brandon Sanderson/Mistborn Era One" {
				t.Errorf("match didn't move with its book: %+v", x)
			}
		case "B000000003":
			if x.Match == nil || x.Match.Path != "Some Author/Big Book" {
				t.Errorf("match didn't follow its disc into the joined book: %+v", x)
			}
		}
	}
	acct, err := b.cat.AudibleAccount(ctx, testAccount)
	if err != nil || !acct.Paused || acct.LibraryID != nil {
		t.Fatalf("account after its library went = %+v, %v", acct, err)
	}
	if j, err := b.api.bridge.Job(ctx, poll.ID); err != nil || j.State != bridge.StateCancelled || j.Error != bridge.CancelAudiblePaused {
		t.Fatalf("the paused account's open poll = %+v, %v", j, err)
	}
	resp, out := b.do(t, "POST", "/api/v1/admin/audible/accounts/"+testAccount+"/poll", b.adminTok, "")
	if resp.StatusCode != http.StatusConflict || codeOf(out) != "account_paused" {
		t.Fatalf("Check now on a paused account: %d %s", resp.StatusCode, out)
	}
	// Nor is one whose person was disabled since it was mapped.
	gone, _ := newUserWithSession(t, b.testEnv, "dana", false)
	if code, out := b.putAccount(t, "acct0003", gone, b.lib.ID); code != http.StatusOK {
		t.Fatalf("put account 3: %d %s", code, out)
	}
	if _, err := b.db.ExecContext(ctx, `UPDATE users SET disabled = 1 WHERE id = ?`, gone); err != nil {
		t.Fatal(err)
	}
	due, err := b.cat.DueAudiblePolls(ctx, time.Now(), 10)
	if err != nil || len(due) != 1 || due[0].Account != "acct0002" {
		t.Fatalf("due = %v, %v (a paused account, or a disabled person's, is never due)", due, err)
	}
	// Mapped again, it resumes.
	if code, out := b.putAccount(t, testAccount, b.adminID, b.lib.ID); code != http.StatusOK || strings.Contains(out, `"paused":true`) {
		t.Fatalf("re-map: %d %s", code, out)
	}
	if resp, out := b.do(t, "POST", "/api/v1/admin/audible/accounts/unknown01/poll", b.adminTok, ""); resp.StatusCode != http.StatusNotFound || codeOf(out) != "unknown_account" {
		t.Fatalf("Check now on an unknown account: %d %s", resp.StatusCode, out)
	}
}

// GET /bridge/libraries/{id}/books pages through a books library by path.
func TestBridgeBooksPaged(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	var paths []string
	after := ""
	for range 10 {
		resp, out := b.do(t, "GET", fmt.Sprintf("/api/v1/bridge/libraries/%d/books?limit=1&after=%s", b.lib.ID, url.QueryEscape(after)), b.bridgeTok, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("books: %d %s", resp.StatusCode, out)
		}
		var page struct {
			Books []map[string]any `json:"books"`
			Next  *string          `json:"next"`
		}
		decodeInto(t, out, &page)
		if len(page.Books) != 1 {
			t.Fatalf("page = %s", out)
		}
		bk := page.Books[0]
		for _, k := range []string{"path", "title", "authors", "narrators", "series", "asin", "isbn", "duration"} {
			if _, ok := bk[k]; !ok {
				t.Fatalf("book without %s: %s", k, out)
			}
		}
		paths = append(paths, bk["path"].(string))
		if page.Next == nil {
			if !strings.Contains(out, `"next":null`) {
				t.Fatalf("last page's next isn't null: %s", out)
			}
			break
		}
		after = *page.Next
	}
	n, _ := b.cat.CountBooksByLibrary(context.Background())
	if len(paths) != n[b.lib.ID] || len(paths) < 2 {
		t.Fatalf("paged %v, library holds %d", paths, n[b.lib.ID])
	}
	for i := 1; i < len(paths); i++ {
		if paths[i] <= paths[i-1] {
			t.Fatalf("not in path order: %v", paths)
		}
	}
	pods, err := b.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Pods", Root: t.TempDir(), Kind: catalog.KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		fmt.Sprintf("/api/v1/bridge/libraries/%d/books", pods.ID):          "not_books",
		"/api/v1/bridge/libraries/999/books":                               "unknown_library",
		fmt.Sprintf("/api/v1/bridge/libraries/%d/books?limit=x", b.lib.ID): "invalid_request",
	} {
		if resp, out := b.do(t, "GET", path, b.bridgeTok, ""); resp.StatusCode < 400 || codeOf(out) != want {
			t.Errorf("%s: %d %s, want %s", path, resp.StatusCode, out, want)
		}
	}
}

// GET /bridge/libraries/{id}/books fills a main series' empty position from the
// code leading the book's folder name, when the code is the series' initials;
// the book's own position is unchanged.
func TestBridgeBooksFolderCode(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	ctx := context.Background()
	const dh06 = "Kirill Klevanski/Dragon Heart/DH06 - Land of Magic"
	books := []struct {
		path string
		book catalog.Book
		want string // the listed position
	}{
		{dh06, catalog.Book{Series: "Dragon Heart"}, "6"},
		{"Jim Butcher/Dresden Files/DF15.5 Brief Cases", catalog.Book{Series: "Dresden Files"}, "15.5"},
		{"HALO/HALO 07 - Evolutions (M4B)", catalog.Book{Series: "Halo"}, ""},                                              // a franchise's number
		{"Kirill Klevanski/Dragon Heart/DH07 - Land of Demons", catalog.Book{Series: "Dragon Heart", SeriesIndex: 9}, "9"}, // a stored position wins
		{"Michael Sisa/Legend of the Arch Magus/LAM05-06", catalog.Book{Series: "Legend of the Arch Magus"}, "5-6"},        // an omnibus stays a range
	}
	for _, c := range books {
		bk := c.book
		bk.LibraryID, bk.RelPath, bk.Title, bk.AddedAt = b.lib.ID, c.path, "T", "2020-01-01"
		if _, err := b.cat.UpsertBook(ctx, &bk); err != nil {
			t.Fatal(err)
		}
	}
	resp, out := b.do(t, "GET", fmt.Sprintf("/api/v1/bridge/libraries/%d/books?limit=500", b.lib.ID), b.bridgeTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("books: %d %s", resp.StatusCode, out)
	}
	var page struct {
		Books []bridgeListBook `json:"books"`
	}
	decodeInto(t, out, &page)
	got := map[string]string{}
	for _, bk := range page.Books {
		if len(bk.Series) > 0 {
			got[bk.Path] = bk.Series[0].Position
		}
	}
	for _, c := range books {
		if got[c.path] != c.want {
			t.Errorf("%s: position %q, want %q", c.path, got[c.path], c.want)
		}
	}
	if bk, err := b.cat.GetBookByPath(ctx, b.lib.ID, dh06); err != nil || bk.SeriesIndex != 0 {
		t.Fatalf("the book's own position changed: %+v, %v", bk, err)
	}
}

// bridgeSeries fills only the main series, leaves a stored range alone and
// keeps a range folder code a range.
func TestBridgeSeriesFolderCode(t *testing.T) {
	t.Parallel()
	b := &catalog.Book{RelPath: "A/Dragon Heart/DH06 - Land of Magic", Series: "Dragon Heart", SeriesList: []catalog.SeriesRef{
		{Name: "Dragon Heart"}, {Name: "Dragon Heart Universe"},
	}}
	got := bridgeSeries(b)
	if len(got) != 2 || got[0].Position != "6" || got[1].Position != "" {
		t.Errorf("bridgeSeries = %+v", got)
	}
	// No main series: an other series' position is never filled.
	more := &catalog.Book{RelPath: "A/Dragon Heart/DH06 - Land of Magic", SeriesList: []catalog.SeriesRef{{Name: "Dragon Heart"}}}
	if got := bridgeSeries(more); len(got) != 1 || got[0].Position != "" {
		t.Errorf("bridgeSeries without a main series = %+v", got)
	}
	r := &catalog.Book{RelPath: "A/Dragon Heart/DH06-07", Series: "Dragon Heart", SeriesIndex: 6, SeriesIndexEnd: 7}
	if got := bridgeSeries(r); len(got) != 1 || got[0].Position != "6-7" {
		t.Errorf("a range = %+v", got)
	}
	omnibus := &catalog.Book{RelPath: "A/Legend of the Arch Magus/LAM05-06", Series: "Legend of the Arch Magus"}
	if got := bridgeSeries(omnibus); len(got) != 1 || got[0].Position != "5-6" {
		t.Errorf("a range folder code = %+v", got)
	}
	none := &catalog.Book{RelPath: "A/Standalone"}
	if got := bridgeSeries(none); len(got) != 0 {
		t.Errorf("no series = %+v", got)
	}
}

// PUT /bridge/libraries/{id}/enrichment backfills an ASIN on a book with none,
// once: the same ASIN again is fine, another 409 asin_set.
func TestBridgeEnrichment(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	ctx := context.Background()
	if _, err := b.db.ExecContext(ctx, `UPDATE books SET asin='' WHERE library_id=? AND rel_path=?`, b.lib.ID, mistbornBook); err != nil {
		t.Fatal(err)
	}
	put := func(path, body string) (int, string) {
		resp, out := b.do(t, "PUT", fmt.Sprintf("/api/v1/bridge/libraries/%d/enrichment?path=%s", b.lib.ID, url.QueryEscape(path)), b.bridgeTok, body)
		return resp.StatusCode, out
	}
	if code, out := put(mistbornBook, `{"asin":"B0TESTASIN"}`); code != http.StatusNoContent {
		t.Fatalf("backfill: %d %s", code, out)
	}
	book, err := b.cat.GetBookByPath(ctx, b.lib.ID, mistbornBook)
	if err != nil || book.ASIN != "B0TESTASIN" {
		t.Fatalf("book asin = %q, %v", book.ASIN, err)
	}
	if code, out := put(mistbornBook, `{"asin":"B0TESTASIN"}`); code != http.StatusNoContent {
		t.Fatalf("same asin again: %d %s", code, out)
	}
	if code, out := put(mistbornBook, `{"asin":"B0OTHERONE"}`); code != http.StatusConflict || codeOf(out) != "asin_set" {
		t.Fatalf("another asin: %d %s", code, out)
	}
	if code, out := put("Nobody/Here", `{"asin":"B0TESTASIN"}`); code != http.StatusNotFound {
		t.Fatalf("no book: %d %s", code, out)
	}
	for name, body := range map[string]string{"bad asin": `{"asin":"x"}`, "isbn too": `{"asin":"B0TESTASIN","isbn":"1"}`} {
		if code, out := put(mistbornBook, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}
	// An admin's ASIN edit, a cleared one too, is never gone under (409); that,
	// the same ASIN again and a refusal write nothing: the content generation
	// (the player's list caches) stays, though the Bridge asks after every poll.
	if err := b.cat.EditBook(ctx, b.lib.ID, cradleBook, catalog.BookEdit{Set: map[string]string{catalog.FieldASIN: ""}, UserID: b.adminID}); err != nil {
		t.Fatal(err)
	}
	gen := b.db.ContentGeneration()
	if code, out := put(cradleBook, `{"asin":"B0TESTASIN"}`); code != http.StatusConflict || codeOf(out) != "asin_set" {
		t.Fatalf("under a cleared ASIN edit: %d %s", code, out)
	}
	for _, path := range []string{mistbornBook, "Nobody/Here"} {
		_, _ = put(path, `{"asin":"B0TESTASIN"}`)
	}
	if after := b.db.ContentGeneration(); after != gen {
		t.Fatalf("content generation moved %d -> %d with nothing written", gen, after)
	}
	var stored int
	if err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM book_enrichment WHERE library_id = ? AND path = ?`, b.lib.ID, cradleBook).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("enrichment under the edit: %d rows, %v", stored, err)
	}
	// A podcast's episode is no book (404); the library root's book (".", a
	// `book` override there) is one.
	pods, err := b.cat.CreateLibrary(ctx, catalog.Library{Name: "Pods", Root: t.TempDir(), Kind: catalog.KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.cat.UpsertBook(ctx, &catalog.Book{LibraryID: pods.ID, RelPath: "Show/Episode.mp3", Title: "Episode", AddedAt: "2020-01-01"}); err != nil {
		t.Fatal(err)
	}
	resp, out := b.do(t, "PUT", fmt.Sprintf("/api/v1/bridge/libraries/%d/enrichment?path=%s", pods.ID, url.QueryEscape("Show/Episode.mp3")), b.bridgeTok, `{"asin":"B0TESTASIN"}`)
	if resp.StatusCode != http.StatusNotFound || codeOf(out) != "book_not_found" {
		t.Fatalf("a podcast episode: %d %s", resp.StatusCode, out)
	}
	if _, err := b.cat.UpsertBook(ctx, &catalog.Book{LibraryID: b.lib.ID, RelPath: ".", Title: "Everything", AddedAt: "2020-01-01"}); err != nil {
		t.Fatal(err)
	}
	if code, out := put(".", `{"asin":"B0ROOTBOOK"}`); code != http.StatusNoContent {
		t.Fatalf("the root book: %d %s", code, out)
	}
	if book, err := b.cat.GetBookByPath(ctx, b.lib.ID, "."); err != nil || book.ASIN != "B0ROOTBOOK" {
		t.Fatalf("root book = %+v, %v", book, err)
	}
}

// The scheduler queues each mapped account's poll when due, once (deduped per
// account), only with an online Bridge that runs audible.poll; a queued poll
// is held while the module is off; and the module's background work runs
// only while it is on.
func TestAudibleScheduler(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	ctx := context.Background()
	d := modules.Deps{Log: slog.New(slog.DiscardHandler), Catalog: b.cat, Bridge: b.api.bridge}
	for _, id := range []string{testAccount, "acct0002"} {
		if code, out := b.putAccount(t, id, b.adminID, b.lib.ID); code != http.StatusOK {
			t.Fatalf("put %s: %d %s", id, code, out)
		}
	}
	countPolls := func() map[string]int {
		t.Helper()
		rows, err := b.db.QueryContext(ctx, `SELECT params, state FROM bridge_jobs WHERE kind = ?`, bridge.KindAudiblePoll)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		out := map[string]int{}
		for rows.Next() {
			var params, state string
			if err := rows.Scan(&params, &state); err != nil {
				t.Fatal(err)
			}
			var p bridge.AudiblePollParams
			_ = json.Unmarshal([]byte(params), &p)
			out[p.Account+":"+state]++
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// A Bridge without Audible: nothing queued, nothing marked.
	now := time.Now()
	if err := audible.SchedulerTick(ctx, d, now); err != nil {
		t.Fatal(err)
	}
	if n := countPolls(); len(n) != 0 {
		t.Fatalf("polls without the kind = %v", n)
	}
	b.beat(t, bridge.KindVerify, bridge.KindAudiblePoll)
	if err := audible.SchedulerTick(ctx, d, now); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{testAccount + ":queued": 1, "acct0002:queued": 1}
	if n := countPolls(); fmt.Sprint(n) != fmt.Sprint(want) {
		t.Fatalf("first tick = %v, want %v", n, want)
	}
	// Not due again within poll_hours; due after, but deduped while open.
	if err := audible.SchedulerTick(ctx, d, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := audible.SchedulerTick(ctx, d, now.Add(7*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := countPolls(); fmt.Sprint(n) != fmt.Sprint(want) {
		t.Fatalf("later ticks = %v, want %v (one open poll per account)", n, want)
	}
	// Check now returns the open poll (200), not a second, raised from the
	// scheduler's priority to a person's.
	setAudible(t, b.testEnv, b.adminTok, true)
	resp, out := b.do(t, "POST", "/api/v1/admin/audible/accounts/"+testAccount+"/poll", b.adminTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Check now with one open: %d %s", resp.StatusCode, out)
	}
	var adopted bridge.Job
	decodeInto(t, out, &adopted)
	if adopted.Priority != 0 {
		t.Fatalf("Check now left the scheduled poll at priority %d", adopted.Priority)
	}
	if j, err := b.api.bridge.Job(ctx, adopted.ID); err != nil || j.Priority != 0 {
		t.Fatalf("stored poll = %+v, %v", j, err)
	}
	// While the module is off a queued poll is held: the claim passes it over.
	setAudible(t, b.testEnv, b.adminTok, false)
	if resp, out := b.do(t, "GET", "/api/v1/bridge/jobs/next?wait=0&kind="+bridge.KindAudiblePoll, b.bridgeTok, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("claim while off: %d %s, want 204", resp.StatusCode, out)
	}
	setAudible(t, b.testEnv, b.adminTok, true)
	resp, out = b.do(t, "GET", "/api/v1/bridge/jobs/next?wait=0&kind="+bridge.KindAudiblePoll, b.bridgeTok, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, `"kind":"audible.poll"`) || !strings.Contains(out, `"library_id":null`) {
		t.Fatalf("claim while on: %d %s", resp.StatusCode, out)
	}
	// An account removed cancels its open poll.
	if resp, out := b.do(t, "DELETE", "/api/v1/bridge/audible/accounts/acct0002", b.bridgeTok, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d %s", resp.StatusCode, out)
	}
	if n := countPolls(); n["acct0002:cancelled"] != 1 {
		t.Fatalf("polls after removal = %v", n)
	}
	var reason string
	if err := b.db.QueryRowContext(ctx, `SELECT error FROM bridge_jobs WHERE kind = ? AND state = 'cancelled'`,
		bridge.KindAudiblePoll).Scan(&reason); err != nil || !bridge.IsCancelReason(reason) || reason != bridge.CancelAudibleRemoved {
		t.Fatalf("cancelled poll's reason = %q, %v", reason, err)
	}
}

// The module's background work (audible.RunScheduler, the registry's Start)
// queues polls only while the module is on.
func TestAudibleSchedulerRunsOnlyWhileOn(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	b := newBridgeEnvOn(t, e)
	b.beat(t, bridge.KindVerify, bridge.KindAudiblePoll)
	if code, out := b.putAccount(t, testAccount, b.adminID, b.lib.ID); code != http.StatusOK {
		t.Fatalf("put: %d %s", code, out)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.api.StartModules(ctx)
	defer b.api.StopModules()
	polls := func() int {
		var n int
		if err := b.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM bridge_jobs WHERE kind = ?`, bridge.KindAudiblePoll).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	time.Sleep(300 * time.Millisecond)
	if n := polls(); n != 0 {
		t.Fatalf("%d polls while the module is off", n)
	}
	setAudible(t, b.testEnv, b.adminTok, true) // the save starts its Start (syncModules)
	deadline := time.Now().Add(5 * time.Second)
	for polls() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := polls(); n != 1 {
		t.Fatalf("%d polls once on, want 1", n)
	}
}

// The account's marketplaces are the metadata's regions.
func TestAudibleMarketplacesAreTheRegions(t *testing.T) {
	t.Parallel()
	if fmt.Sprint(catalog.AudibleMarketplaces) != fmt.Sprint(config.Regions) {
		t.Fatalf("marketplaces %v, regions %v", catalog.AudibleMarketplaces, config.Regions)
	}
}

// A poll's titles and its poll times are user state: they leave the content
// generation (the player's list caches) alone, and so do the sync's times
// (Phase 4c: queued, last synced, refused and a complete poll ending the
// refusal); mapping an account is content.
func TestAudibleTitlesAreUserState(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	ctx := context.Background()
	if code, out := b.putAccount(t, testAccount, b.adminID, b.lib.ID); code != http.StatusOK {
		t.Fatalf("put: %d %s", code, out)
	}
	before := b.db.ContentGeneration()
	if code, out := b.putTitles(t, testAccount, "p1", true, title("B000000001", "x", "available", b.lib.ID, mistbornBook)); code != http.StatusOK {
		t.Fatalf("titles: %d %s", code, out)
	}
	if err := b.cat.MarkAudibleQueued(ctx, testAccount); err != nil {
		t.Fatal(err)
	}
	if err := b.cat.MarkAudibleSyncQueued(ctx, testAccount); err != nil {
		t.Fatal(err)
	}
	sync, _ := bridge.AudibleSync(testAccount, nil, 0)
	for _, j := range []bridge.Job{
		{Kind: bridge.KindAudibleSync, Params: sync.Params, State: bridge.StateDone},
		{Kind: bridge.KindAudibleSync, Params: sync.Params, State: bridge.StateFailed, Error: bridge.SyncAudibleRefused},
	} {
		if err := b.cat.FoldAudibleSync(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	if a, err := b.cat.AudibleAccount(ctx, testAccount); err != nil || a.LastSyncAt == nil || a.RefusedAt == nil {
		t.Fatalf("after the syncs: %+v, %v", a, err)
	}
	if code, out := b.putTitles(t, testAccount, "p2", true, title("B000000001", "x", "available", b.lib.ID, mistbornBook)); code != http.StatusOK {
		t.Fatalf("titles again: %d %s", code, out)
	}
	if after := b.db.ContentGeneration(); after != before {
		t.Fatalf("content generation moved %d -> %d", before, after)
	}
}
