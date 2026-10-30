package api

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Track U3: the calendar feed (a feed token, releases.ics) and the Upcoming
// covers. Security-critical: every token kind against every route that could
// take it, allowed and denied.

const (
	calendarPath = "/api/v1/me/upcoming/calendar"
	feedPath     = "/api/v1/me/upcoming/releases.ics"
	upCoverPath  = "/api/v1/me/upcoming/cover"
)

// newCalendarEnv is newUpcomingEnv with a dated preorder (with a cover) in
// "Other Series" too, and limits that let a test make many requests.
func newCalendarEnv(t *testing.T) *upcomingEnv {
	t.Helper()
	u := newUpcomingEnv(t)
	u.api.ipLimiter = newRateLimiter(100000, 100000)
	u.api.mediaLimiter = newRateLimiter(100000, 100000)
	u.api.tokenLimiter = newRateLimiter(100000, 100000)
	u.series(t, u.lib.ID, "Other Series", "other", []catalog.MetaSeriesMember{
		{WorkID: "o1", Position: "1", Title: "One"}, {WorkID: "o2", Position: "2", Title: "Secret Two"},
		{WorkID: "o3", Position: "3", Title: "Secret Three", ReleaseDate: "2099-05-05", CoverURL: "https://c/o3.jpg"},
	}, []catalog.UpcomingItem{
		{WorkID: "o2", Kind: catalog.UpcomingMissing, Position: "2"},
		{WorkID: "o3", Kind: catalog.UpcomingPreorder, ReleaseDate: "2099-05-05", Position: "3"},
	})
	return u
}

