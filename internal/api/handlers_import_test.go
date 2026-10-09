package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/importer"
	"github.com/kodestar/audiosilo-server/internal/importer/abstest"
)

// syncBuffer is a log sink safe for the background fetch to write to.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// importEnv is a test env with a fake ABS (the recorded alex and jo), the
// AudioSilo user "alex" who can see the Books library but not Kids, and the
// API logging to a buffer.
type importEnv struct {
	*testEnv
	abs       *abstest.Server
	adminTok  string
	memberTok string
	alexID    int64
	alexTok   string
	logs      *syncBuffer
	bodies    []string // every response body, for the token check
}

func newImportEnv(t *testing.T) *importEnv {
	t.Helper()
	e := &importEnv{testEnv: newTestEnv(t), abs: abstest.New(t), logs: &syncBuffer{}}
	log := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.api.log = log
	e.api.imports = importer.New(e.cat, authUsers{e.auth}, time.Local, e.api.SessionRetention, log)
	// The recorded history is from 2026: keep it all raw for as long as the
	// retention allows, so these tests see sessions next year too.
	e.setSessionDays(config.MaxSessionDays)
	e.adminTok, e.memberTok, _ = adminAndMember(t, e.testEnv)
	ctx := context.Background()
	alex, err := e.auth.CreateUser(ctx, "Alex", "alex-password", auth.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	e.alexID = alex.ID
	if e.alexTok, err = e.auth.IssueToken(ctx, alex.ID, auth.KindSession, "t", 0); err != nil {
		t.Fatal(err)
	}
	books, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Books", Root: t.TempDir()})
	kids, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Kids", Root: t.TempDir()})
	for _, b := range []catalog.Book{
		{LibraryID: books.ID, RelPath: "Brandon Sanderson/The Stormlight Archive/01 - The Way of Kings", Title: "The Way of Kings", Author: "Brandon Sanderson", Duration: 10800},
		{LibraryID: books.ID, RelPath: "Andy Weir/Project Hail Mary", Title: "Project Hail Mary", Author: "Andy Weir", Duration: 8400},
		{LibraryID: books.ID, RelPath: "James S. A. Corey/The Expanse/01 - Leviathan Wakes", Title: "Leviathan Wakes", Author: "James S. A. Corey", Duration: 9600},
		{LibraryID: books.ID, RelPath: "Jim Butcher/The Dresden Files/01 - Storm Front", Title: "Storm Front", Author: "Jim Butcher", Duration: 3900},
		{LibraryID: books.ID, RelPath: "Mary Shelley/Frankenstein", Title: "Frankenstein", Author: "Mary Shelley", Duration: 4501},
		{LibraryID: books.ID, RelPath: "Émile Zola/Germinal", Title: "Germinal", Author: "Émile Zola", Duration: 6000},
		{LibraryID: kids.ID, RelPath: "Beatrix Potter/The Tale of Peter Rabbit", Title: "The Tale of Peter Rabbit", Author: "Beatrix Potter", Duration: 480},
	} {
		b.IsFolder, b.AddedAt = true, "2026-01-01T00:00:00Z"
		if _, err := e.cat.UpsertBook(ctx, &b); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.cat.GrantWholeLibrary(ctx, alex.ID, books.ID); err != nil {
		t.Fatal(err)
	}
	return e
}

// setSessionDays changes the session retention setting, as a settings save does.
func (e *importEnv) setSessionDays(days int) {
	c := *e.api.config().Config
	c.Activity.SessionDays = days
	e.api.live.Store(newLiveConfig(&c, &c))
}

// call sends a request and decodes a JSON answer into out (when not nil).
func (e *importEnv) call(t *testing.T, method, path, token, body string, want int, out any) {
	t.Helper()
	resp, b := e.do(t, method, "/api/v1"+path, token, body)
	e.bodies = append(e.bodies, b)
	if resp.StatusCode != want {
		t.Fatalf("%s %s = %d %s, want %d", method, path, resp.StatusCode, b, want)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(b), out); err != nil {
			t.Fatalf("%s %s: %v in %s", method, path, err, b)
		}
	}
}

// TestImportFromABS walks the admin flow end to end against the recorded ABS:
// list its users, start, poll, review, change the cutoff, apply, undo, delete.
func TestImportFromABS(t *testing.T) {
	t.Parallel()
	e := newImportEnv(t)
	var users struct {
		Version string             `json:"version"`
		Users   []importer.AbsUser `json:"users"`
	}
	e.call(t, "POST", "/admin/imports/abs/users", e.adminTok,
		fmt.Sprintf(`{"url":%q,"token":%q}`, e.abs.URL+"/", abstest.AdminToken), 200, &users)
	if users.Version != "2.37.1" || len(users.Users) != 3 {
		t.Fatalf("users = %+v", users)
	}
	for _, u := range users.Users {
		want := u.Username == "alex" // AudioSilo's "Alex", any case
		if (u.SuggestedUserID != nil) != want || want && *u.SuggestedUserID != e.alexID {
			t.Errorf("%s suggested %v", u.Username, u.SuggestedUserID)
		}
	}

	var started struct {
		Imports []catalog.Import `json:"imports"`
	}
	// abs_username names the source while it is fetched (the fetch then reads ABS's).
	e.call(t, "POST", "/admin/imports/abs", e.adminTok, fmt.Sprintf(`{"url":%q,"token":%q,"users":[{"abs_user_id":%q,"abs_username":"Alex (listed)","user_id":%d}]}`,
		e.abs.URL, abstest.AdminToken, abstest.AlexID, e.alexID), 202, &started)
	if len(started.Imports) != 1 || started.Imports[0].Status != catalog.ImportFetching ||
		started.Imports[0].SourceURL != e.abs.URL || started.Imports[0].Username != "Alex" ||
		started.Imports[0].SourceUser != "Alex (listed)" {
		t.Fatalf("started = %+v", started)
	}
	id := started.Imports[0].ID
	e.api.imports.Wait()

	var d catalog.ImportDetail
	e.call(t, "GET", fmt.Sprintf("/admin/imports/%d", id), e.adminTok, "", 200, &d)
	s := d.Summary
	if d.Status != catalog.ImportReview || d.SourceUser != "alex" || d.Cutoff != nil || s == nil {
		t.Fatalf("review = %+v", d)
	}
	// Six books by path; Peter Rabbit is in Kids (no access), two exist only in
	// ABS and one was deleted there.
	if s.Items != 10 || s.Matched.Path != 6 || s.Unmatched != 4 || s.Finished != 3 || s.Bookmarks != 3 ||
		s.Sessions != 32 || s.Listened <= 0 {
		t.Fatalf("summary = %+v", *s)
	}
	reasons := map[string]int{}
	for _, u := range d.UnmatchedItems {
		reasons[u.Reason]++
	}
	if reasons[catalog.ReasonNoAccess] != 1 || reasons[catalog.ReasonNoMatch] != 3 {
		t.Errorf("unmatched = %+v", d.UnmatchedItems)
	}

	// A cutoff leaves out what started on or after it; null takes it away again.
	e.call(t, "PATCH", fmt.Sprintf("/admin/imports/%d", id), e.adminTok, `{"cutoff":"2026-09-15"}`, 200, &d)
	if d.Cutoff == nil || d.Summary.SkippedAfterCutoff == 0 || d.Summary.Sessions+d.Summary.SkippedAfterCutoff != 32 {
		t.Fatalf("with a cutoff = %+v %+v", d.Cutoff, *d.Summary)
	}
	e.call(t, "PATCH", fmt.Sprintf("/admin/imports/%d", id), e.adminTok, `{"cutoff":null}`, 200, &d)
	if d.Cutoff != nil || d.Summary.Sessions != 32 {
		t.Fatalf("without = %+v %+v", d.Cutoff, *d.Summary)
	}

	e.checkNoToken(t, abstest.AdminToken) // the stored payload included

	var imp catalog.Import
	e.call(t, "POST", fmt.Sprintf("/admin/imports/%d/apply", id), e.adminTok, "", 200, &imp)
	if imp.Status != catalog.ImportApplied || imp.Summary.Sessions != 32 {
		t.Fatalf("applied = %+v", imp)
	}
	e.call(t, "POST", fmt.Sprintf("/admin/imports/%d/apply", id), e.adminTok, "", 409, nil)
	e.call(t, "DELETE", fmt.Sprintf("/admin/imports/%d", id), e.adminTok, "", 409, nil)

	// Alex's own stats now show the listening and the three finished books.
	var stats struct {
		Stats catalog.UserStats `json:"stats"`
	}
	e.call(t, "GET", "/me/stats?range=2026", e.alexTok, "", 200, &stats)
	if stats.Stats.Totals.Listened <= 0 || stats.Stats.Totals.Finished != 3 || len(stats.Stats.TopBooks) == 0 {
		t.Errorf("alex's stats = %+v", stats.Stats.Totals)
	}

	// The admin's session list pages through the imported sessions, newest
	// start first.
	var all []catalog.Session
	cursor := ""
	for range 20 {
		var page struct {
			Sessions   []catalog.Session `json:"sessions"`
			NextBefore *int64            `json:"next_before"`
		}
		e.call(t, "GET", fmt.Sprintf("/admin/sessions?user_id=%d&limit=10%s", e.alexID, cursor), e.adminTok, "", 200, &page)
		all = append(all, page.Sessions...)
		if page.NextBefore == nil {
			break
		}
		// The console's cursor: the last session's id and start (before_at).
		cursor = fmt.Sprintf("&before=%d&before_at=%s", *page.NextBefore, url.QueryEscape(all[len(all)-1].StartedAt))
	}
	if len(all) != 32 || !all[0].Imported || all[0].Client == nil || all[0].Client.App != "Audiobookshelf" ||
		!slices.IsSortedFunc(all, func(a, b catalog.Session) int { return strings.Compare(b.StartedAt, a.StartedAt) }) {
		t.Errorf("paged %d sessions, first %+v", len(all), all[0])
	}

	var list struct {
		Imports []catalog.Import `json:"imports"`
	}
	e.call(t, "GET", fmt.Sprintf("/admin/imports?user_id=%d", e.alexID), e.adminTok, "", 200, &list)
	if len(list.Imports) != 1 || list.Imports[0].ID != id {
		t.Errorf("list = %+v", list)
	}

	e.call(t, "POST", fmt.Sprintf("/admin/imports/%d/undo", id), e.adminTok, "", 200, &imp)
	if imp.Status != catalog.ImportUndone {
		t.Fatalf("undone = %+v", imp)
	}
	e.call(t, "GET", "/me/stats?range=2026", e.alexTok, "", 200, &stats)
	if stats.Stats.Totals.Listened != 0 || stats.Stats.Totals.Finished != 0 {
		t.Errorf("stats after undo = %+v", stats.Stats.Totals)
	}
	e.call(t, "POST", fmt.Sprintf("/admin/imports/%d/undo", id), e.adminTok, "", 409, nil)
	e.call(t, "DELETE", fmt.Sprintf("/admin/imports/%d", id), e.adminTok, "", 204, nil)
	e.call(t, "GET", fmt.Sprintf("/admin/imports/%d", id), e.adminTok, "", 404, nil)

	e.checkNoToken(t, abstest.AdminToken)
}

// startAlex starts an import of the recorded alex into Alex and waits for its
// review.
func (e *importEnv) startAlex(t *testing.T) catalog.ImportDetail {
	t.Helper()
	var started struct {
		Imports []catalog.Import `json:"imports"`
	}
	e.call(t, "POST", "/admin/imports/abs", e.adminTok, fmt.Sprintf(`{"url":%q,"token":%q,"users":[{"abs_user_id":%q,"user_id":%d}]}`,
		e.abs.URL, abstest.AdminToken, abstest.AlexID, e.alexID), 202, &started)
	e.api.imports.Wait()
	var d catalog.ImportDetail
	e.call(t, "GET", fmt.Sprintf("/admin/imports/%d", started.Imports[0].ID), e.adminTok, "", 200, &d)
	if d.Status != catalog.ImportReview || d.Summary == nil {
		t.Fatalf("review = %+v", d.Import)
	}
	return d
}

// TestReimportFromABS: a re-import replaces the first and behaves like a first
// import: the progress the first moved on is moved on again (the review promised
// it, and the apply writes what the review showed), and the imported sessions
// show in the player's history (the book's, and /me/history).
func TestReimportFromABS(t *testing.T) {
	t.Parallel()
	e := newImportEnv(t)
	ctx := context.Background()
	books, err := e.cat.ImportBooks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var phm catalog.Ref
	for _, b := range books {
		if b.RelPath == "Andy Weir/Project Hail Mary" {
			phm = catalog.Ref{LibraryID: b.LibraryID, Path: b.RelPath}
		}
	}
	// Alex listened a little here first, before ABS's last update (5520 s, 26 September).
	if _, err := e.cat.SaveProgress(ctx, e.alexID, catalog.Progress{Ref: phm, Position: 600, Duration: 8400,
		UpdatedAt: "2026-09-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}

	var firstPos float64
	for round := range 2 {
		d := e.startAlex(t)
		var imp catalog.Import
		e.call(t, "POST", fmt.Sprintf("/admin/imports/%d/apply", d.ID), e.adminTok, "", 200, &imp)
		if !reflect.DeepEqual(*imp.Summary, *d.Summary) {
			t.Errorf("round %d: applied %+v, reviewed %+v", round, *imp.Summary, *d.Summary)
		}
		p, err := e.cat.GetProgress(ctx, e.alexID, phm)
		if err != nil || p == nil || p.Position != 5520 {
			t.Fatalf("round %d: progress = %+v, %v", round, p, err)
		}
		if round == 0 {
			firstPos = p.Position
		} else if p.Position != firstPos {
			t.Errorf("the re-import left %v, the first import %v", p.Position, firstPos)
		}
	}

	var page struct {
		History []catalog.HistoryEntry `json:"history"`
	}
	e.call(t, "GET", "/me/history?limit=100", e.alexTok, "", 200, &page)
	if len(page.History) != 32 || page.History[0].Book == nil {
		t.Errorf("%d spans in /me/history, want the 32 imported sessions once", len(page.History))
	}
	var book struct {
		History []catalog.History `json:"history"`
	}
	e.call(t, "GET", fmt.Sprintf("/libraries/%d/history?path=%s", phm.LibraryID, url.QueryEscape(phm.Path)),
		e.alexTok, "", 200, &book)
	if len(book.History) == 0 || book.History[0].To <= book.History[0].From {
		t.Errorf("book history = %+v", book.History)
	}
}

// TestImportRollsUpPastRetention: an apply writes the imported sessions already
// past the session retention, as the setting is when it applies, as day totals,
// the rest as sessions: the person's stats are the same as with every session
// raw, the summary still counts every imported session, and the history has a
// span for each.
func TestImportRollsUpPastRetention(t *testing.T) {
	t.Parallel()
	e := newImportEnv(t)
	ctx := context.Background()
	var stats [2]struct {
		Stats catalog.UserStats `json:"stats"`
	}
	var raw [2]int
	for round, days := range []int{config.MaxSessionDays, config.MinSessionDays} {
		d := e.startAlex(t) // the second replaces the first
		e.setSessionDays(days)
		var imp catalog.Import
		e.call(t, "POST", fmt.Sprintf("/admin/imports/%d/apply", d.ID), e.adminTok, "", 200, &imp)
		if imp.Summary.Sessions != 32 {
			t.Fatalf("round %d: applied %+v", round, *imp.Summary)
		}
		e.call(t, "GET", "/me/stats?range=2026", e.alexTok, "", 200, &stats[round])
		sessions, _, err := e.cat.ListSessions(ctx, catalog.SessionFilter{UserID: e.alexID, Limit: 200})
		if err != nil {
			t.Fatal(err)
		}
		raw[round] = len(sessions)
		cutoff := catalog.FormatStamp(time.Now().Add(-time.Duration(days) * 24 * time.Hour))
		for _, s := range sessions {
			if s.LastAt < cutoff {
				t.Errorf("round %d: a session past the retention stayed raw: %+v", round, s)
			}
		}
		var page struct {
			History []catalog.HistoryEntry `json:"history"`
		}
		e.call(t, "GET", "/me/history?limit=100", e.alexTok, "", 200, &page)
		if len(page.History) != 32 {
			t.Errorf("round %d: %d history spans, want 32", round, len(page.History))
		}
	}
	// The recorded August is always past the shorter retention.
	if raw[0] != 32 || raw[1] >= 32 {
		t.Errorf("raw sessions: %d all kept, %d with the short retention", raw[0], raw[1])
	}
	all, short := stats[0].Stats, stats[1].Stats
	if all.Totals != short.Totals || all.Estimated != short.Estimated || !slices.Equal(all.Days, short.Days) {
		t.Errorf("stats with the short retention = %+v, all raw %+v", short.Totals, all.Totals)
	}
}

// checkNoToken: the token is nowhere: not in an imports row (the payload
// unzipped), a response or the log.
func (e *importEnv) checkNoToken(t *testing.T, token string) {
	t.Helper()
	ctx := context.Background()
	imps, err := e.cat.ListImports(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range imps {
		raw, _ := json.Marshal(imp)
		payload, err := e.cat.ImportPayload(ctx, imp.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(payload) > 0 {
			zr, err := gzip.NewReader(bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(zr)
			raw = append(raw, b...)
		}
		if strings.Contains(string(raw), token) {
			t.Errorf("import %d stores the token", imp.ID)
		}
	}
	for _, b := range e.bodies {
		if strings.Contains(b, token) {
			t.Errorf("a response carries the token: %s", b)
		}
	}
	if strings.Contains(e.logs.String(), token) {
		t.Errorf("the log carries the token:\n%s", e.logs.String())
	}
}

// TestImportFailures: what an admin gets for a bad address, a server that
// isn't ABS, a refused token (synchronously from the users list, and as a
// failed import from a start), and a malformed start; the token never shows.
func TestImportFailures(t *testing.T) {
	t.Parallel()
	e := newImportEnv(t)
	const bad = "not-the-right-token-1234"
	e.call(t, "POST", "/admin/imports/abs/users", e.adminTok, `{"url":"ftp://abs","token":"x"}`, 400, nil)
	var errBody map[string]string
	// A token that can't be one is refused before ABS is asked anything.
	for _, tok := range []string{"", "  ", "a\r\nX-Injected: 1", "a\u0000b", strings.Repeat("t", 8193)} {
		body, _ := json.Marshal(map[string]string{"url": e.abs.URL, "token": tok})
		e.call(t, "POST", "/admin/imports/abs/users", e.adminTok, string(body), 400, &errBody)
		if errBody["code"] != "invalid_import" {
			t.Errorf("token %q = %v", tok, errBody)
		}
	}
	if n := e.abs.Requests.Load(); n != 0 {
		t.Errorf("a malformed token reached ABS %d times", n)
	}
	e.call(t, "POST", "/admin/imports/abs/users", e.adminTok,
		fmt.Sprintf(`{"url":%q,"token":%q}`, e.abs.URL, bad), 502, &errBody)
	if errBody["code"] != importer.CodeUnauthorized {
		t.Errorf("bad token = %v", errBody)
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"app":"something-else"}`))
	}))
	defer other.Close()
	e.call(t, "POST", "/admin/imports/abs/users", e.adminTok,
		fmt.Sprintf(`{"url":%q,"token":"x"}`, other.URL), 502, &errBody)
	if errBody["code"] != importer.CodeNotABS {
		t.Errorf("not ABS = %v", errBody)
	}

	for _, body := range []string{
		fmt.Sprintf(`{"url":%q,"token":"x","users":[]}`, e.abs.URL),
		fmt.Sprintf(`{"url":%q,"token":"x","users":[{"abs_user_id":"a","user_id":99999}]}`, e.abs.URL),
		fmt.Sprintf(`{"url":%q,"token":"x","users":[{"abs_user_id":"a","user_id":%d},{"abs_user_id":"b","user_id":%d}]}`, e.abs.URL, e.alexID, e.alexID),
		fmt.Sprintf(`{"url":%q,"token":"x","users":[{"abs_user_id":"a","user_id":%d}],"cutoff":"yesterday"}`, e.abs.URL, e.alexID),
	} {
		e.call(t, "POST", "/admin/imports/abs", e.adminTok, body, 400, &errBody)
		if errBody["code"] != "invalid_import" {
			t.Errorf("%s = %v", body, errBody)
		}
	}

	var started struct {
		Imports []catalog.Import `json:"imports"`
	}
	e.call(t, "POST", "/admin/imports/abs", e.adminTok, fmt.Sprintf(`{"url":%q,"token":%q,"users":[{"abs_user_id":%q,"user_id":%d}],"cutoff":"auto"}`,
		e.abs.URL, bad, abstest.JoID, e.alexID), 202, &started)
	e.api.imports.Wait()
	var d catalog.ImportDetail
	e.call(t, "GET", fmt.Sprintf("/admin/imports/%d", started.Imports[0].ID), e.adminTok, "", 200, &d)
	if d.Status != catalog.ImportFailed || d.ErrorCode != importer.CodeUnauthorized || d.Error == "" || d.Summary != nil {
		t.Errorf("failed import = %+v", d.Import)
	}
	e.call(t, "PATCH", fmt.Sprintf("/admin/imports/%d", d.ID), e.adminTok, `{"cutoff":null}`, 409, nil)
	e.call(t, "POST", fmt.Sprintf("/admin/imports/%d/apply", d.ID), e.adminTok, "", 409, nil)
	if !strings.Contains(e.logs.String(), "abs_unauthorized") {
		t.Errorf("the failure wasn't logged:\n%s", e.logs.String())
	}
	e.checkNoToken(t, bad)
}

// TestImportRoutesAreAdminOnly: every import route refuses a non-admin.
func TestImportRoutesAreAdminOnly(t *testing.T) {
	t.Parallel()
	e := newImportEnv(t)
	for _, r := range []struct{ method, path, body string }{
		{"POST", "/admin/imports/abs/users", fmt.Sprintf(`{"url":%q,"token":%q}`, e.abs.URL, abstest.AdminToken)},
		{"POST", "/admin/imports/abs", fmt.Sprintf(`{"url":%q,"token":%q,"users":[{"abs_user_id":%q,"user_id":%d}]}`,
			e.abs.URL, abstest.AdminToken, abstest.AlexID, e.alexID)},
		{"GET", "/admin/imports", ""},
		{"GET", "/admin/imports/1", ""},
		{"PATCH", "/admin/imports/1", `{"cutoff":null}`},
		{"POST", "/admin/imports/1/apply", ""},
		{"POST", "/admin/imports/1/undo", ""},
		{"DELETE", "/admin/imports/1", ""},
	} {
		e.call(t, r.method, r.path, e.memberTok, r.body, 403, nil)
	}
	if n := e.abs.Requests.Load(); n != 0 {
		t.Errorf("a refused request reached ABS %d times", n)
	}
	if imps, _ := e.cat.ListImports(context.Background(), 0); len(imps) != 0 {
		t.Errorf("a refused request started an import: %+v", imps)
	}
}

func TestParseImportCutoff(t *testing.T) {
	t.Parallel()
	day, _ := time.ParseInLocation(time.DateOnly, "2026-09-15", time.Local)
	for raw, want := range map[string]importer.Cutoff{
		`"auto"`:                 {Auto: true},
		`null`:                   {},
		`"2026-09-15"`:           {At: day},
		`"2026-09-15T10:00:00Z"`: {At: time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)},
	} {
		got, ok := parseImportCutoff(json.RawMessage(raw))
		if !ok || got.Auto != want.Auto || !got.At.Equal(want.At) {
			t.Errorf("%s = %+v, %v; want %+v", raw, got, ok, want)
		}
	}
	for _, raw := range []string{`"yesterday"`, `12`, `"AUTO"`, `{}`} {
		if _, ok := parseImportCutoff(json.RawMessage(raw)); ok {
			t.Errorf("%s accepted", raw)
		}
	}
}
