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
		a.User.DisplayName == "" || a.LibraryID == nil || *a.LibraryID != b.lib.ID || a.Policy != "manual" || !a.Sync ||
		a.PollHours != catalog.AudibleDefaultPollHours || a.Paused {
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
	if p := adminTitles(t, b, "q=50%25_"); len(p.Titles) != 0 {
		t.Errorf("a LIKE wildcard matched: %+v", p.Titles)
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
	// Bad titles calls are refused whole.
	for name, body := range map[string]string{
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

// A library deleted: titles matched to it lose the match, accounts mapped to
// it pause (no poll queued for them, Check now 409 account_paused) until the
// Bridge maps them again; a book moved carries its match.
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
		title("B000000002", "On main", "available", b.lib.ID, mistbornBook)); code != http.StatusOK {
		t.Fatalf("titles: %d %s", code, out)
	}
	// Moved: the match follows the book.
	if err := b.cat.MoveDurableState(ctx, b.lib.ID, mistbornBook, "Brandon Sanderson/Mistborn Era One"); err != nil {
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
		}
	}
	acct, err := b.cat.AudibleAccount(ctx, testAccount)
	if err != nil || !acct.Paused || acct.LibraryID != nil {
		t.Fatalf("account after its library went = %+v, %v", acct, err)
	}
	resp, out := b.do(t, "POST", "/api/v1/admin/audible/accounts/"+testAccount+"/poll", b.adminTok, "")
	if resp.StatusCode != http.StatusConflict || codeOf(out) != "account_paused" {
		t.Fatalf("Check now on a paused account: %d %s", resp.StatusCode, out)
	}
	due, err := b.cat.DueAudiblePolls(ctx, time.Now(), 10)
	if err != nil || len(due) != 1 || due[0] != "acct0002" {
		t.Fatalf("due = %v, %v (a paused account is never due)", due, err)
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
	// Check now returns the open poll (200), not a second.
	setAudible(t, b.testEnv, b.adminTok, true)
	resp, out := b.do(t, "POST", "/api/v1/admin/audible/accounts/"+testAccount+"/poll", b.adminTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Check now with one open: %d %s", resp.StatusCode, out)
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
// generation (the player's list caches) alone; mapping an account is content.
func TestAudibleTitlesAreUserState(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	if code, out := b.putAccount(t, testAccount, b.adminID, b.lib.ID); code != http.StatusOK {
		t.Fatalf("put: %d %s", code, out)
	}
	before := b.db.ContentGeneration()
	if code, out := b.putTitles(t, testAccount, "p1", true, title("B000000001", "x", "available", b.lib.ID, mistbornBook)); code != http.StatusOK {
		t.Fatalf("titles: %d %s", code, out)
	}
	if err := b.cat.MarkAudibleQueued(context.Background(), testAccount); err != nil {
		t.Fatal(err)
	}
	if after := b.db.ContentGeneration(); after != before {
		t.Fatalf("content generation moved %d -> %d", before, after)
	}
}