// expanseReader is a listener signed in on one device whose one share is the
// library's "Expanse" folder; it returns their id, session and the share's id.
func expanseReader(t *testing.T, u *upcomingEnv) (id int64, tok string, shareID int64) {
	t.Helper()
	ctx := context.Background()
	id, tok = newUserWithSession(t, u.testEnv, "reader", false)
	share, err := u.cat.CreateShare(ctx, catalog.Share{Name: "Expanse only", Paths: []catalog.PathRule{{LibraryID: u.lib.ID, Path: "Expanse"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := u.cat.GrantShare(ctx, id, share.ID); err != nil {
		t.Fatal(err)
	}
	return id, tok, share.ID
}

// holdCopy gives the admin a copy of workID (identified by idKey) at path in a
// second library, so their Upcoming list drops it.
func holdCopy(t *testing.T, u *upcomingEnv, path, idKey, workID string) {
	t.Helper()
	ctx := context.Background()
	other, err := u.cat.CreateLibrary(ctx, catalog.Library{Name: "Other", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	u.book(t, other.ID, path, "", 0)
	if err := u.cat.SaveUpcomingOwned(ctx, []catalog.UpcomingOwned{{Ref: catalog.Ref{LibraryID: other.ID, Path: path},
		IDKey: idKey, WorkID: workID, CheckedAt: "2026-10-20T04:00:00.000Z"}}, nil); err != nil {
		t.Fatal(err)
	}
}

// mintFeed POSTs /me/upcoming/calendar as tok, wanting 201, and returns the
// feed's path (with its ?token=) and the secret.
func mintFeed(t *testing.T, e *testEnv, tok string) (path, secret string) {
	t.Helper()
	resp, body := e.do(t, "POST", calendarPath, tok, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint = %d %s", resp.StatusCode, body)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("mint Cache-Control = %q, want no-store", cc)
	}
	var link struct {
		Path      string `json:"path"`
		CreatedAt string `json:"created_at"`
	}
	decodeInto(t, body, &link)
	p, err := url.Parse(link.Path)
	if err != nil || p.Path != feedPath || p.Query().Get("token") == "" || link.CreatedAt == "" || p.Host != "" {
		t.Fatalf("mint answer = %s", body)
	}
	return link.Path, p.Query().Get("token")
}

func feedURL(secret string) string { return feedPath + "?token=" + url.QueryEscape(secret) }

func calendarStatus(t *testing.T, e *testEnv, tok string) (active bool, created string) {
	t.Helper()
	resp, body := e.do(t, "GET", calendarPath, tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d %s", resp.StatusCode, body)
	}
	var st struct {
		Active    bool   `json:"active"`
		CreatedAt string `json:"created_at"`
	}
	decodeInto(t, body, &st)
	if !st.Active && strings.Contains(body, "created_at") {
		t.Fatalf("no link, yet a created_at: %s", body)
	}
	return st.Active, st.CreatedAt
}

func TestUpcomingCalendarWire(t *testing.T) {
	t.Parallel()
	u := newCalendarEnv(t)
	tok := adminSession(t, u.testEnv)

	if active, _ := calendarStatus(t, u.testEnv, tok); active {
		t.Fatal("a link before any mint")
	}
	path, secret := mintFeed(t, u.testEnv, tok)
	active, created := calendarStatus(t, u.testEnv, tok)
	if !active || created == "" {
		t.Fatalf("status after the mint = %v %q", active, created)
	}
	// The secret is never stored as it is, nor answered again.
	var plain int
	if err := u.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM tokens WHERE token_hash = ?`, secret).Scan(&plain); err != nil || plain != 0 {
		t.Fatalf("plain secret rows = %d %v", plain, err)
	}
	if _, body := u.do(t, "GET", calendarPath, tok, ""); strings.Contains(body, secret) {
		t.Fatal("the status hands the secret out again")
	}

	// Allowed: the feed by ?token= (the minted path as it is) and by header.
	resp, body := u.do(t, "GET", path, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("feed = %d %s", resp.StatusCode, body)
	}
	for h, want := range map[string]string{"Content-Type": "text/calendar; charset=utf-8",
		"Content-Disposition": `inline; filename="releases.ics"`, "Cache-Control": "private, max-age=3600"} {
		if got := resp.Header.Get(h); got != want {
			t.Errorf("%s = %q, want %q", h, got, want)
		}
	}
	if r, raw := u.raw(t, "GET", path, "", true, nil); r.Header.Get("Content-Encoding") != "" || string(raw) != body {
		t.Errorf("the feed to a client taking gzip = %q, %d bytes; want it as it is", r.Header.Get("Content-Encoding"), len(raw))
	}
	// Revalidated by its ETag, a digest of the calendar.
	etag := resp.Header.Get("ETag")
	if !strings.HasPrefix(etag, `"ics-`) {
		t.Fatalf("feed ETag = %q", etag)
	}
	if r, _ := u.doHeaders(t, "GET", path, "", "", map[string]string{"If-None-Match": etag}); r.StatusCode != http.StatusNotModified ||
		r.Header.Get("Cache-Control") != upcomingCalendarCache {
		t.Fatalf("feed revalidated = %d %v, want 304", r.StatusCode, r.Header)
	}
	if r, _ := u.doHeaders(t, "GET", path, "", "", map[string]string{"If-None-Match": `"ics-0"`}); r.StatusCode != http.StatusOK {
		t.Fatalf("feed with a stale ETag = %d, want 200", r.StatusCode)
	}
	for _, want := range []string{"BEGIN:VCALENDAR\r\n", "UID:e4@srv-test\r\n", "UID:e3@srv-test\r\n", "UID:o3@srv-test\r\n",
		"DTSTART;VALUE=DATE:20991112\r\n", "SUMMARY:Cibola Burn (The Expanse #4)\r\n", "URL:https://meta.audiosilo.app/work?id=e4\r\n",
		"END:VCALENDAR\r\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("feed lacks %q:\n%s", want, body)
		}
	}
	// Left out: a month-only preorder (e5), missing volumes (e2, o2).
	for _, left := range []string{"UID:e5@", "UID:e2@", "UID:o2@"} {
		if strings.Contains(body, left) {
			t.Errorf("feed holds %s:\n%s", left, body)
		}
	}
	if resp, body := u.do(t, "GET", feedPath, secret, ""); resp.StatusCode != http.StatusOK || !strings.Contains(body, "UID:e4@srv-test") {
		t.Fatalf("feed by header = %d %s", resp.StatusCode, body)
	}

	// Reset: the old link dies, the new one works, one row.
	_, second := mintFeed(t, u.testEnv, tok)
	if resp, _ := u.do(t, "GET", feedURL(secret), "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the old link after a reset = %d, want 401", resp.StatusCode)
	}
	if resp, _ := u.do(t, "GET", feedPath, secret, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the old link by header after a reset = %d, want 401", resp.StatusCode)
	}
	if resp, _ := u.do(t, "GET", feedURL(second), "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("the new link = %d", resp.StatusCode)
	}
	var rows int
	if err := u.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM tokens WHERE user_id = ? AND kind = 'feed'`, u.adminID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("feed rows = %d %v, want 1", rows, err)
	}

	// Turn off: 204, again 204; the link dies, the status says none.
	for range 2 {
		if resp, body := u.do(t, "DELETE", calendarPath, tok, ""); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("turn off = %d %s", resp.StatusCode, body)
		}
	}
	if resp, _ := u.do(t, "GET", feedURL(second), "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the link after turning off = %d, want 401", resp.StatusCode)
	}
	if active, _ := calendarStatus(t, u.testEnv, tok); active {
		t.Fatal("a link after turning off")
	}
}

// Allowed and denied: the feed is the token owner's own list, read with their
// scopes at each request: another person's series never appears, a work the
// owner holds a copy of is dropped, and nobody's feed shows another's.
func TestUpcomingFeedScoped(t *testing.T) {
	t.Parallel()
	u := newCalendarEnv(t)
	ctx := context.Background()
	readerID, readerTok, shareID := expanseReader(t, u)
	_, readerFeed := mintFeed(t, u.testEnv, readerTok)
	_, adminFeed := mintFeed(t, u.testEnv, adminSession(t, u.testEnv))

	_, body := u.do(t, "GET", feedURL(readerFeed), "", "")
	if !strings.Contains(body, "UID:e4@") || strings.Contains(body, "UID:o3@") || strings.Contains(body, "Secret Three") {
		t.Fatalf("the reader's feed must hold the Expanse only:\n%s", body)
	}
	if _, body := u.do(t, "GET", feedURL(adminFeed), "", ""); !strings.Contains(body, "UID:o3@") {
		t.Fatalf("the admin's feed must hold o3:\n%s", body)
	}

	// The admin gets a copy of e4 in another library: dropped from their feed only.
	holdCopy(t, u, "Loose/Cibola", "a:B4", "e4")
	if _, body := u.do(t, "GET", feedURL(adminFeed), "", ""); strings.Contains(body, "UID:e4@") {
		t.Fatalf("a work the owner holds must be dropped:\n%s", body)
	}
	if _, body := u.do(t, "GET", feedURL(readerFeed), "", ""); !strings.Contains(body, "UID:e4@") {
		t.Fatalf("a copy the reader can't see must not drop it:\n%s", body)
	}

	// A share taken away is felt at the next request.
	if err := u.cat.RevokeShare(ctx, readerID, shareID); err != nil {
		t.Fatal(err)
	}
	if _, body := u.do(t, "GET", feedURL(readerFeed), "", ""); strings.Contains(body, "BEGIN:VEVENT") {
		t.Fatalf("no access, no events:\n%s", body)
	}
}

// A server that has never run the job: an empty calendar (no component, which
// RFC 5545 asks for one of; calendar apps read it as empty).
func TestUpcomingFeedPendingIsEmpty(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	_, secret := mintFeed(t, m.testEnv, m.adamTok)
	resp, body := m.do(t, "GET", feedURL(secret), "", "")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, "BEGIN:VCALENDAR\r\n") || !strings.HasSuffix(body, "END:VCALENDAR\r\n") ||
		strings.Contains(body, "VEVENT") {
		t.Fatalf("pending feed = %d %q", resp.StatusCode, body)
	}
}

// otherKindTokens are a session, an API key, a cast, a bridge and a pairing
// token of the admin's (none of which the feed takes).
func otherKindTokens(t *testing.T, u *upcomingEnv) map[string]string {
	t.Helper()
	ctx := context.Background()
	sess := adminSession(t, u.testEnv)
	_, key := u.mintAPIKey(t, sess, "script")
	_, cred, err := u.auth.ResolveRequest(ctx, sess, auth.Presence{}, auth.KindSession)
	if err != nil {
		t.Fatal(err)
	}
	cast, _, err := u.auth.IssueCastToken(ctx, u.adminID, cred.ID, u.lib.ID, "Expanse/1")
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := u.auth.IssueBridgePairCode(ctx, u.adminID)
	if err != nil {
		t.Fatal(err)
	}
	bridgeTok, err := u.auth.ExchangeBridgeCode(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	pairing, err := u.auth.IssueToken(ctx, u.adminID, auth.KindPairing, "", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"session": sess, "api key": key, "cast": cast, "bridge": bridgeTok, "pairing": pairing}
}

// Security-critical: the feed takes a feed token and nothing else, as
// ?token= or as the header.
func TestUpcomingFeedRefusesOtherKinds(t *testing.T) {
	t.Parallel()
	u := newCalendarEnv(t)
	for name, tok := range otherKindTokens(t, u) {
		if resp, _ := u.do(t, "GET", feedURL(tok), "", ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("feed with a %s as ?token= = %d, want 401", name, resp.StatusCode)
		}
		if resp, _ := u.do(t, "GET", feedPath, tok, ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("feed with a %s as the header = %d, want 401", name, resp.StatusCode)
		}
	}
	for name, path := range map[string]string{"none": feedPath, "bogus": feedURL("not-a-token"), "empty": feedPath + "?token="} {
		if resp, _ := u.do(t, "GET", path, "", ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("feed with %s = %d, want 401", name, resp.StatusCode)
		}
	}
}

// Security-critical: a feed token is refused on every other authenticated
// route of the table, as the header and as ?token= (the media routes, the
// Upcoming cover, /meta/cover and the Bridge's routes included), and the feed
// route takes it alone (requireFeedAuth). TestFeedKindNamedOnce covers the
// routes the table doesn't list.
func TestFeedTokenRefusedOnEveryOtherRoute(t *testing.T) {
	t.Parallel()
	u := newCalendarEnv(t)
	_, secret := mintFeed(t, u.testEnv, adminSession(t, u.testEnv))
	var others []authedRoute
	feeds := 0
	for _, r := range authedRoutes(t) {
		switch {
		case r.path == feedPath:
			if r.wrapper != "requireFeedAuth" {
				t.Errorf("the feed route is wrapped in %s", r.wrapper)
			}
			feeds++
		case r.wrapper == "requireFeedAuth":
			t.Errorf("%s %s takes a feed token", r.method, r.path)
		default:
			others = append(others, r)
		}
	}
	if feeds != 1 {
		t.Fatalf("found the feed route %d times", feeds)
	}
	refusedOnRoutes(t, u.testEnv, "feed", secret, others, strconv.FormatInt(u.lib.ID, 10), "path=Expanse%2F1&work=e3&url=x&size=160")
}

// Security-critical, wherever a route is registered (api.go's table, a
// module's routes, the podcast transfer routes, a built pattern): the package
// names the feed kind once (feedKinds), uses feedKinds only in requireFeedAuth,
// and wraps one route in requireFeedAuth, so no route but the feed takes a
// feed token.
func TestFeedKindNamedOnce(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var kind, kinds, wraps []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if x, ok := n.X.(*ast.Ident); ok && x.Name == "auth" && n.Sel.Name == "KindFeed" {
					kind = append(kind, fset.Position(n.Pos()).String())
				}
				if n.Sel.Name == "requireFeedAuth" {
					wraps = append(wraps, fset.Position(n.Pos()).String())
				}
			case *ast.Ident:
				if n.Name == "feedKinds" {
					kinds = append(kinds, fset.Position(n.Pos()).String())
				}
			}
			return true
		})
	}
	if len(kind) != 1 || len(kinds) != 2 || len(wraps) != 1 {
		t.Fatalf("auth.KindFeed at %v (want feedKinds alone), feedKinds at %v (want its declaration and "+
			"requireFeedAuth), requireFeedAuth wrapping %v (want the feed route alone)", kind, kinds, wraps)
	}
}

// The mint's refusals in order: 401, 403 kid_account, 403 demo_account, 403
// for an API key, then 404 metadata_off / upcoming_off. The status read and
// Turn off are refused for a kid; Turn off is refused for an API key but works
// with the feature off.
func TestUpcomingCalendarRefusals(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	m.api.tokenLimiter = newRateLimiter(100000, 100000)
	m.api.mediaLimiter = newRateLimiter(100000, 100000)
	for _, method := range []string{"GET", "POST", "DELETE"} {
		if s := m.status(t, method, calendarPath, "", ""); s != http.StatusUnauthorized {
			t.Errorf("%s anonymous = %d, want 401", method, s)
		}
	}
	_, kidTok := m.kid(t, m.adamTok, "Evie")
	for _, method := range []string{"GET", "POST", "DELETE"} {
		if r, b := m.do(t, method, calendarPath, kidTok, ""); r.StatusCode != http.StatusForbidden || codeOf(b) != "kid_account" {
			t.Errorf("%s as a kid = %d %s, want 403 kid_account", method, r.StatusCode, b)
		}
	}
	for _, c := range []struct{ path, tok string }{{upCoverPath + "?work=e3", kidTok}, {upCoverPath + "?work=e3&token=" + url.QueryEscape(kidTok), ""}} {
		if r, b := m.do(t, "GET", c.path, c.tok, ""); r.StatusCode != http.StatusForbidden || codeOf(b) != "kid_account" {
			t.Errorf("cover as a kid = %d %s, want 403 kid_account", r.StatusCode, b)
		}
	}

	_, key := m.mintAPIKey(t, m.adamTok, "script")
	if r, b := m.do(t, "POST", calendarPath, key, ""); r.StatusCode != http.StatusForbidden {
		t.Errorf("mint with an API key = %d %s, want 403", r.StatusCode, b)
	}
	if r, b := m.do(t, "DELETE", calendarPath, key, ""); r.StatusCode != http.StatusForbidden {
		t.Errorf("turn off with an API key = %d %s, want 403", r.StatusCode, b)
	}
	if active, _ := calendarStatus(t, m.testEnv, key); active { // the read is allowed
		t.Error("a link before any mint")
	}

	_, demoTok := newUserWithSession(t, m.testEnv, "demo_viewer", true)
	if r, b := m.do(t, "POST", calendarPath, demoTok, ""); r.StatusCode != http.StatusForbidden || codeOf(b) != "demo_account" {
		t.Errorf("mint as a demo = %d %s, want 403 demo_account", r.StatusCode, b)
	}
	if active, _ := calendarStatus(t, m.testEnv, demoTok); active {
		t.Error("a demo account with a link")
	}

	// A grown-up mints, then the feature goes off: every route but Turn off
	// answers the 404s, in order; the API key and demo refusals come first.
	_, adamFeed := mintFeed(t, m.testEnv, m.adamTok)
	adminTok := m.adminTok
	for _, step := range []struct {
		patch, code string
	}{{`{"metadata":{"upcoming":false}}`, "upcoming_off"}, {`{"metadata":{"upcoming":true,"enabled":false}}`, "metadata_off"}} {
		if r, b := m.do(t, "PATCH", "/api/v1/admin/settings", adminTok, step.patch); r.StatusCode != http.StatusOK {
			t.Fatalf("settings %s = %d %s", step.patch, r.StatusCode, b)
		}
		for _, req := range []struct{ method, path, tok string }{
			{"GET", calendarPath, m.adamTok}, {"POST", calendarPath, m.adamTok},
			{"GET", feedURL(adamFeed), ""}, {"GET", upCoverPath + "?work=e3", m.adamTok},
		} {
			if r, b := m.do(t, req.method, req.path, req.tok, ""); r.StatusCode != http.StatusNotFound || codeOf(b) != step.code {
				t.Errorf("%s %s while off = %d %s, want 404 %s", req.method, req.path, r.StatusCode, b, step.code)
			}
		}
		if r, _ := m.do(t, "POST", calendarPath, key, ""); r.StatusCode != http.StatusForbidden {
			t.Errorf("API key mint while off = %d, want 403 first", r.StatusCode)
		}
		if r, b := m.do(t, "POST", calendarPath, demoTok, ""); r.StatusCode != http.StatusForbidden || codeOf(b) != "demo_account" {
			t.Errorf("demo mint while off = %d %s, want 403 demo_account first", r.StatusCode, b)
		}
	}
	// Turn off works while the feature is off, and the link stays dead after.
	if r, b := m.do(t, "DELETE", calendarPath, m.adamTok, ""); r.StatusCode != http.StatusNoContent {
		t.Fatalf("turn off while off = %d %s", r.StatusCode, b)
	}
	if r, _ := m.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"metadata":{"enabled":true}}`); r.StatusCode != http.StatusOK {
		t.Fatal("metadata back on")
	}
	if r, _ := m.do(t, "GET", feedURL(adamFeed), "", ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a link turned off while the feature was off = %d, want 401", r.StatusCode)
	}
}

// Security-critical: a feed token ends with the person's kid switch (and a kid
// can't mint), the account being disabled or deleted. A person who is a kid
// while somehow still holding a feed token is 403 kid_account.
func TestUpcomingFeedEndsWithTheAccount(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	ctx := context.Background()
	ellis, ellisTok := m.grownup(t, m.adamTok, "Ellis", "all")
	_, ellisFeed := mintFeed(t, m.testEnv, ellisTok)
	if r, _ := m.do(t, "GET", feedURL(ellisFeed), "", ""); r.StatusCode != http.StatusOK {
		t.Fatalf("the grown-up's feed = %d", r.StatusCode)
	}
	if s := m.status(t, "PATCH", memberURL(ellis.ID, ""), m.adamTok, `{"kind":"kid"}`); s != http.StatusOK {
		t.Fatalf("switch to a kid = %d", s)
	}
	if r, _ := m.do(t, "GET", feedURL(ellisFeed), "", ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the feed after the kid switch = %d, want 401", r.StatusCode)
	}
	if r, b := m.do(t, "POST", calendarPath, ellisTok, ""); r.StatusCode != http.StatusForbidden || codeOf(b) != "kid_account" {
		t.Fatalf("the kid's mint = %d %s", r.StatusCode, b)
	}

	// A feed token whose owner is a kid by a write that cleared nothing (a
	// direct change, as a future path might make): the route still refuses it.
	bea, beaTok := newUserWithSession(t, m.testEnv, "bea", false)
	_, beaFeed := mintFeed(t, m.testEnv, beaTok)
	if _, err := m.db.ExecContext(ctx, `UPDATE users SET managed_by = ?, member_kind = 'kid', member_access = 'all' WHERE id = ?`, m.adam, bea); err != nil {
		t.Fatal(err)
	}
	if r, b := m.do(t, "GET", feedURL(beaFeed), "", ""); r.StatusCode != http.StatusForbidden || codeOf(b) != "kid_account" {
		t.Fatalf("a kid's leftover feed token = %d %s, want 403 kid_account", r.StatusCode, b)
	}

	// Disabled (refused, and back once enabled), then deleted.
	cal, calTok := newUserWithSession(t, m.testEnv, "cal", false)
	_, calFeed := mintFeed(t, m.testEnv, calTok)
	calURL := "/api/v1/admin/users/" + strconv.FormatInt(cal, 10)
	if s := m.status(t, "PATCH", calURL, m.adminTok, `{"disabled":true}`); s != http.StatusOK {
		t.Fatalf("disable = %d", s)
	}
	if r, _ := m.do(t, "GET", feedURL(calFeed), "", ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a disabled account's feed = %d, want 401", r.StatusCode)
	}
	if s := m.status(t, "PATCH", calURL, m.adminTok, `{"disabled":false}`); s != http.StatusOK {
		t.Fatalf("enable = %d", s)
	}
	if r, _ := m.do(t, "GET", feedURL(calFeed), "", ""); r.StatusCode != http.StatusOK {
		t.Fatalf("the feed once enabled again = %d, want 200", r.StatusCode)
	}
	if s := m.status(t, "DELETE", calURL, m.adminTok, ""); s != http.StatusOK && s != http.StatusNoContent {
		t.Fatalf("delete = %d", s)
	}
	if r, _ := m.do(t, "GET", feedURL(calFeed), "", ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a deleted account's feed = %d, want 401", r.StatusCode)
	}
}

// The Upcoming cover: only a cover the caller's own list hands out right now.
func TestUpcomingCover(t *testing.T) {
	t.Parallel()
	u := newCalendarEnv(t)
	img := coverJPEG(t)
	var fetches atomic.Int32
	u.api.fetchCover = func(_ context.Context, raw string, _ int64) ([]byte, error) {
		fetches.Add(1)
		if raw == "https://c/broken.jpg" {
			return nil, errors.New("upstream down")
		}
		return img, nil
	}
	admin := adminSession(t, u.testEnv)
	_, key := u.mintAPIKey(t, admin, "script")
	cover := func(work, extra string) string { return upCoverPath + "?work=" + url.QueryEscape(work) + extra }

	// Allowed: the admin's own listed work, by header, by ?token=, with an API key.
	resp, body := u.do(t, "GET", cover("e3", "&size=160"), admin, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/jpeg" || resp.Header.Get("ETag") == "" {
		t.Fatalf("cover = %d %v %q", resp.StatusCode, resp.Header, body)
	}
	etag := resp.Header.Get("ETag")
	if r, _ := u.do(t, "GET", cover("e3", "&size=160&token="+url.QueryEscape(admin)), "", ""); r.StatusCode != http.StatusOK {
		t.Fatalf("cover by ?token= = %d", r.StatusCode)
	}
	if r, _ := u.do(t, "GET", cover("e3", "&token="+url.QueryEscape(key)), "", ""); r.StatusCode != http.StatusOK {
		t.Fatalf("cover with an API key = %d", r.StatusCode)
	}
	if r, _ := u.doHeaders(t, "GET", cover("e3", "&size=160"), admin, "", map[string]string{"If-None-Match": etag}); r.StatusCode != http.StatusNotModified {
		t.Fatalf("cover revalidated = %d, want 304", r.StatusCode)
	}

	// Denied: a listed work without a cover, a work not in the list at all, a
	// work the community lists but this person's series don't (o3, for a
	// reader with the Expanse only), and a work the caller holds.
	_, readerTok, _ := expanseReader(t, u)
	if r, _ := u.do(t, "GET", cover("o3", ""), admin, ""); r.StatusCode != http.StatusOK {
		t.Fatalf("the admin's o3 = %d", r.StatusCode)
	}
	if r, _ := u.do(t, "GET", cover("e3", ""), readerTok, ""); r.StatusCode != http.StatusOK {
		t.Fatalf("the reader's e3 = %d", r.StatusCode)
	}
	before := fetches.Load()
	for _, d := range []struct{ work, tok string }{{"e4", admin}, {"nope", admin}, {"o3", readerTok}, {"o1", admin}} {
		if r, b := u.do(t, "GET", cover(d.work, ""), d.tok, ""); r.StatusCode != http.StatusNotFound || codeOf(b) != "not_found" {
			t.Errorf("cover %s = %d %s, want 404 not_found", d.work, r.StatusCode, b)
		}
	}
	if fetches.Load() != before {
		t.Error("a refused cover was fetched")
	}
	// Held: a copy of e3 elsewhere drops it from the admin's list, and its cover.
	holdCopy(t, u, "Loose/Abaddon", "a:B3", "e3")
	if r, _ := u.do(t, "GET", cover("e3", ""), admin, ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("a held work's cover = %d, want 404", r.StatusCode)
	}

	// Bad input: no work, a size outside the thumbnail sizes.
	if r, _ := u.do(t, "GET", upCoverPath, admin, ""); r.StatusCode != http.StatusBadRequest {
		t.Errorf("no work = %d, want 400", r.StatusCode)
	}
	if r, _ := u.do(t, "GET", cover("o3", "&size=123"), admin, ""); r.StatusCode != http.StatusBadRequest {
		t.Errorf("bad size = %d, want 400", r.StatusCode)
	}
	// Other kinds: a cast, bridge or pairing token, or no token, is a 401.
	for name, tok := range otherKindTokens(t, u) {
		if name == "session" || name == "api key" {
			continue
		}
		if r, _ := u.do(t, "GET", cover("o3", "&token="+url.QueryEscape(tok)), "", ""); r.StatusCode != http.StatusUnauthorized {
			t.Errorf("cover with a %s as ?token= = %d, want 401", name, r.StatusCode)
		}
	}
	if r, _ := u.do(t, "GET", cover("o3", ""), "", ""); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous cover = %d, want 401", r.StatusCode)
	}

	// The fetch fails: 502 cover_unavailable.
	u.series(t, u.lib.ID, "Other Series", "other", []catalog.MetaSeriesMember{
		{WorkID: "o4", Position: "4", Title: "Four", ReleaseDate: "2099-06-06", CoverURL: "https://c/broken.jpg"},
	}, []catalog.UpcomingItem{{WorkID: "o4", Kind: catalog.UpcomingPreorder, ReleaseDate: "2099-06-06", Position: "4"}})
	if r, b := u.do(t, "GET", cover("o4", ""), admin, ""); r.StatusCode != http.StatusBadGateway || codeOf(b) != "cover_unavailable" {
		t.Errorf("a broken cover = %d %s, want 502 cover_unavailable", r.StatusCode, b)
	}
}
