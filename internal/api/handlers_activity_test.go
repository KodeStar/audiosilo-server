package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
)

const unsouled = "Will Wight/Cradle/01 - Unsouled.m4b"

// activityEnv is a test env with one library (the testdata fixtures), an admin
// "player" token on a device called iPhone, an admin console token, and a member.
type activityEnv struct {
	*testEnv
	libID              int64
	playerTok, console string
	memberTok          string
	memberID           int64
}

func newActivityEnv(t *testing.T) *activityEnv {
	t.Helper()
	e := newTestEnv(t)
	ctx := context.Background()
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	console, memberTok, memberID := adminAndMember(t, e)
	player, err := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "iPhone", 0)
	if err != nil {
		t.Fatal(err)
	}
	return &activityEnv{testEnv: e, libID: lib.ID, playerTok: player, console: console, memberTok: memberTok, memberID: memberID}
}

func (e *activityEnv) libPath() string { return "/api/v1/libraries/" + strconv.FormatInt(e.libID, 10) }

// play sends a progress save from the player token, naming its app.
func (e *activityEnv) play(t *testing.T, position float64) {
	t.Helper()
	resp, body := e.doHeaders(t, "PUT", e.libPath()+"/progress?path="+url.QueryEscape(unsouled), e.playerTok,
		`{"position":`+strconv.FormatFloat(position, 'f', -1, 64)+`,"duration":120}`,
		map[string]string{auth.ClientHeader: "AudioSilo/1.4.2 (ios)"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("progress save = %d %s", resp.StatusCode, body)
	}
}

func TestSessionsFromProgressSaves(t *testing.T) {
	e := newActivityEnv(t)
	e.play(t, 10)
	e.play(t, 12)

	// Allowed: the admin sees the live session with its device and app.
	resp, body := e.do(t, "GET", "/api/v1/admin/sessions/live", e.console, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("live = %d %s", resp.StatusCode, body)
	}
	var live struct {
		Sessions []catalog.Session `json:"sessions"`
	}
	json.Unmarshal([]byte(body), &live)
	if len(live.Sessions) != 1 {
		t.Fatalf("live sessions = %s", body)
	}
	s := live.Sessions[0]
	if s.Path != unsouled || s.DeviceName != "iPhone" || s.Client == nil || s.Client.Platform != "ios" ||
		s.State != catalog.SessionPlaying || s.Username != "admin" || s.IP == "" || s.EndPos != 12 {
		t.Fatalf("live session = %+v", s)
	}

	resp, body = e.do(t, "GET", "/api/v1/admin/sessions?user_id="+strconv.FormatInt(e.adminID, 10), e.console, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"next_before":null`) || !strings.Contains(body, `"device_name":"iPhone"`) {
		t.Fatalf("history = %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(t, "GET", "/api/v1/admin/sessions?path=x", e.console, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("path without library_id = %d %s, want 400", resp.StatusCode, body)
	}

	// Denied: a member can't see anyone's sessions.
	for _, p := range []string{"/api/v1/admin/sessions/live", "/api/v1/admin/sessions"} {
		if resp, _ := e.do(t, "GET", p, e.memberTok, ""); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("member GET %s = %d, want 403", p, resp.StatusCode)
		}
	}
}

func TestDevicesEndpoints(t *testing.T) {
	e := newActivityEnv(t)
	e.play(t, 10) // the player token names its app

	resp, body := e.do(t, "GET", "/api/v1/admin/devices?user_id="+strconv.FormatInt(e.adminID, 10), e.console, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("devices = %d %s", resp.StatusCode, body)
	}
	var out struct {
		Devices []auth.Device `json:"devices"`
	}
	json.Unmarshal([]byte(body), &out)
	var player, console *auth.Device
	for i := range out.Devices {
		switch out.Devices[i].Name {
		case "iPhone":
			player = &out.Devices[i]
		case "t":
			console = &out.Devices[i]
		}
	}
	if player == nil || console == nil || player.Client == nil || player.Client.Version != "1.4.2" ||
		player.Current || !console.Current || console.Client != nil {
		t.Fatalf("devices = %s", body)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/admin/devices?user_id=abc", e.console, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad user_id = %d, want 400", resp.StatusCode)
	}

	// Denied: a member can neither list nor sign out devices.
	if resp, _ := e.do(t, "GET", "/api/v1/admin/devices", e.memberTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member list = %d, want 403", resp.StatusCode)
	}
	if resp, _ := e.do(t, "DELETE", "/api/v1/admin/devices/"+strconv.FormatInt(player.ID, 10), e.memberTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member sign-out = %d, want 403", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/me", e.playerTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatal("a refused sign-out must leave the device signed in")
	}

	// The admin can't sign out the device they are using.
	resp, body = e.do(t, "DELETE", "/api/v1/admin/devices/"+strconv.FormatInt(console.ID, 10), e.console, "")
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, `"code":"current_device"`) {
		t.Fatalf("sign out own device = %d %s, want 409 current_device", resp.StatusCode, body)
	}

	// Allowed: sign out the player; it is refused afterwards, the console isn't.
	if resp, body := e.do(t, "DELETE", "/api/v1/admin/devices/"+strconv.FormatInt(player.ID, 10), e.console, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("sign out = %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/me", e.playerTok, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("signed-out device = %d, want 401", resp.StatusCode)
	}
	if resp, _ := e.do(t, "DELETE", "/api/v1/admin/devices/"+strconv.FormatInt(player.ID, 10), e.console, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second sign-out = %d, want 404", resp.StatusCode)
	}
}

func TestAdminEditProgress(t *testing.T) {
	e := newActivityEnv(t)
	member := strconv.FormatInt(e.memberID, 10)
	adminLib := "/api/v1/admin/libraries/" + strconv.FormatInt(e.libID, 10)
	edit := adminLib + "/progress?path=" + url.QueryEscape(unsouled) + "&user_id=" + member
	if resp, body := e.do(t, "PATCH", edit, e.console, `{"finished":true}`); resp.StatusCode != http.StatusNotFound {
		// Nothing indexed and no progress yet: the book is unknown.
		t.Fatalf("edit before indexing = %d %s, want 404", resp.StatusCode, body)
	}
	ctx := context.Background()
	if _, err := e.cat.SaveProgress(ctx, e.memberID, catalog.Progress{
		Ref: catalog.Ref{LibraryID: e.libID, Path: unsouled}, Position: 30, Duration: 120,
	}); err != nil {
		t.Fatal(err)
	}

	// Allowed.
	resp, body := e.do(t, "PATCH", edit, e.console, `{"finished":true,"started_at":"2026-09-01"}`)
	startOfDay := time.Date(2026, 9, 1, 0, 0, 0, 0, time.Local).UTC().Format(time.RFC3339)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"finished":true`) ||
		!strings.Contains(body, `"position":120`) || !strings.Contains(body, `"started_at":"`+startOfDay+`"`) {
		t.Fatalf("mark finished = %d %s (a date means the start of that day)", resp.StatusCode, body)
	}
	// Started and finished today, at any time of day (a date once meant noon, so a
	// morning edit had the finish before the start).
	today := time.Now().Format(time.DateOnly)
	if resp, body := e.do(t, "PATCH", edit, e.console, `{"finished":true,"started_at":"`+today+`","finished_at":"`+today+`"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("started and finished today = %d %s", resp.StatusCode, body)
	}
	resp, body = e.do(t, "GET", "/api/v1/admin/users/"+member+"/progress", e.console, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"finished_at":"`) {
		t.Fatalf("user progress = %d %s", resp.StatusCode, body)
	}
	for _, bad := range []string{`{"finished_at":"not a date"}`, `{"position":-1}`, `{"bogus":1}`, `{"finished":false,"finished_at":"2026-09-02"}`} {
		if resp, body := e.do(t, "PATCH", edit, e.console, bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("edit %s = %d %s, want 400", bad, resp.StatusCode, body)
		}
	}
	if resp, _ := e.do(t, "PATCH", adminLib+"/progress?path=x&user_id=9999", e.console, `{}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown user = %d, want 404", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/admin/users/9999/progress", e.console, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown user progress = %d, want 404", resp.StatusCode)
	}

	// Denied: a member can't edit or read anyone's progress here (not even their own).
	if resp, _ := e.do(t, "PATCH", edit, e.memberTok, `{"finished":false}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member edit = %d, want 403", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/admin/users/"+member+"/progress", e.memberTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member read = %d, want 403", resp.StatusCode)
	}
}

// An admin's edit starts progress only on a book the person can see (409
// no_access otherwise), and an admin's own scope doesn't count.
func TestAdminEditProgressNeedsTheUsersAccess(t *testing.T) {
	e := newActivityEnv(t)
	ctx := context.Background()
	lib, err := e.cat.GetLibrary(ctx, e.libID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.api.scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	// The Cradle folder holds two parts, so it is one book.
	const cradle = "Will Wight/Cradle"
	edit := "/api/v1/admin/libraries/" + strconv.FormatInt(e.libID, 10) + "/progress?path=" +
		url.QueryEscape(cradle) + "&user_id=" + strconv.FormatInt(e.memberID, 10)

	// Denied: the member has no access to the library.
	resp, body := e.do(t, "PATCH", edit, e.console, `{"finished":true}`)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, `"code":"no_access"`) {
		t.Fatalf("edit without the user's access = %d %s, want 409 no_access", resp.StatusCode, body)
	}
	if rows, _ := e.cat.ListUserProgress(ctx, e.memberID); len(rows) != 0 {
		t.Fatalf("a refused edit wrote progress: %+v", rows)
	}

	// Allowed once the member can see the book.
	if err := e.cat.GrantWholeLibrary(ctx, e.memberID, e.libID); err != nil {
		t.Fatal(err)
	}
	if resp, body := e.do(t, "PATCH", edit, e.console, `{"finished":true}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("edit with the user's access = %d %s, want 200", resp.StatusCode, body)
	}
	// An admin target always has access.
	self := "/api/v1/admin/libraries/" + strconv.FormatInt(e.libID, 10) + "/progress?path=" +
		url.QueryEscape(cradle) + "&user_id=" + strconv.FormatInt(e.adminID, 10)
	if resp, body := e.do(t, "PATCH", self, e.console, `{"finished":true}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("edit for an admin = %d %s, want 200", resp.StatusCode, body)
	}
}

func TestAdminStatsRange(t *testing.T) {
	e := newActivityEnv(t)
	e.play(t, 10)
	e.play(t, 20)

	resp, body := e.do(t, "GET", "/api/v1/admin/stats?range=7d", e.console, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats range = %d %s", resp.StatusCode, body)
	}
	var out struct {
		Activity *catalog.Activity `json:"activity"`
	}
	json.Unmarshal([]byte(body), &out)
	if out.Activity == nil || out.Activity.Range != "7d" || len(out.Activity.Days) < 7 ||
		out.Activity.Totals.Sessions != 1 || out.Activity.Totals.Listeners != 1 {
		t.Fatalf("activity = %s", body)
	}
	if resp, body := e.do(t, "GET", "/api/v1/admin/stats", e.console, ""); resp.StatusCode != http.StatusOK || strings.Contains(body, `"activity"`) {
		t.Fatalf("stats without range = %d %s (no activity expected)", resp.StatusCode, body)
	}
	resp, body = e.do(t, "GET", "/api/v1/admin/stats?range=3w", e.console, "")
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, `"code":"invalid_range"`) {
		t.Fatalf("bad range = %d %s, want 400 invalid_range", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/admin/stats?range=7d", e.memberTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member stats = %d, want 403", resp.StatusCode)
	}
}

