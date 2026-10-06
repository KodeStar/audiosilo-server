package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
)

// cradleBook and mistbornBook (the fixture library's two books) live in ratings_test.go.

// statsEnv is the activity env with the library scanned and the member granted
// only the "Will Wight" folder.
type statsEnv struct {
	*activityEnv
	shareID int64
}

func newStatsEnv(t *testing.T) *statsEnv {
	t.Helper()
	e := newActivityEnv(t)
	ctx := context.Background()
	lib, err := e.cat.GetLibrary(ctx, e.libID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := library.NewScanner(e.cat, "", slog.Default()).Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	share, err := e.cat.CreateShare(ctx, catalog.Share{Name: "Wight",
		Paths: []catalog.PathRule{{LibraryID: e.libID, Path: "Will Wight"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.cat.GrantShare(ctx, e.memberID, share.ID); err != nil {
		t.Fatal(err)
	}
	return &statsEnv{activityEnv: e, shareID: share.ID}
}

// save sends a progress save on path from token.
func (e *statsEnv) save(t *testing.T, token, path string, position float64, finished bool) {
	t.Helper()
	body := `{"position":` + strconv.FormatFloat(position, 'f', -1, 64) + `,"duration":120,"finished":` +
		strconv.FormatBool(finished) + `}`
	if resp, b := e.do(t, "PUT", e.libPath()+"/progress?path="+url.QueryEscape(path), token, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("progress save = %d %s", resp.StatusCode, b)
	}
}

// listenTo plays path from token: saves that record listening, then a finish.
func (e *statsEnv) listenTo(t *testing.T, token, path string) {
	t.Helper()
	e.save(t, token, path, 10, false)
	e.save(t, token, path, 20, false)
	e.save(t, token, path, 120, true)
}

// getJSON GETs path as token, wanting 200, and decodes the body generically.
func (e *statsEnv) getJSON(t *testing.T, path, token string) (map[string]any, string) {
	t.Helper()
	resp, body := e.do(t, "GET", path, token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, resp.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("GET %s: %v %s", path, err, body)
	}
	return out, body
}

// crossUserKeys are the fields that would carry other people; none may appear
// anywhere in a person's own stats.
var crossUserKeys = []string{"by_user", "top_users", "listeners", "user_id", "username", "inactive_users",
	"peak_concurrent", "funnel", "drop_offs", "storage", "coverage", "growth"}

// assertNoCrossUserKeys walks a decoded JSON value for any crossUserKeys key.
func assertNoCrossUserKeys(t *testing.T, v any, where string) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			for _, bad := range crossUserKeys {
				if k == bad {
					t.Fatalf("%s carries %q", where, k)
				}
			}
			assertNoCrossUserKeys(t, child, where+"."+k)
		}
	case []any:
		for _, child := range x {
			assertNoCrossUserKeys(t, child, where+"[]")
		}
	}
}

type myStatsBody struct {
	Stats catalog.UserStats `json:"stats"`
}

func (e *statsEnv) myStats(t *testing.T, token string) (catalog.UserStats, string) {
	t.Helper()
	raw, body := e.getJSON(t, "/api/v1/me/stats?range=7d", token)
	assertNoCrossUserKeys(t, raw, "stats")
	var out myStatsBody
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return out.Stats, body
}

func TestMyStats(t *testing.T) {
	e := newStatsEnv(t)
	e.listenTo(t, e.memberTok, cradleBook)

	before, body := e.myStats(t, e.memberTok)
	if before.Range != "7d" || len(before.Days) < 7 || before.Totals.Listened <= 0 || before.Totals.Sessions != 1 ||
		before.Totals.Books != 1 || before.Totals.Finished != 1 {
		t.Fatalf("member stats = %s", body)
	}
	if len(before.TopBooks) != 1 || before.TopBooks[0].Path != cradleBook || len(before.FinishedBooks) != 1 ||
		before.FinishedBooks[0].Path != cradleBook || len(before.TopAuthors) != 1 || before.TopAuthors[0].Name != "Will Wight" {
		t.Fatalf("member's book rows = %s", body)
	}
	var days float64
	for _, d := range before.Days {
		days += d.Listened
	}
	if days != before.Totals.Listened {
		t.Fatalf("days sum to %v, totals say %v", days, before.Totals.Listened)
	}

	// Another person's listening, on the same book and on another, never shows.
	e.listenTo(t, e.playerTok, cradleBook)
	e.listenTo(t, e.playerTok, mistbornBook)
	after, body := e.myStats(t, e.memberTok)
	if after.Totals != before.Totals || after.Previous != before.Previous || len(after.TopBooks) != 1 ||
		len(after.FinishedBooks) != 1 || len(after.Clients) != len(before.Clients) {
		t.Fatalf("the admin's listening reached the member's stats: %s", body)
	}
	if strings.Contains(body, "Sanderson") || strings.Contains(body, "Final Empire") || strings.Contains(body, "Mistborn") {
		t.Fatalf("another person's book in the member's stats: %s", body)
	}
	// The admin's own stats have both books (an admin sees every library).
	admin, body := e.myStats(t, e.playerTok)
	if admin.Totals.Books != 2 || admin.Totals.Finished != 2 || len(admin.TopBooks) != 2 || len(admin.FinishedBooks) != 2 {
		t.Fatalf("admin's own stats = %s", body)
	}

	// The share is revoked: the book leaves the rows naming it, its time stays.
	if err := e.cat.RevokeShare(context.Background(), e.memberID, e.shareID); err != nil {
		t.Fatal(err)
	}
	revoked, body := e.myStats(t, e.memberTok)
	if revoked.Totals != before.Totals || len(revoked.TopBooks) != 0 || len(revoked.FinishedBooks) != 0 ||
		len(revoked.TopAuthors) != 0 || len(revoked.TopNarrators) != 0 || len(revoked.TopSeries) != 0 {
		t.Fatalf("after the share was revoked = %s", body)
	}
	for _, leak := range []string{"Will Wight", "Cradle", "Unsouled"} {
		if strings.Contains(body, leak) {
			t.Fatalf("%q echoed back after the share was revoked: %s", leak, body)
		}
	}
}

// TestMyStatsEmptyShape: with no listening, every list is [] (never null) and the
// envelope has exactly the contract's keys.
func TestMyStatsEmptyShape(t *testing.T) {
	e := newStatsEnv(t)
	raw, body := e.getJSON(t, "/api/v1/me/stats", e.memberTok) // range defaults to 30d
	stats, _ := raw["stats"].(map[string]any)
	want := []string{"range", "from", "to", "timezone", "utc_offset", "totals", "previous", "estimated", "days",
		"hour_weekday", "top_books", "top_authors", "top_narrators", "top_series", "finished_books", "playback", "clients"}
	if len(stats) != len(want) || stats["range"] != "30d" {
		t.Fatalf("stats keys = %d, want %d: %s", len(stats), len(want), body)
	}
	for _, k := range want {
		if _, ok := stats[k]; !ok {
			t.Fatalf("stats lacks %q: %s", k, body)
		}
	}
	for _, k := range []string{"top_books", "top_authors", "top_narrators", "top_series", "finished_books", "playback", "clients"} {
		if l, ok := stats[k].([]any); !ok || len(l) != 0 {
			t.Fatalf("%s = %v, want []", k, stats[k])
		}
	}
	totals, _ := stats["totals"].(map[string]any)
	if len(totals) != 4 {
		t.Fatalf("totals = %v, want listened/sessions/books/finished", totals)
	}
}

func TestMyListening(t *testing.T) {
	e := newStatsEnv(t)
	e.listenTo(t, e.playerTok, cradleBook) // the admin listens; the member doesn't

	raw, body := e.getJSON(t, "/api/v1/me/listening?range=7d", e.memberTok)
	assertNoCrossUserKeys(t, raw, "listening")
	var out catalog.UserListening
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Range != "7d" || len(out.Days) < 7 || out.From == "" {
		t.Fatalf("member listening = %s", body)
	}
	for _, d := range out.Days {
		if d.Listened != 0 {
			t.Fatalf("the admin's listening is in the member's days: %s", body)
		}
	}
	for _, d := range raw["days"].([]any) {
		if len(d.(map[string]any)) != 2 {
			t.Fatalf("a day carries more than date and listened: %v", d)
		}
	}
	_, body = e.getJSON(t, "/api/v1/me/listening?range=7d", e.playerTok)
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, d := range out.Days {
		sum += d.Listened
	}
	if sum <= 0 {
		t.Fatalf("the admin's own listening is missing: %s", body)
	}
}

func TestMyStatsErrors(t *testing.T) {
	e := newStatsEnv(t)
	for _, p := range []string{"/api/v1/me/stats?range=3w", "/api/v1/me/listening?range=3w", "/api/v1/me/stats?range=1999"} {
		resp, body := e.do(t, "GET", p, e.memberTok, "")
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, `"code":"invalid_range"`) {
			t.Errorf("%s = %d %s, want 400 invalid_range", p, resp.StatusCode, body)
		}
	}
	// Denied: every route needs a signed-in caller.
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/v1/me/stats", ""}, {"GET", "/api/v1/me/listening", ""}, {"GET", "/api/v1/me/goal", ""},
		{"PUT", "/api/v1/me/goal", `{"books_per_year":5}`}, {"DELETE", "/api/v1/me/goal", ""},
	} {
		if resp, _ := e.do(t, c.method, c.path, "", c.body); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", c.method, c.path, resp.StatusCode)
		}
	}
	if _, si := e.do(t, "GET", "/api/v1/server", "", ""); !strings.Contains(si, `"user_stats":true`) {
		t.Fatalf("/server missing user_stats: %s", si)
	}
}

