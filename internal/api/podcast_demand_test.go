package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/podcast"
)

// immediatePodcastWait makes the stream's wait end at once on a fake clock. The
// budget's wall-clock deadline is a minute, so a loaded machine never spends it
// before the wait is reached (server#189, server#200); the fake clock runs the
// one wait to the end of that minute in a single step.
func immediatePodcastWait(b *bridgeEnv) {
	b.api.podcastDemand.Budget = time.Minute
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	b.api.podcastDemand.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	b.api.podcastDemand.After = func(d time.Duration) (<-chan time.Time, func()) {
		now := time.Unix(0, clock.Add(int64(d)))
		ch := make(chan time.Time, 1)
		ch <- now
		return ch, func() {}
	}
}
func fetchRoute(lib int64, path string) string {
	return podcastRoute(lib, "podcasts/fetch?path="+url.QueryEscape(path))
}
func fetchBody(path string) string {
	raw, _ := json.Marshal(map[string]string{"path": path})
	return string(raw)
}
func TestPodcastPlayerFetchAllowedDenied(t *testing.T) {
	b, f := podcastStorageEnv(t)
	immediatePodcastWait(b)
	absent := f.Episodes[2].Path
	ready := f.Episodes[0].Path
	kid, kidTok := newUserWithSession(t, b.testEnv, "fetch-kid", false)
	if _, err := b.db.ExecContext(context.Background(), `UPDATE users SET managed_by=?,member_kind='kid',member_access='all' WHERE id=?`, b.adminID, kid); err != nil {
		t.Fatal(err)
	}
	demo, demoTok := newUserWithSession(t, b.testEnv, "fetch-demo", false)
	if err := b.cat.GrantWholeLibrary(context.Background(), demo, b.lib.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.ExecContext(context.Background(), `UPDATE users SET is_demo=1 WHERE id=?`, demo); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{b.userTok, b.adminTok, kidTok} {
		for _, method := range []string{"GET", "POST"} {
			resp, body := b.doHeaders(t, method, fetchRoute(b.lib.ID, absent), tok, fetchBody(absent), podcastHeaders())
			want := 200
			if method == "POST" {
				want = 202
			}
			if resp.StatusCode != want {
				t.Fatalf("%s allowed %d %s", method, resp.StatusCode, body)
			}
		}
	}
	for _, tc := range []struct {
		token, header, path string
		want                int
	}{
		{"", "AudioSilo/dev (web)", absent, 401},
		{b.bridgeTok, "AudioSilo/dev (web)", absent, 401},
		{b.userTok, "AudioSilo/2.1.0 (web)", absent, 403},
		{b.userTok, "AudioSilo/dev (web)", ".hidden/no.mp3", 400},
		{b.userTok, "AudioSilo/dev (web)", "Fixture Show/no.mp3", 404},
	} {
		for _, method := range []string{"GET", "POST"} {
			resp, body := b.doHeaders(t, method, fetchRoute(b.lib.ID, tc.path), tc.token, fetchBody(tc.path), map[string]string{auth.ClientHeader: tc.header})
			if resp.StatusCode != tc.want {
				t.Fatalf("%s denied %d %s want %d", method, resp.StatusCode, body, tc.want)
			}
		}
	}
	resp, body := b.doHeaders(t, "GET", fetchRoute(b.lib.ID, absent), demoTok, "", podcastHeaders())
	if resp.StatusCode != 200 || !strings.Contains(body, `"state":"unavailable"`) {
		t.Fatalf("demo observation %d %s", resp.StatusCode, body)
	}
	for _, route := range []string{"podcasts/episodes?show=Fixture+Show", "item?path=" + url.QueryEscape(absent)} {
		resp, body = b.doHeaders(t, "GET", podcastRoute(b.lib.ID, route), demoTok, "", podcastHeaders())
		if resp.StatusCode != 200 || !strings.Contains(body, `"code":"demo_account"`) {
			t.Fatalf("demo summary %d %s", resp.StatusCode, body)
		}
	}
	resp, body = b.doHeaders(t, "POST", fetchRoute(b.lib.ID, absent), demoTok, fetchBody(absent), podcastHeaders())
	if resp.StatusCode != 403 || !strings.Contains(body, "demo_account") {
		t.Fatalf("demo retry %d %s", resp.StatusCode, body)
	}
	resp, body = b.doHeaders(t, "POST", fetchRoute(b.lib.ID, ready), b.userTok, fetchBody(ready), podcastHeaders())
	if resp.StatusCode != 200 || !strings.Contains(body, `"state":"ready"`) {
		t.Fatalf("ready %d %s", resp.StatusCode, body)
	}
	resp, body = b.doHeaders(t, "POST", fetchRoute(b.lib.ID, absent), b.userTok, `{"path":"Fixture Show/no.mp3","priority":100}`, podcastHeaders())
	if resp.StatusCode != 400 {
		t.Fatalf("strict %d %s", resp.StatusCode, body)
	}
	// An allowed kid's stream also promotes shared work, but grants no policy access.
	resp, body = b.doHeaders(t, "GET", podcastRoute(b.lib.ID, "stream?path="+url.QueryEscape(absent)), kidTok, "", podcastHeaders())
	if resp.StatusCode != 503 || !strings.Contains(body, "podcast_fetching") {
		t.Fatalf("kid stream %d %s", resp.StatusCode, body)
	}
	resp, body = b.doHeaders(t, "POST", fmt.Sprintf("/api/v1/admin/libraries/%d/podcasts/fetch", b.lib.ID), kidTok, fetchBody(absent), podcastHeaders())
	if resp.StatusCode != 403 {
		t.Fatalf("kid admin %d %s", resp.StatusCode, body)
	}
}
func TestPodcastFetchPrivateScopeAndNoJobs(t *testing.T) {
	b, uid, f := podcastEnv(t, true)
	b.beat(t, bridge.KindPodcastFetch)
	for _, method := range []string{"GET", "POST"} {
		resp, body := b.doHeaders(t, method, fetchRoute(b.lib.ID, f.Episodes[0].Path), b.userTok, fetchBody(f.Episodes[0].Path), podcastHeaders())
		if resp.StatusCode != 403 {
			t.Fatalf("private %d %s", resp.StatusCode, body)
		}
	}
	var n int
	if err := b.db.QueryRowContext(context.Background(), `SELECT count(*) FROM bridge_jobs WHERE kind='podcast.fetch'`).Scan(&n); err != nil || n != 0 {
		t.Fatal("denied queued", n, err)
	}
	if err := b.cat.FollowPodcast(context.Background(), uid, b.lib.ID, "Example Show", f.ShowID); err != nil {
		t.Fatal(err)
	}
	resp, body := b.doHeaders(t, "POST", fetchRoute(b.lib.ID, f.Episodes[0].Path), b.userTok, fetchBody(f.Episodes[0].Path), podcastHeaders())
	if resp.StatusCode != 202 {
		t.Fatalf("follow proof %d %s", resp.StatusCode, body)
	}
	if _, err := b.db.ExecContext(context.Background(), `DELETE FROM user_share_access WHERE user_id=?`, uid); err != nil {
		t.Fatal(err)
	}
	resp, body = b.doHeaders(t, "GET", fetchRoute(b.lib.ID, f.Episodes[0].Path), b.userTok, "", podcastHeaders())
	if resp.StatusCode != 403 {
		t.Fatalf("revoked %d %s", resp.StatusCode, body)
	}
}
func TestPodcastFetchUnavailableBranches(t *testing.T) {
	for _, kind := range []string{"module", "offline", "old_bridge", "mapping", "demo"} {
		t.Run(kind, func(t *testing.T) {
			b, f := podcastStorageEnv(t)
			immediatePodcastWait(b)
			path := f.Episodes[2].Path
			want, reason := 409, "bridge_unavailable"
			switch kind {
			case "module":
				resp, body := b.do(t, "PATCH", "/api/v1/admin/settings", b.adminTok, `{"modules":{"podcasts":false}}`)
				if resp.StatusCode != 200 {
					t.Fatal(body)
				}
				want = 404
				reason = "module_off"
			case "offline":
				b.api.bridge.Forget()
				if _, err := b.db.ExecContext(context.Background(), `UPDATE bridge_status SET heartbeat_at='2000-01-01T00:00:00Z'`); err != nil {
					t.Fatal(err)
				}
			case "old_bridge":
				b.beat(t, bridge.KindPodcastPoll)
			case "mapping":
				if err := b.api.bridge.SaveHeartbeat(context.Background(), credentialBridgeID(t, b), bridge.Status{Kinds: []string{bridge.KindPodcastFetch}}); err != nil {
					t.Fatal(err)
				}
				reason = "library_not_writable"
			case "demo":
				if _, err := b.db.ExecContext(context.Background(), `UPDATE users SET is_demo=1 WHERE id=?`, b.adminID); err != nil {
					t.Fatal(err)
				}
				want = 403
				reason = "demo_account"
			}
			resp, body := b.doHeaders(t, "POST", fetchRoute(b.lib.ID, path), b.adminTok, fetchBody(path), podcastHeaders())
			if resp.StatusCode != want || !strings.Contains(body, reason) {
				t.Fatalf("retry %d %s", resp.StatusCode, body)
			}
			for _, extra := range []string{"", "&download=1", "&transcode=1"} {
				resp, body = b.doHeaders(t, "GET", podcastRoute(b.lib.ID, "stream?path="+url.QueryEscape(path)+extra), b.adminTok, "", podcastHeaders())
				if resp.StatusCode != 503 || !strings.Contains(body, "podcast_fetch_unavailable") || !strings.Contains(body, reason) || resp.Header.Get("Retry-After") != "2" || resp.Header.Get("Cache-Control") != "no-store" {
					t.Fatalf("stream %d %s", resp.StatusCode, body)
				}
			}
			// Ready descriptors remain readable in every branch.
			resp, body = b.doHeaders(t, "GET", podcastRoute(b.lib.ID, "stream?path="+url.QueryEscape(f.Episodes[0].Path)), b.adminTok, "", podcastHeaders())
			if resp.StatusCode != 200 {
				t.Fatalf("ready %d %s", resp.StatusCode, body)
			}
			var n int
			if err := b.db.QueryRowContext(context.Background(), `SELECT count(*) FROM bridge_jobs WHERE kind='podcast.fetch'`).Scan(&n); err != nil || n != 0 {
				t.Fatal("unavailable queued", n, err)
			}
		})
	}
}
func credentialBridgeID(t *testing.T, b *bridgeEnv) int64 {
	t.Helper()
	var id int64
	if err := b.db.QueryRowContext(context.Background(), `SELECT id FROM tokens WHERE kind='bridge' AND revoked=0`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
func TestPodcastStreamWaitEnvelopeAndNearCompletion(t *testing.T) {
	b, f := podcastStorageEnv(t)
	immediatePodcastWait(b)
	absent := f.Episodes[2].Path
	for _, extra := range []string{"", "&download=1", "&transcode=1"} {
		headers := podcastHeaders()
		headers["Range"] = "bytes=0-99"
		resp, body := b.doHeaders(t, "GET", podcastRoute(b.lib.ID, "stream?path="+url.QueryEscape(absent)+extra), b.userTok, "", headers)
		if resp.StatusCode != 503 || !strings.Contains(body, "podcast_fetching") || resp.Header.Get("Retry-After") != "2" || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%d %s", resp.StatusCode, body)
		}
	}
	ready := f.Episodes[0].Path
	if err := os.Remove(filepath.Join(b.lib.Root, ready)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.api.scanner.IndexPodcastPath(context.Background(), *b.lib, "Fixture Show"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	b.api.podcastDemand.Now = func() time.Time { return now }
	b.api.podcastDemand.After = func(wait time.Duration) (<-chan time.Time, func()) {
		if wait <= 0 || wait > b.api.podcastDemand.Budget {
			t.Fatalf("wait %v", wait)
		}
		raw, err := os.ReadFile("../podcast/testdata/phase2c2/chap-v24-reverse.mp3")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(b.lib.Root, ready), raw, 0644); err != nil {
			t.Fatal(err)
		}
		if _, _, err = b.api.scanner.IndexPodcastPath(context.Background(), *b.lib, "Fixture Show"); err != nil {
			t.Fatal(err)
		}
		now = now.Add(wait)
		ch := make(chan time.Time, 1)
		ch <- now
		return ch, func() {}
	}
	resp, body := b.doHeaders(t, "GET", podcastRoute(b.lib.ID, "stream?path="+url.QueryEscape(ready)), b.userTok, "", podcastHeaders())
	if resp.StatusCode != 200 || len(body) != 16493 {
		t.Fatalf("near completion %d bytes %d", resp.StatusCode, len(body))
	}
}

// A listener who leaves while the stream waits keeps the fetch they asked for.
// The budget is a minute here: the wait ends when the listener leaves (After
// cancels), and the 2 s wall-clock default ran out before the request was
// queued on a loaded machine (server#189).
func TestPodcastWaiterDisconnectKeepsIntent(t *testing.T) {
	b, f := podcastStorageEnv(t)
	path := f.Episodes[2].Path
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.api.podcastDemand.Budget = time.Minute
	var waited atomic.Bool
	b.api.podcastDemand.After = func(time.Duration) (<-chan time.Time, func()) {
		waited.Store(true)
		cancel()
		return make(chan time.Time), func() {}
	}
	req := httptest.NewRequest("GET", podcastRoute(b.lib.ID, "stream?path="+url.QueryEscape(path)), nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+b.userTok)
	req.Header.Set(auth.ClientHeader, "AudioSilo/dev (web)")
	b.srv.Config.Handler.ServeHTTP(httptest.NewRecorder(), req)
	if !waited.Load() {
		t.Fatal("the stream never waited, so the listener never left during the wait")
	}
	var n int
	if err := b.db.QueryRowContext(context.Background(), `SELECT count(*) FROM bridge_jobs WHERE kind='podcast.fetch' AND rel_path=? AND state='queued' AND cancel_requested=0`, path).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
func TestPodcastFetchConcurrentHTTPAndSummaries(t *testing.T) {
	b, f := podcastStorageEnv(t)
	immediatePodcastWait(b)
	path := f.Episodes[2].Path
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			method, route, body, want := "POST", fetchRoute(b.lib.ID, path), fetchBody(path), 202
			headers := podcastHeaders()
			if i%4 != 0 {
				method = "GET"
				route = podcastRoute(b.lib.ID, "stream?path="+url.QueryEscape(path))
				body = ""
				want = 503
				if i%4 == 1 {
					headers["Range"] = "bytes=0-99"
				}
				if i%4 == 2 {
					route += "&download=1"
				}
			}
			resp, body := b.doHeaders(t, method, route, b.userTok, body, headers)
			if resp.StatusCode != want {
				t.Errorf("%d %s", resp.StatusCode, body)
			}
		}()
	}
	wg.Wait()
	var n, priority int
	if err := b.db.QueryRowContext(context.Background(), `SELECT count(*),max(priority) FROM bridge_jobs WHERE kind='podcast.fetch' AND rel_path=?`, path).Scan(&n, &priority); err != nil || n != 1 || priority != 100 {
		t.Fatal(n, priority, err)
	}
	for _, route := range []string{"podcasts/episodes?show=Fixture+Show", "item?path=" + url.QueryEscape(path)} {
		resp, body := b.doHeaders(t, "GET", podcastRoute(b.lib.ID, route), b.userTok, "", podcastHeaders())
		if resp.StatusCode != 200 || !strings.Contains(body, `"fetch":{"state":"queued"`) {
			t.Fatalf("summary %d %s", resp.StatusCode, body)
		}
	}
	resp, body := b.do(t, "GET", "/api/v1/server", "", "")
	if resp.StatusCode != 200 || !strings.Contains(body, `"podcast_fetch":true`) {
		t.Fatal(body)
	}
}
func TestPodcastClaimPriorityRoute(t *testing.T) {
	b := newBridgeEnv(t)
	for _, value := range []string{"", "1.0", "2147483648", "-2147483649", "abc", "100&min_priority=0"} {
		resp, body := b.do(t, "GET", "/api/v1/bridge/jobs/next?min_priority="+value, b.bridgeTok, "")
		if resp.StatusCode != 400 || !strings.Contains(body, "invalid_request") {
			t.Fatalf("invalid %q %d %s", value, resp.StatusCode, body)
		}
	}
	for _, value := range []string{"-2147483648", "2147483647", "100", "-10"} {
		resp, body := b.do(t, "GET", "/api/v1/bridge/jobs/next?min_priority="+value, b.bridgeTok, "")
		if resp.StatusCode != 204 {
			t.Fatalf("allowed %q %d %s", value, resp.StatusCode, body)
		}
	}
	for _, token := range []string{b.adminTok, b.userTok, ""} {
		resp, body := b.do(t, "GET", "/api/v1/bridge/jobs/next?min_priority=100", token, "")
		if resp.StatusCode != 401 {
			t.Fatalf("denied %d %s", resp.StatusCode, body)
		}
	}
}
func TestPodcastStreamWireGolden(t *testing.T) {
	raw, err := os.ReadFile("../podcast/testdata/phase2c2/wire-2b.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Stream json.RawMessage `json:"stream_response"`
		Status struct {
			Fetch bridge.FetchStatus `json:"fetch"`
		} `json:"status_response"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	writePodcastFetchPending(w, fixture.Status.Fetch)
	var got, want any
	if json.Unmarshal(w.Body.Bytes(), &got) != nil || json.Unmarshal(fixture.Stream, &want) != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("wire %s", w.Body.String())
	}
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "2" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Result())
	}
}

func TestPodcastStreamCooldownAndRetry(t *testing.T) {
	b, f := podcastStorageEnv(t)
	immediatePodcastWait(b)
	path := f.Episodes[2].Path
	resp, body := b.doHeaders(t, "POST", fetchRoute(b.lib.ID, path), b.userTok, fetchBody(path), podcastHeaders())
	if resp.StatusCode != 202 {
		t.Fatal(body)
	}
	held, err := b.api.bridge.ClaimFor(context.Background(), credentialBridgeID(t, b), []string{bridge.KindPodcastFetch}, 0)
	if err != nil || held == nil {
		t.Fatal(err)
	}
	state := bridge.StateFailed
	if _, err = b.api.bridge.UpdateHeld(context.Background(), held.ID, bridge.Update{Attempt: held.Attempts, State: &state, Result: json.RawMessage(`{"code":"too_large","message":"CANARY"}`)}); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		resp, body = b.doHeaders(t, "GET", podcastRoute(b.lib.ID, "stream?path="+url.QueryEscape(path)), b.userTok, "", podcastHeaders())
		if resp.StatusCode != 503 || !strings.Contains(body, "podcast_fetch_failed") || strings.Contains(body, "CANARY") {
			t.Fatalf("cooldown %d %s", resp.StatusCode, body)
		}
	}
	var n int
	if err = b.db.QueryRowContext(context.Background(), `SELECT count(*) FROM bridge_jobs WHERE kind='podcast.fetch'`).Scan(&n); err != nil || n != 1 {
		t.Fatal("stream retry storm", n, err)
	}
	resp, body = b.doHeaders(t, "POST", fetchRoute(b.lib.ID, path), b.userTok, fetchBody(path), podcastHeaders())
	if resp.StatusCode != 202 || !strings.Contains(body, `"state":"queued"`) {
		t.Fatalf("retry %d %s", resp.StatusCode, body)
	}
	if err = b.db.QueryRowContext(context.Background(), `SELECT count(*) FROM bridge_jobs WHERE kind='podcast.fetch' AND state='queued'`).Scan(&n); err != nil || n != 1 {
		t.Fatal("retry intent", n, err)
	}
}
func TestPodcastEnqueueTimeCountsAgainstWait(t *testing.T) {
	b, f := podcastStorageEnv(t)
	path := f.Episodes[2].Path
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	b.api.podcastDemand.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	b.api.podcastDemand.After = func(time.Duration) (<-chan time.Time, func()) {
		t.Fatal("enqueue budget was restarted")
		return nil, func() {}
	}
	moduleOn := b.api.bridge.ModuleOn
	b.api.bridge.ModuleOn = func(id string) bool { clock.Add(int64(podcast.StreamWait)); return moduleOn(id) }
	resp, body := b.doHeaders(t, "GET", podcastRoute(b.lib.ID, "stream?path="+url.QueryEscape(path)), b.userTok, "", podcastHeaders())
	if resp.StatusCode != 503 || !strings.Contains(body, "podcast_fetching") {
		t.Fatalf("budget %d %s", resp.StatusCode, body)
	}
}

func TestPodcastFinalPresenceCheckWithoutWake(t *testing.T) {
	b, f := podcastStorageEnv(t)
	ready := f.Episodes[0].Path
	b.cat.SetPodcastChanged(nil)
	if err := os.Remove(filepath.Join(b.lib.Root, ready)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.api.scanner.IndexPodcastPath(context.Background(), *b.lib, "Fixture Show"); err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	b.api.podcastDemand.Now = func() time.Time { return clock }
	b.api.podcastDemand.After = func(wait time.Duration) (<-chan time.Time, func()) {
		raw, err := os.ReadFile("../podcast/testdata/phase2c2/chap-v24-reverse.mp3")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(b.lib.Root, ready), raw, 0644); err != nil {
			t.Fatal(err)
		}
		if _, _, err = b.api.scanner.IndexPodcastPath(context.Background(), *b.lib, "Fixture Show"); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(wait)
		ch := make(chan time.Time, 1)
		ch <- clock
		return ch, func() {}
	}
	resp, body := b.doHeaders(t, "GET", podcastRoute(b.lib.ID, "stream?path="+url.QueryEscape(ready)), b.userTok, "", podcastHeaders())
	if resp.StatusCode != 200 || len(body) != 16493 {
		t.Fatalf("final presence %d %d", resp.StatusCode, len(body))
	}
}

func TestPodcastFetchRouteTokensAndKinds(t *testing.T) {
	b, f := podcastStorageEnv(t)
	path := f.Episodes[2].Path
	cast, err := b.auth.IssueToken(context.Background(), b.adminID, auth.KindCast, "cast-fetch", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "POST"} {
		for _, route := range []string{fetchRoute(b.lib.ID, path), fetchRoute(b.lib.ID, path) + "&token=" + url.QueryEscape(b.userTok)} {
			tok := cast
			if strings.Contains(route, "&token=") {
				tok = ""
			}
			resp, body := b.doHeaders(t, method, route, tok, fetchBody(path), podcastHeaders())
			if resp.StatusCode != 401 {
				t.Fatalf("token %d %s", resp.StatusCode, body)
			}
		}
		for _, id := range []int64{1, 999999} {
			resp, body := b.doHeaders(t, method, fetchRoute(id, path), b.adminTok, fetchBody(path), podcastHeaders())
			want := 409
			if id == 999999 {
				want = 404
			}
			if resp.StatusCode != want {
				t.Fatalf("kind %d %s", resp.StatusCode, body)
			}
		}
	}
	var n int
	if err = b.db.QueryRowContext(context.Background(), `SELECT count(*) FROM bridge_jobs WHERE kind='podcast.fetch'`).Scan(&n); err != nil || n != 0 {
		t.Fatal("denied observation queued work", n, err)
	}
	// A reader compiled before the optional summary still sees its original types.
	resp, body := b.doHeaders(t, "GET", podcastRoute(b.lib.ID, "item?path="+url.QueryEscape(path)), b.userTok, "", podcastHeaders())
	var old struct {
		Episode struct {
			Downloaded bool   `json:"downloaded"`
			Generation int    `json:"generation"`
			ShowPath   string `json:"show_path"`
		} `json:"episode"`
	}
	if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &old) != nil || old.Episode.Downloaded || old.Episode.Generation != 0 || old.Episode.ShowPath != "Fixture Show" {
		t.Fatalf("legacy shape %d %s", resp.StatusCode, body)
	}
	var newer struct {
		Episode struct {
			Fetch *bridge.FetchStatus `json:"fetch"`
		} `json:"episode"`
	}
	if json.Unmarshal([]byte(`{"episode":{"downloaded":false}}`), &newer) != nil || newer.Episode.Fetch != nil {
		t.Fatal("optional fetch became required")
	}
}
