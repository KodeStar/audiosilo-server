package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
)

// Sync v2 (Track W1): POST /me/listening/sync.

const (
	syncURL      = "/api/v1/me/listening/sync"
	syncUnsouled = "Will Wight/Cradle/01 - Unsouled.m4b"
	syncMistborn = "Brandon Sanderson/Mistborn/01 - The Final Empire.m4b"
)

// syncEnv is a test env with the fixture library scanned.
type syncEnv struct {
	*testEnv
	lib *catalog.Library
}

func newSyncEnv(t *testing.T) *syncEnv {
	t.Helper()
	e := newTestEnv(t)
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	lib, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Main", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := library.NewScanner(e.cat, "", nil).Scan(context.Background(), *lib); err != nil {
		t.Fatal(err)
	}
	return &syncEnv{testEnv: e, lib: lib}
}

// listenJSON is one listen event on path, ended a minute ago and lasting 60 s.
func listenJSON(id string, lib int64, path string, from, to float64, base string) string {
	end := time.Now().Add(-time.Minute)
	b := ""
	if base != "" {
		b = `,"base_version":` + base
	}
	return fmt.Sprintf(`{"id":%q,"kind":"listen","library_id":%d,"path":%q,"from_pos":%v,"to_pos":%v,`+
		`"playback_speed":1,"started_at":%q,"ended_at":%q,"start":"resume"%s}`,
		id, lib, path, from, to, end.Add(-time.Duration((to-from)*float64(time.Second))).UTC().Format(time.RFC3339Nano),
		end.UTC().Format(time.RFC3339Nano), b)
}

func syncBody(device string, events ...string) string {
	return fmt.Sprintf(`{"device_id":%q,"sent_at":%q,"events":[%s]}`, device,
		time.Now().UTC().Format(time.RFC3339Nano), strings.Join(events, ","))
}

type syncAnswer struct {
	ServerTime string `json:"server_time"`
	Results    []struct {
		ID         string `json:"id"`
		Status     string `json:"status"`
		Reason     string `json:"reason"`
		Kept       string `json:"kept"`
		OtherPlace *struct {
			Position   float64 `json:"position"`
			DeviceName string  `json:"device_name"`
		} `json:"other_place"`
	} `json:"results"`
	Progress []catalog.Progress `json:"progress"`
}

func (s *syncEnv) post(t *testing.T, token, body string) (int, syncAnswer, string) {
	t.Helper()
	resp, raw := s.do(t, "POST", syncURL, token, body)
	var a syncAnswer
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal([]byte(raw), &a); err != nil {
			t.Fatalf("answer %s: %v", raw, err)
		}
	}
	return resp.StatusCode, a, raw
}