func TestMyGoal(t *testing.T) {
	e := newStatsEnv(t)
	year := strconv.Itoa(time.Now().Year())
	var g catalog.GoalStatus
	resp, body := e.do(t, "GET", "/api/v1/me/goal", e.memberTok, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"goal":null`) ||
		json.Unmarshal([]byte(body), &g) != nil || g.Year != year || g.Finished != 0 {
		t.Fatalf("no goal = %d %s", resp.StatusCode, body)
	}

	resp, body = e.do(t, "PUT", "/api/v1/me/goal", e.memberTok, `{"books_per_year":12}`)
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &g) != nil || g.Goal == nil ||
		g.Goal.BooksPerYear != 12 || g.Goal.UpdatedAt == "" || g.Year != year {
		t.Fatalf("set goal = %d %s", resp.StatusCode, body)
	}
	for _, bad := range []string{`{"books_per_year":0}`, `{"books_per_year":1001}`, `{"books_per_year":-3}`,
		`{"books_per_year":2.5}`, `{"books_per_year":"12"}`, `{}`, `{"books_per_year":12,"user_id":1}`, `nope`} {
		if resp, body := e.do(t, "PUT", "/api/v1/me/goal", e.memberTok, bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("PUT %s = %d %s, want 400", bad, resp.StatusCode, body)
		}
	}
	if resp, body := e.do(t, "PUT", "/api/v1/me/goal", e.memberTok, `{"books_per_year":1000}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("goal 1000 = %d %s", resp.StatusCode, body)
	}

	// The finishes counted are the caller's, this year.
	e.listenTo(t, e.memberTok, cradleBook)
	e.listenTo(t, e.playerTok, cradleBook)
	e.listenTo(t, e.playerTok, mistbornBook)
	_, body = e.getJSON(t, "/api/v1/me/goal", e.memberTok)
	if json.Unmarshal([]byte(body), &g) != nil || g.Goal == nil || g.Goal.BooksPerYear != 1000 || g.Finished != 1 {
		t.Fatalf("member's goal = %s", body)
	}
	// Isolation: the admin sees their own (no goal, two finishes), not the member's.
	_, body = e.getJSON(t, "/api/v1/me/goal", e.playerTok)
	if g = (catalog.GoalStatus{}); json.Unmarshal([]byte(body), &g) != nil || g.Goal != nil || g.Finished != 2 {
		t.Fatalf("admin's goal = %s", body)
	}

	for range 2 { // idempotent
		if resp, body := e.do(t, "DELETE", "/api/v1/me/goal", e.memberTok, ""); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("delete goal = %d %s", resp.StatusCode, body)
		}
	}
	if _, body := e.getJSON(t, "/api/v1/me/goal", e.memberTok); !strings.Contains(body, `"goal":null`) {
		t.Fatalf("after delete = %s", body)
	}

	// Deleting the user purges their goal.
	if resp, body := e.do(t, "PUT", "/api/v1/me/goal", e.memberTok, `{"books_per_year":3}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("set goal = %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(t, "DELETE", "/api/v1/admin/users/"+strconv.FormatInt(e.memberID, 10), e.console, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete member = %d %s", resp.StatusCode, body)
	}
	left, err := e.cat.GoalStatusFor(context.Background(), e.memberID, time.Now(), time.Local)
	if err != nil || left.Goal != nil {
		t.Fatalf("a deleted user's goal survived: %+v, %v", left, err)
	}
}