func TestCORSAllowsClientHeader(t *testing.T) {
	e := newTestEnvWith(t, func(c *config.Config) { c.CORSOrigins = []string{"http://localhost:8081"} })
	resp, _ := e.doHeaders(t, "OPTIONS", "/api/v1/me", "", "", map[string]string{"Origin": "http://localhost:8081"})
	if got := resp.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(got, auth.ClientHeader) {
		t.Fatalf("allowed origin: Access-Control-Allow-Headers = %q, want it to list %s", got, auth.ClientHeader)
	}
	resp, _ = e.doHeaders(t, "OPTIONS", "/api/v1/me", "", "", map[string]string{"Origin": "http://evil.example"})
	if got := resp.Header.Get("Access-Control-Allow-Headers"); got != "" {
		t.Fatalf("other origin: Access-Control-Allow-Headers = %q, want none", got)
	}
}

// TestAccountLimiterKeysOnClientIPAfterAuth guards the request-context keys: the
// client address must survive authentication. It once collided with the
// token-kind key, so every signed-in caller shared one self-service bucket per
// token kind instead of one per address.
func TestAccountLimiterKeysOnClientIPAfterAuth(t *testing.T) {
	e := newTestEnvWith(t, func(c *config.Config) { c.TrustedProxies = []string{"127.0.0.1/32"} })
	tok, err := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	if err != nil {
		t.Fatal(err)
	}
	from := func(ip string) int {
		resp, _ := e.doHeaders(t, "POST", "/api/v1/auth/password", tok, `{"password":"x"}`,
			map[string]string{"X-Forwarded-For": ip})
		return resp.StatusCode
	}
	for i := 0; i < 10; i++ {
		if got := from("192.0.2.1"); got == http.StatusTooManyRequests {
			t.Fatalf("attempt %d from one address was limited too early", i+1)
		}
	}
	// Denied: that address has used its attempts.
	if got := from("192.0.2.1"); got != http.StatusTooManyRequests {
		t.Fatalf("11th attempt from the same address = %d, want 429", got)
	}
	// Allowed: another address has its own bucket.
	if got := from("192.0.2.2"); got == http.StatusTooManyRequests {
		t.Fatal("a different address must not share the first one's bucket")
	}
}