// Allowed: a session, an API key, a kid and a demo account sync. Denied: no
// token, and a cast, calendar feed or Bridge token (each 401).
func TestListeningSyncAuth(t *testing.T) {
	t.Parallel()
	s := newSyncEnv(t)
	ctx := context.Background()
	issue := func(user int64, kind string) string {
		t.Helper()
		tok, err := s.auth.IssueToken(ctx, user, kind, "dev", 0)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	kidID, kidTok := newUserWithSession(t, s.testEnv, "kiddo", false)
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET managed_by=?,member_kind='kid',member_access='all' WHERE id=?`,
		s.adminID, kidID); err != nil {
		t.Fatal(err)
	}
	demoID, demoTok := newUserWithSession(t, s.testEnv, "visitor", true)
	for _, uid := range []int64{kidID, demoID} {
		if err := s.cat.GrantWholeLibrary(ctx, uid, s.lib.ID); err != nil {
			t.Fatal(err)
		}
	}
	for name, tok := range map[string]string{
		"session": issue(s.adminID, auth.KindSession), "api key": issue(s.adminID, auth.KindAPI),
		"kid": kidTok, "demo": demoTok,
	} {
		code, a, raw := s.post(t, tok, syncBody("dev-"+strings.ReplaceAll(name, " ", ""),
			listenJSON("ok-"+strings.ReplaceAll(name, " ", ""), s.lib.ID, syncUnsouled, 0, 60, "0")))
		if code != http.StatusOK || len(a.Results) != 1 || (a.Results[0].Status != "applied" && a.Results[0].Status != "diverged") {
			t.Errorf("%s: %d %s", name, code, raw)
		}
	}
	for name, tok := range map[string]string{
		"none": "", "cast": issue(s.adminID, auth.KindCast), "feed": issue(s.adminID, auth.KindFeed),
		"bridge": issue(s.adminID, auth.KindBridge),
	} {
		if code, _, raw := s.post(t, tok, syncBody("dev", listenJSON("no-"+name, s.lib.ID, syncUnsouled, 0, 60, "0"))); code != http.StatusUnauthorized {
			t.Errorf("%s token: %d %s", name, code, raw)
		}
	}
	if n := s.eventCount(t); n != 4 {
		t.Fatalf("%d events stored, want 4", n)
	}
}

func (s *syncEnv) eventCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM listening_events WHERE kind <> 'put'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The envelope's 400s; a bad event is never one (it is that event's result).
func TestListeningSyncEnvelope(t *testing.T) {
	t.Parallel()
	s := newSyncEnv(t)
	tok, _ := s.auth.IssueToken(context.Background(), s.adminID, auth.KindSession, "Phone", 0)
	now := time.Now().UTC().Format(time.RFC3339)
	many := make([]string, catalog.MaxSyncEvents+1)
	for i := range many {
		many[i] = listenJSON("m"+strconv.Itoa(i), s.lib.ID, syncUnsouled, 0, 1, "")
	}
	for name, tc := range map[string]struct{ body, msg, code string }{
		"not json":       {`{`, "invalid request", ""},
		"unknown field":  {`{"device_id":"d","sent_at":"` + now + `","events":[],"extra":1}`, "invalid request", ""},
		"no device":      {`{"sent_at":"` + now + `","events":[]}`, "device_id is required", ""},
		"long device":    {`{"device_id":"` + strings.Repeat("d", 201) + `","sent_at":"` + now + `","events":[]}`, "device_id is too long", ""},
		"no clock":       {`{"device_id":"d","events":[]}`, "send sent_at or clock_offset", ""},
		"both clocks":    {`{"device_id":"d","sent_at":"` + now + `","clock_offset":1,"events":[]}`, "send sent_at or clock_offset", ""},
		"bad sent_at":    {`{"device_id":"d","sent_at":"today","events":[]}`, "send sent_at or clock_offset", ""},
		"huge offset":    {`{"device_id":"d","clock_offset":2e9,"events":[]}`, "send sent_at or clock_offset", ""},
		"too many":       {syncBody("d", many...), "", "too_many_events"},
		"body over 1MiB": {`{"device_id":"` + strings.Repeat("d", 1<<20) + `"}`, "invalid request", ""},
	} {
		resp, raw := s.do(t, "POST", syncURL, tok, tc.body)
		var e struct{ Error, Code string }
		_ = json.Unmarshal([]byte(raw), &e)
		if resp.StatusCode != http.StatusBadRequest || (tc.msg != "" && e.Error != tc.msg) || e.Code != tc.code {
			t.Errorf("%s: %d %.200s", name, resp.StatusCode, raw)
		}
	}
	// An empty batch is a clock probe; clock_offset is a valid clock; a bad event
	// is its own "invalid" while the rest applies.
	code, a, raw := s.post(t, tok, syncBody("d"))
	if code != http.StatusOK || a.ServerTime == "" || len(a.Results) != 0 || !strings.Contains(raw, `"progress":[]`) {
		t.Fatalf("probe: %d %s", code, raw)
	}
	body := `{"device_id":"d","clock_offset":12.5,"events":[{"id":"bad","kind":"listen","from_pos":"x"},` +
		listenJSON("good", s.lib.ID, syncUnsouled, 0, 30, "") + `]}`
	if code, a, raw = s.post(t, tok, body); code != http.StatusOK || a.Results[0].Status != "rejected" ||
		a.Results[0].Reason != "invalid" || a.Results[1].Status != "applied" {
		t.Fatalf("bad event: %d %s", code, raw)
	}
}

// Scope: an event outside the caller's share is no_access while the rest of
// the batch applies (a library the caller holds no grant on reads the same,
// as on every route); an unknown library (to an admin) or a hidden path is
// not_found; a track path keys its book's progress, and that progress
// (other_place, device_name) is on every progress read.
func TestListeningSyncScopeAndProgressReads(t *testing.T) {
	t.Parallel()
	s := newSyncEnv(t)
	ctx := context.Background()
	uid, tok := newUserWithSession(t, s.testEnv, "sam", false)
	grantWightOnly(t, s.testEnv, s.lib.ID, uid)
	watch, err := s.auth.IssueToken(ctx, uid, auth.KindSession, "Sam's Watch", 0)
	if err != nil {
		t.Fatal(err)
	}
	code, a, raw := s.post(t, tok, syncBody("phone",
		listenJSON("in", s.lib.ID, syncUnsouled, 17000, 17500, "0"),
		listenJSON("out", s.lib.ID, syncMistborn, 0, 60, "0"),
		listenJSON("nolib", 9999, syncUnsouled, 0, 60, "0"),
		listenJSON("hidden", s.lib.ID, "Will Wight/.hidden/x.m4b", 0, 60, "0"),
		listenJSON("dots", s.lib.ID, "Will Wight/..", 0, 60, "0")))
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, raw)
	}
	want := [][2]string{{"applied", ""}, {"rejected", "no_access"}, {"rejected", "no_access"}, {"rejected", "not_found"},
		{"rejected", "invalid"}}
	for i, w := range want {
		if a.Results[i].Status != w[0] || a.Results[i].Reason != w[1] {
			t.Errorf("event %d: %+v, want %v", i, a.Results[i], w)
		}
	}
	book, err := s.cat.BookPathOfFile(ctx, s.lib.ID, syncUnsouled)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Progress) != 1 || a.Progress[0].Path != book || a.Progress[0].DeviceName != "sam's phone" {
		t.Fatalf("answer's progress (want the book %q): %+v", book, a.Progress)
	}
	v := strconv.FormatInt(a.Progress[0].Version, 10)
	adminTok, _ := s.auth.IssueToken(ctx, s.adminID, auth.KindSession, "Admin", 0)
	if _, a, raw = s.post(t, adminTok, syncBody("admin", listenJSON("nolib-admin", 9999, syncUnsouled, 0, 60, ""))); a.Results[0].Reason != "not_found" {
		t.Fatalf("unknown library to an admin: %s", raw)
	}

	// The phone carries on (version 2), then the watch's span from version 1,
	// behind it: diverged, kept stored, offered and persisted.
	if _, a, raw = s.post(t, tok, syncBody("phone", listenJSON("in2", s.lib.ID, syncUnsouled, 17500, 18000, v))); a.Results[0].Status != "applied" {
		t.Fatalf("carry on: %s", raw)
	}
	_, a, raw = s.post(t, watch, syncBody("watch", listenJSON("w1", s.lib.ID, syncUnsouled, 14400, 16800, v)))
	if r := a.Results[0]; r.Status != "diverged" || r.Kept != "stored" || r.OtherPlace == nil ||
		r.OtherPlace.Position != 16800 || r.OtherPlace.DeviceName != "Sam's Watch" {
		t.Fatalf("watch: %s", raw)
	}
	libPath := "/api/v1/libraries/" + strconv.FormatInt(s.lib.ID, 10)
	for _, url := range []string{libPath + "/progress?path=" + url.QueryEscape(book), "/api/v1/me/progress"} {
		_, body := s.do(t, "GET", url, tok, "")
		if !strings.Contains(body, `"device_name":"sam's phone"`) ||
			!strings.Contains(body, `"other_place":{"position":16800,"device_id":"watch","device_name":"Sam's Watch"`) {
			t.Errorf("%s: %s", url, body)
		}
	}
}

// A podcast library the caller's client can't see (no client header: an
// older player) is refused as PUT .../progress refuses it (no grant it can
// see: no_access); the podcast player syncs there.
func TestListeningSyncHiddenPodcastLibrary(t *testing.T) {
	t.Parallel()
	b, uid, f := podcastEnv(t, false)
	ep := f.Episodes[0].Path
	tok, _ := b.auth.IssueToken(context.Background(), uid, auth.KindSession, "old", 0)
	_, a, raw := (&syncEnv{testEnv: b.testEnv}).post(t, tok, syncBody("old", listenJSON("pod", b.lib.ID, ep, 0, 30, "")))
	if len(a.Results) != 1 || a.Results[0].Status != "rejected" || a.Results[0].Reason != "no_access" {
		t.Fatalf("hidden podcast: %s", raw)
	}
	prog := "/api/v1/libraries/" + strconv.FormatInt(b.lib.ID, 10) + "/progress?path=" + url.QueryEscape(ep)
	if resp, _ := b.do(t, "PUT", prog, tok, `{"position":1}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PUT parity: %d", resp.StatusCode)
	}
	resp, raw := b.doHeaders(t, "POST", syncURL, b.userTok, syncBody("new", listenJSON("pod2", b.lib.ID, ep, 0, 30, "")),
		podcastHeaders())
	if resp.StatusCode != http.StatusOK || !strings.Contains(raw, `"status":"applied"`) {
		t.Fatalf("podcast player: %d %s", resp.StatusCode, raw)
	}
}

// GET /server advertises listening_sync.
func TestListeningSyncCapability(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	_, body := e.do(t, "GET", "/api/v1/server", "", "")
	if !strings.Contains(body, `"listening_sync":true`) {
		t.Fatalf("capability missing: %s", body)
	}
}

// An old player's PUT answers the shape it always did (plus the omitempty
// device_name) and is logged as a "put" event; the name is the token's, never
// the body's, and an echoed other_place is accepted and ignored.
func TestPutProgressStillLegacy(t *testing.T) {
	t.Parallel()
	s := newSyncEnv(t)
	tok, _ := s.auth.IssueToken(context.Background(), s.adminID, auth.KindSession, "Old iPhone", 0)
	prog := "/api/v1/libraries/" + strconv.FormatInt(s.lib.ID, 10) + "/progress?path=" + url.QueryEscape(syncUnsouled)
	resp, raw := s.do(t, "PUT", prog, tok, `{"position":12.5,"duration":120,"device_id":"old","updated_at":"2026-10-25T07:00:00Z",`+
		`"device_name":"Forged","other_place":{"position":1,"device_id":"x","device_name":"y","updated_at":"z"}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put: %d %s", resp.StatusCode, raw)
	}
	var out struct {
		Progress map[string]json.RawMessage `json:"progress"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(out.Progress))
	for k := range out.Progress {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{"device_id", "device_name", "duration", "finished", "library_id", "path", "playback_speed", "position",
		"started_at", "updated_at", "version"}
	if !slices.Equal(keys, want) || string(out.Progress["device_name"]) != `"Old iPhone"` {
		t.Fatalf("PUT answer keys %v: %s", keys, raw)
	}
	var kind, status string
	if err := s.db.QueryRowContext(context.Background(), `SELECT kind, status FROM listening_events`).Scan(&kind, &status); err != nil ||
		kind != "put" || status != "applied" {
		t.Fatalf("put event: %s %s %v", kind, status, err)
	}
}

// The listener's own PATCH and the admin's PATCH mark a finish-state change: a
// sync unfinish dated before the edit is "older" and leaves the book finished,
// one dated after it applies.
func TestEditedFinishHoldsAgainstAnOlderSyncUnfinish(t *testing.T) {
	t.Parallel()
	s := newSyncEnv(t)
	ctx := context.Background()
	uid, tok := newUserWithSession(t, s.testEnv, "sam", false)
	if err := s.cat.GrantWholeLibrary(ctx, uid, s.lib.ID); err != nil {
		t.Fatal(err)
	}
	book, err := s.cat.BookPathOfFile(ctx, s.lib.ID, syncUnsouled)
	if err != nil {
		t.Fatal(err)
	}
	adminTok := adminSession(t, s.testEnv)
	unfinish := func(id string, at time.Time) string {
		t.Helper()
		_, a, raw := s.post(t, tok, syncBody("watch", fmt.Sprintf(
			`{"id":%q,"kind":"unfinish","library_id":%d,"path":%q,"at":%q}`, id, s.lib.ID, book,
			at.UTC().Format(time.RFC3339Nano))))
		if len(a.Results) != 1 {
			t.Fatalf("unfinish %s: %s", id, raw)
		}
		return a.Results[0].Status + " " + a.Results[0].Reason
	}
	finished := func() bool {
		t.Helper()
		p, err := s.cat.GetProgress(ctx, uid, catalog.Ref{LibraryID: s.lib.ID, Path: book})
		if err != nil || p == nil {
			t.Fatalf("progress: %+v, %v", p, err)
		}
		return p.Finished
	}
	for _, w := range []struct{ name, url, tok string }{
		{"own", bookURL(s.lib.ID, "progress", book), tok},
		{"admin", "/api/v1/admin/libraries/" + strconv.FormatInt(s.lib.ID, 10) + "/progress?path=" +
			url.QueryEscape(book) + "&user_id=" + strconv.FormatInt(uid, 10), adminTok},
	} {
		before := time.Now().Add(-time.Second)
		if resp, raw := s.do(t, "PATCH", w.url, w.tok, `{"finished":true}`); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s PATCH: %d %s", w.name, resp.StatusCode, raw)
		}
		if got := unfinish(w.name+"-older", before); got != "not_applied older" || !finished() {
			t.Fatalf("%s: older unfinish %q, finished %v", w.name, got, finished())
		}
		time.Sleep(2 * time.Millisecond)
		if got := unfinish(w.name+"-newer", time.Now()); got != "applied " || finished() {
			t.Fatalf("%s: newer unfinish %q, finished %v", w.name, got, finished())
		}
	}
}
