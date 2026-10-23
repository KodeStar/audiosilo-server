package api

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

const privateFeed = "https://feeds.example.com/private?auth=SECRET123"

func podcastEnv(t *testing.T, private bool) (*bridgeEnv, int64, *feed.File) {
	t.Helper()
	b := newBridgeEnv(t)
	ctx := context.Background()
	root := t.TempDir()
	dir := filepath.Join(root, "Example Show")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := feed.ReadFile("../podcast/testdata/feed-v1.json", "Example Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Private = private
	if private {
		f.FeedURL = ""
		f.FeedURLSHA256 = feed.FeedURLHash(privateFeed)
		f.Show.Link = ""
	}
	raw := []byte(mustJSON(t, f))
	if err := os.WriteFile(filepath.Join(dir, "feed.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	b.lib, err = b.cat.CreateLibrary(ctx, catalog.Library{Name: "Podcasts", Root: root, Kind: catalog.KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	scanFixture(t, b.testEnv, b.lib)
	uid, tok := newUserWithSession(t, b.testEnv, "alice", false)
	b.userTok = tok
	if err := b.cat.GrantWholeLibrary(ctx, uid, b.lib.ID); err != nil {
		t.Fatal(err)
	}
	b.beat(t, bridge.KindPodcastSubscribe, bridge.KindPodcastPoll)
	podcastsModule(t, b, true)
	return b, uid, f
}

// podcastsModule turns the podcasts module on or off as the admin.
func podcastsModule(t *testing.T, b *bridgeEnv, on bool) {
	t.Helper()
	resp, body := b.do(t, "PATCH", "/api/v1/admin/settings", b.adminTok, fmt.Sprintf(`{"modules":{"podcasts":%t}}`, on))
	if resp.StatusCode != 200 {
		t.Fatalf("podcasts module %t: %d %s", on, resp.StatusCode, body)
	}
}

func podcastHeaders() map[string]string {
	return map[string]string{auth.ClientHeader: "AudioSilo/dev (web)"}
}
func podcastRoute(id int64, suffix string) string {
	return fmt.Sprintf("/api/v1/libraries/%d/%s", id, suffix)
}
func TestPodcastClientGateMatrix(t *testing.T) {
	b, uid, f := podcastEnv(t, false)
	ctx := context.Background()
	episode := f.Episodes[0].Path
	for _, id := range []int64{uid, b.adminID} {
		if _, err := b.cat.SaveProgress(ctx, id, catalog.Progress{Ref: catalog.Ref{LibraryID: b.lib.ID, Path: episode}, Position: 30, Duration: 1720}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, header, kind string
		admin, allowed     bool
	}{
		{"dev", "AudioSilo/dev (web)", auth.KindSession, false, true},
		{"console", "AudioSilo Admin (web)", auth.KindSession, true, true},
		{"released", "AudioSilo/2.2.0 (ios)", auth.KindSession, false, true},
		{"released admin", "AudioSilo/2.2.0 (web)", auth.KindSession, true, true},
		{"newer", "AudioSilo/2.3.0 (android)", auth.KindSession, false, true},
		{"just below", "AudioSilo/2.1.9 (ios)", auth.KindSession, false, false},
		{"old", "AudioSilo/2.1.0 (web)", auth.KindSession, false, false},
		{"old admin", "AudioSilo/2.1.0 (web)", auth.KindSession, true, false},
		{"missing", "", auth.KindSession, false, false},
		{"api key", "", auth.KindAPI, false, false},
		{"manager", "AudioSilo Manager/dev (desktop)", auth.KindSession, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := uid
			if tc.admin {
				id = b.adminID
			}
			tok, err := b.auth.IssueToken(ctx, id, tc.kind, tc.name, 0)
			if err != nil {
				t.Fatal(err)
			}
			headers := map[string]string{}
			if tc.header != "" {
				headers[auth.ClientHeader] = tc.header
			}
			resp, body := b.doHeaders(t, "GET", "/api/v1/libraries", tok, "", headers)
			if resp.StatusCode != 200 {
				t.Fatal(body)
			}
			var list struct{ Libraries []catalog.Library }
			decodeInto(t, body, &list)
			visible := false
			for _, l := range list.Libraries {
				if l.ID == b.lib.ID {
					visible = true
					if l.Kind != "podcasts" {
						t.Fatal(body)
					}
				}
			}
			if visible != tc.allowed {
				t.Fatalf("libraries %s", body)
			}
			for _, route := range []string{"fs", "books", "item?path=" + url.QueryEscape(episode), "stream?path=" + url.QueryEscape(episode), "progress?path=" + url.QueryEscape(episode)} {
				resp, body = b.doHeaders(t, "GET", podcastRoute(b.lib.ID, route), tok, "", headers)
				if tc.allowed {
					want := 200
					if strings.HasPrefix(route, "stream") {
						want = 503
						if !strings.Contains(body, "podcast_fetch_unavailable") {
							t.Fatal(body)
						}
					}
					if resp.StatusCode != want {
						t.Fatalf("%s %d %s", route, resp.StatusCode, body)
					}
				} else if resp.StatusCode != 403 {
					t.Fatalf("cached %s %d %s", route, resp.StatusCode, body)
				}
			}
			_, body = b.doHeaders(t, "GET", "/api/v1/me/progress", tok, "", headers)
			if strings.Contains(body, episode) != tc.allowed {
				t.Fatalf("me/progress %s", body)
			}
			// Headerless media inherits this token's stored app.
			resp, body = b.do(t, "GET", podcastRoute(b.lib.ID, "stream?path="+url.QueryEscape(episode)+"&token="+url.QueryEscape(tok)), "", "")
			want := 403
			if tc.allowed {
				want = 503
			}
			if resp.StatusCode != want || tc.allowed && !strings.Contains(body, "podcast_fetch_unavailable") {
				t.Fatalf("media inherited %d %s", resp.StatusCode, body)
			}
		})
	}
}

func TestPodcastPrivateShowScopes(t *testing.T) {
	b, uid, f := podcastEnv(t, true)
	ctx := context.Background()
	headers := podcastHeaders()
	episode := f.Episodes[1].Path
	f.Episodes[1].Fetch.Bytes = 5
	raw := []byte(mustJSON(t, f))
	if err := os.WriteFile(filepath.Join(b.lib.Root, "Example Show", "feed.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.lib.Root, filepath.FromSlash(episode)), []byte("audio"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.api.scanner.IndexPodcastPath(ctx, *b.lib, "Example Show"); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{b.userTok, b.adminTok} {
		for _, route := range []string{"item?path=" + url.QueryEscape(episode), "stream?path=" + url.QueryEscape(episode)} {
			resp, body := b.doHeaders(t, "GET", podcastRoute(b.lib.ID, route), tok, "", headers)
			if resp.StatusCode != 403 {
				t.Fatalf("non-follower %s %d %s", route, resp.StatusCode, body)
			}
		}
	}
	body := fmt.Sprintf(`{"library_id":%d,"feed_url":%q}`, b.lib.ID, privateFeed)
	resp, out := b.doHeaders(t, "POST", "/api/v1/podcasts/follow", b.userTok, body, headers)
	if resp.StatusCode != 200 || strings.Contains(out, "SECRET123") || !strings.Contains(out, `"feed_url":null`) {
		t.Fatalf("proof %d %s", resp.StatusCode, out)
	}
	for _, route := range []string{"fs", "fs?path=Example+Show", "books", "item?path=" + url.QueryEscape(episode), "stream?path=" + url.QueryEscape(episode), "podcasts", "podcasts/episodes?show=Example+Show"} {
		resp, out = b.doHeaders(t, "GET", podcastRoute(b.lib.ID, route), b.userTok, "", headers)
		if resp.StatusCode != 200 {
			t.Fatalf("follower %s %d %s", route, resp.StatusCode, out)
		}
	}
	// Reciprocal household listening still hides a private show from an admin
	// using the player until that admin follows it.
	for _, id := range []int64{uid, b.adminID} {
		if err := b.cat.SetHouseholdSharing(ctx, id, true); err != nil {
			t.Fatal(err)
		}
	}
	for _, pos := range []float64{10, 20} {
		if _, err := b.cat.SaveProgress(ctx, uid, catalog.Progress{Ref: catalog.Ref{LibraryID: b.lib.ID, Path: episode}, Position: pos, Duration: 1512}); err != nil {
			t.Fatal(err)
		}
	}
	resp, out = b.doHeaders(t, "GET", "/api/v1/household/listening", b.adminTok, "", headers)
	if resp.StatusCode != 200 || strings.Contains(out, "Example Show") {
		t.Fatalf("private household %d %s", resp.StatusCode, out)
	}

	// A grant inside a private show is discarded until this caller follows it.
	other, otherTok := newUserWithSession(t, b.testEnv, "other", false)
	share, err := b.cat.CreateShare(ctx, catalog.Share{Name: "Episode"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.cat.AddSharePath(ctx, share.ID, catalog.PathRule{LibraryID: b.lib.ID, Path: episode}); err != nil {
		t.Fatal(err)
	}
	if err := b.cat.GrantShare(ctx, other, share.ID); err != nil {
		t.Fatal(err)
	}
	resp, out = b.doHeaders(t, "GET", podcastRoute(b.lib.ID, "item?path="+url.QueryEscape(episode)), otherTok, "", headers)
	if resp.StatusCode != 403 {
		t.Fatalf("private nested grant %d %s", resp.StatusCode, out)
	}
	_, out = b.doHeaders(t, "GET", "/api/v1/search?q=Episode", b.userTok, "", headers)
	if strings.Contains(out, "Example Show") {
		t.Fatal(out)
	}
	resp, out = b.doHeaders(t, "DELETE", fmt.Sprintf("/api/v1/podcasts/follow?library_id=%d&path=Example+Show", b.lib.ID), b.userTok, "", headers)
	if resp.StatusCode != 204 {
		t.Fatalf("unfollow %d %s", resp.StatusCode, out)
	}
	s, err := b.cat.CallerScope(ctx, catalog.Caller{ID: uid, Podcasts: true}, b.lib.ID)
	if err != nil || s.AllowAll || len(s.Paths) != 0 {
		t.Fatalf("unfollow scope %+v %v", s, err)
	}
}
func TestPodcastURLSecrecyAndRestart(t *testing.T) {
	b, _, _ := podcastEnv(t, false)
	headers := podcastHeaders()
	var logs bytes.Buffer
	b.api.log = slog.New(slog.NewTextHandler(&logs, nil))
	follow := func(secret string) bridge.Job {
		t.Helper()
		resp, out := b.doHeaders(t, "POST", "/api/v1/podcasts/follow", b.userTok, fmt.Sprintf(`{"library_id":%d,"feed_url":%q}`, b.lib.ID, secret), headers)
		if resp.StatusCode != 202 || strings.Contains(out, "SECRET123") {
			t.Fatalf("follow %d %s", resp.StatusCode, out)
		}
		var response struct{ Request struct{ ID int64 } }
		decodeInto(t, out, &response)
		j, err := b.api.bridge.Job(context.Background(), response.Request.ID)
		if err != nil {
			t.Fatal(err)
		}
		return *j
	}
	j := follow(privateFeed)
	for _, route := range []string{fmt.Sprintf("/api/v1/admin/bridge/jobs/%d", j.ID), "/api/v1/admin/bridge/jobs", "/api/v1/me/podcasts"} {
		_, out := b.doHeaders(t, "GET", route, b.adminTok, "", headers)
		if strings.Contains(out, "SECRET123") {
			t.Fatal(out)
		}
	}
	var stored string
	if err := b.db.QueryRowContext(context.Background(), `SELECT params FROM bridge_jobs WHERE id=?`, j.ID).Scan(&stored); err != nil || strings.Contains(stored, "SECRET123") {
		t.Fatalf("stored %s %v", stored, err)
	}
	resp, out := b.do(t, "GET", "/api/v1/bridge/jobs/next?kind=podcast.subscribe", b.bridgeTok, "")
	if resp.StatusCode != 200 || !strings.Contains(out, "SECRET123") {
		t.Fatalf("claim credential %d %s", resp.StatusCode, out)
	}
	var claim bridge.Job
	decodeInto(t, out, &claim)
	payload := fmt.Sprintf(`{"attempt":%d,"state":"failed","error":%q,"result":{"code":"http_401","message":%q},"progress":{"message":%q}}`, claim.Attempts, "failed "+privateFeed, "failed "+privateFeed, "fetching "+privateFeed)
	resp, out = b.do(t, "PATCH", fmt.Sprintf("/api/v1/bridge/jobs/%d", j.ID), b.bridgeTok, payload)
	if resp.StatusCode != 200 || strings.Contains(out, "SECRET123") {
		t.Fatalf("patch %d %s", resp.StatusCode, out)
	}
	for _, route := range []string{fmt.Sprintf("/api/v1/admin/bridge/jobs/%d", j.ID), "/api/v1/admin/bridge/jobs", "/api/v1/me/podcasts"} {
		_, out = b.doHeaders(t, "GET", route, b.userTok, "", headers)
		if strings.Contains(out, "SECRET123") {
			t.Fatal(out)
		}
	}
	if err := b.db.QueryRowContext(context.Background(), `SELECT params||progress||result||error FROM bridge_jobs WHERE id=?`, j.ID).Scan(&stored); err != nil || strings.Contains(stored, "SECRET123") {
		t.Fatalf("stored error %s %v", stored, err)
	}
	j = follow(privateFeed + "&retry=1")
	b.api.SetBridge(bridge.New(b.db, time.Now))
	got, err := b.api.bridge.Job(context.Background(), j.ID)
	if err != nil || got.State != "failed" || !strings.Contains(string(got.Result), "request_expired") {
		t.Fatalf("restart %+v %v", got, err)
	}
	if strings.Contains(logs.String(), "SECRET123") {
		t.Fatal(logs.String())
	}
}
func TestPodcastFollowErrorsAndCoreReads(t *testing.T) {
	b, uid, _ := podcastEnv(t, false)
	h := podcastHeaders()
	post := func(body string, status int, code string) {
		t.Helper()
		resp, out := b.doHeaders(t, "POST", "/api/v1/podcasts/follow", b.userTok, body, h)
		if resp.StatusCode != status || code != "" && !strings.Contains(out, `"code":"`+code+`"`) {
			t.Fatalf("follow %s %d %s", body, resp.StatusCode, out)
		}
	}
	for _, body := range []string{`{}`, fmt.Sprintf(`{"library_id":%d,"path":"Example Show","feed_url":"https://x/"}`, b.lib.ID), fmt.Sprintf(`{"library_id":%d,"feed_url":"https://x/","unknown":true}`, b.lib.ID)} {
		post(body, 400, "invalid_request")
	}
	post(fmt.Sprintf(`{"library_id":%d,"feed_url":"ftp://x/y"}`, b.lib.ID), 400, "invalid_feed_url")
	post(fmt.Sprintf(`{"library_id":%d,"path":"Missing"}`, b.lib.ID), 404, "show_not_found")
	post(fmt.Sprintf(`{"library_id":%d,"path":"Example Show"}`, b.lib.ID), 200, "")
	post(fmt.Sprintf(`{"library_id":%d,"feed_url":"https://feeds.example.com/show.xml"}`, b.lib.ID), 200, "")
	resp, out := b.doHeaders(t, "POST", "/api/v1/podcasts/check", b.userTok, fmt.Sprintf(`{"library_id":%d,"path":"Example Show"}`, b.lib.ID), h)
	if resp.StatusCode != 202 {
		t.Fatalf("check %d %s", resp.StatusCode, out)
	}
	if err := b.cat.UnfollowPodcast(context.Background(), uid, b.lib.ID, "Example Show"); err != nil {
		t.Fatal(err)
	}
	// Turning the module off still permits indexed reads.
	podcastsModule(t, b, false)
	post(fmt.Sprintf(`{"library_id":%d,"path":"Example Show"}`, b.lib.ID), 404, "module_off")
	resp, out = b.doHeaders(t, "GET", podcastRoute(b.lib.ID, "podcasts"), b.userTok, "", h)
	if resp.StatusCode != 200 {
		t.Fatalf("core read %d %s", resp.StatusCode, out)
	}
	for _, kind := range []string{bridge.KindPodcastSubscribe, bridge.KindPodcastPoll} {
		resp, out = b.do(t, "POST", "/api/v1/admin/bridge/jobs", b.adminTok, fmt.Sprintf(`{"kind":%q,"library_id":%d}`, kind, b.lib.ID))
		if resp.StatusCode != 400 {
			t.Fatalf("internal kind %d %s", resp.StatusCode, out)
		}
	}
}

func TestPodcastActionDeniedMatrix(t *testing.T) {
	b, uid, _ := podcastEnv(t, false)
	h := podcastHeaders()
	ctx := context.Background()
	request := fmt.Sprintf(`{"library_id":%d,"feed_url":"https://example.org/new"}`, b.lib.ID)
	check := func(tok, body string, status int, code string) {
		t.Helper()
		resp, out := b.doHeaders(t, "POST", "/api/v1/podcasts/follow", tok, body, h)
		if resp.StatusCode != status || !strings.Contains(out, `"code":"`+code+`"`) {
			t.Fatalf("%s %d %s", code, resp.StatusCode, out)
		}
	}
	_, demoTok := newUserWithSession(t, b.testEnv, "demo-pod", true)
	kid, kidTok := newUserWithSession(t, b.testEnv, "kid-pod", false)
	if _, err := b.db.ExecContext(ctx, `UPDATE users SET managed_by=?,member_kind='kid',member_access='all' WHERE id=?`, b.adminID, kid); err != nil {
		t.Fatal(err)
	}
	check(demoTok, request, 403, "demo_account")
	check(kidTok, request, 403, "kid_account")
	_, outsider := newUserWithSession(t, b.testEnv, "outside-pod", false)
	check(outsider, request, 403, "no_library_access")
	// A subtree grant allows following a visible show by path, but not URLs.
	other, partial := newUserWithSession(t, b.testEnv, "partial-pod", false)
	share, err := b.cat.CreateShare(ctx, catalog.Share{Name: "One show"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.cat.AddSharePath(ctx, share.ID, catalog.PathRule{LibraryID: b.lib.ID, Path: "Example Show"}); err != nil {
		t.Fatal(err)
	}
	if err := b.cat.GrantShare(ctx, other, share.ID); err != nil {
		t.Fatal(err)
	}
	check(partial, request, 403, "no_library_access")
	books, err := b.cat.CreateLibrary(ctx, catalog.Library{Name: "Other Books", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.cat.GrantWholeLibrary(ctx, uid, books.ID); err != nil {
		t.Fatal(err)
	}
	check(b.userTok, fmt.Sprintf(`{"library_id":%d,"feed_url":"https://example.org/"}`, books.ID), 409, "not_podcast_library")
	b.beat(t, bridge.KindVerify)
	check(b.userTok, request, 409, "bridge_unavailable")
	b.beat(t, bridge.KindPodcastSubscribe, bridge.KindPodcastPoll)
	// An online Bridge that cannot write gives the separate actionable code.
	if _, out := b.do(t, "PUT", "/api/v1/bridge/status", b.bridgeTok, fmt.Sprintf(`{"version":"0.9.0","kinds":["podcast.subscribe","podcast.poll"],"libraries":[{"library_id":%d,"mount_path":"/pods","mapped":true,"writable":false}]}`, b.lib.ID)); out == "" {
		t.Fatal("no heartbeat answer")
	}
	check(b.userTok, request, 409, "library_not_writable")
	b.beat(t, bridge.KindPodcastSubscribe, bridge.KindPodcastPoll)
	for i := range 5 {
		resp, out := b.doHeaders(t, "POST", "/api/v1/podcasts/follow", b.userTok, fmt.Sprintf(`{"library_id":%d,"feed_url":"https://example.org/new%d"}`, b.lib.ID, i), h)
		if resp.StatusCode != 202 {
			t.Fatalf("queue %d %s", resp.StatusCode, out)
		}
	}
	check(b.userTok, request, 429, "too_many_requests")
}
func TestPodcastCapabilityMarkerAndRescan(t *testing.T) {
	b, _, f := podcastEnv(t, false)
	h := podcastHeaders()
	caps := serverCapabilities(t, b.testEnv)
	if !caps["podcasts"] {
		t.Fatal("podcasts flag false")
	}
	podcastsModule(t, b, false)
	caps = serverCapabilities(t, b.testEnv)
	if enabled, present := caps["podcasts"]; !present || enabled {
		t.Fatalf("off capability %+v", caps)
	}
	var art bytes.Buffer
	if err := png.Encode(&art, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.lib.Root, "Example Show", "cover.png"), art.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.api.scanner.IndexPodcastPath(context.Background(), *b.lib, "Example Show"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"Example Show", f.Episodes[0].Path} {
		for _, size := range []string{"", "&size=160"} {
			resp, out := b.doHeaders(t, "GET", podcastRoute(b.lib.ID, "cover?path="+url.QueryEscape(path)+size), b.userTok, "", h)
			if resp.StatusCode != 200 {
				t.Fatalf("podcast cover %d %s", resp.StatusCode, out)
			}
		}
	}

	dir := filepath.Join(b.lib.Root, ".audiosilo")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "library.json"), []byte(fmt.Sprintf(`{"version":1,"library_id":%d,"nonce":"0123456789abcdef0123456789abcdef"}`, b.lib.ID)), 0600); err != nil {
		t.Fatal(err)
	}
	marker := fmt.Sprintf("/api/v1/bridge/libraries/%d/marker", b.lib.ID)
	resp, out := b.do(t, "GET", marker, b.bridgeTok, "")
	if resp.StatusCode != 200 || !strings.Contains(out, "0123456789abcdef") {
		t.Fatalf("marker %d %s", resp.StatusCode, out)
	}
	resp, _ = b.do(t, "GET", marker, b.userTok, "")
	if resp.StatusCode != 401 {
		t.Fatalf("marker auth %d", resp.StatusCode)
	}
	if err := os.WriteFile(filepath.Join(dir, "library.json"), []byte(`{"version":1,"library_id":999,"nonce":"0123456789abcdef0123456789abcdef"}`), 0600); err != nil {
		t.Fatal(err)
	}
	resp, out = b.do(t, "GET", marker, b.bridgeTok, "")
	if resp.StatusCode != 404 || !strings.Contains(out, "no_marker") {
		t.Fatalf("marker denied %d %s", resp.StatusCode, out)
	}
	for _, path := range []string{"Example Show", f.Episodes[0].Path} {
		resp, out = b.do(t, "POST", "/api/v1/bridge/rescan", b.bridgeTok, fmt.Sprintf(`{"library_id":%d,"path":%q}`, b.lib.ID, path))
		if resp.StatusCode != 200 || !strings.Contains(out, `"episodes":3`) {
			t.Fatalf("rescan %d %s", resp.StatusCode, out)
		}
	}
	for _, extra := range []string{`,"mode":"attachments"`, `,"from":"Other"`, `,"from":""`} {
		resp, out = b.do(t, "POST", "/api/v1/bridge/rescan", b.bridgeTok, fmt.Sprintf(`{"library_id":%d,"path":"Example Show"%s}`, b.lib.ID, extra))
		if resp.StatusCode != 400 {
			t.Fatalf("rescan denied %d %s", resp.StatusCode, out)
		}
	}
	resp, out = b.do(t, "GET", fmt.Sprintf("/api/v1/bridge/libraries/%d/book", b.lib.ID), b.bridgeTok, "")
	if resp.StatusCode != 404 || !strings.Contains(out, "no_book") {
		t.Fatalf("sample gate %d %s", resp.StatusCode, out)
	}
	resp, out = b.doHeaders(t, "GET", podcastRoute(b.lib.ID, "chapters?path="+url.QueryEscape(f.Episodes[0].Path)), b.userTok, "", h)
	if resp.StatusCode != 200 || !strings.Contains(out, `"chapters":[]`) {
		t.Fatalf("chapters %d %s", resp.StatusCode, out)
	}
}

func TestPodcastCheckRequiresFollowAndThrottles(t *testing.T) {
	b, uid, f := podcastEnv(t, false)
	h := podcastHeaders()
	body := fmt.Sprintf(`{"library_id":%d,"path":"Example Show"}`, b.lib.ID)
	resp, out := b.doHeaders(t, "POST", "/api/v1/podcasts/check", b.userTok, body, h)
	if resp.StatusCode != 404 || !strings.Contains(out, "show_not_found") {
		t.Fatalf("not following %d %s", resp.StatusCode, out)
	}
	if err := b.cat.FollowPodcast(context.Background(), uid, b.lib.ID, "Example Show", f.ShowID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.ExecContext(context.Background(), `INSERT INTO podcast_feed_state(library_id,show_path,show_id,last_checked_at) VALUES(?,'Example Show','a1b2c3d4e5f60718',?)`, b.lib.ID, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	resp, out = b.doHeaders(t, "POST", "/api/v1/podcasts/check", b.userTok, body, h)
	if resp.StatusCode != 429 || !strings.Contains(out, "checked_recently") {
		t.Fatalf("recent %d %s", resp.StatusCode, out)
	}
}

func TestPodcastStatsAndCastGate(t *testing.T) {
	b, uid, f := podcastEnv(t, false)
	h := podcastHeaders()
	ctx := context.Background()
	resp, out := b.do(t, "GET", "/api/v1/admin/stats", b.adminTok, "")
	if resp.StatusCode != 200 {
		t.Fatal(out)
	}
	var stats struct {
		TotalBooks     int `json:"total_books"`
		TotalLibraries int `json:"total_libraries"`
	}
	decodeInto(t, out, &stats)
	if stats.TotalBooks != 2 || stats.TotalLibraries != 2 {
		t.Fatalf("stats %s", out)
	}
	resp, out = b.doHeaders(t, "POST", "/api/v1/cast/token", b.userTok, fmt.Sprintf(`{"library_id":%d,"path":%q}`, b.lib.ID, f.Episodes[0].Path), h)
	if resp.StatusCode != 403 || !strings.Contains(out, `"code":"not_a_book"`) {
		t.Fatalf("cast mint %d %s", resp.StatusCode, out)
	}
	_, cred, err := b.auth.ResolveRequest(ctx, b.userTok, auth.Presence{}, auth.KindSession)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := b.auth.IssueCastToken(ctx, uid, cred.ID, b.lib.ID, f.Episodes[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	resp, out = b.doHeaders(t, "GET", podcastRoute(b.lib.ID, "stream?path="+url.QueryEscape(f.Episodes[0].Path)+"&token="+url.QueryEscape(token)), "", "", h)
	if resp.StatusCode != 403 {
		t.Fatalf("cast media gate %d %s", resp.StatusCode, out)
	}
}

// A path that names no show is a missing directory or cover, not a server error.
func TestPodcastMissingShowIsNotFound(t *testing.T) {
	b, _, f := podcastEnv(t, false)
	h := podcastHeaders()
	for _, route := range []string{"fs?path=Missing", "fs?path=" + url.QueryEscape(f.Episodes[0].Path), "cover?path=Missing", "cover?path=Missing&size=160"} {
		resp, out := b.doHeaders(t, "GET", podcastRoute(b.lib.ID, route), b.userTok, "", h)
		if resp.StatusCode != 404 {
			t.Fatalf("%s %d %s", route, resp.StatusCode, out)
		}
	}
}

// Restoring a private show's lost secret keeps its URL out of bridge_jobs even
// when the request leaves out "private" and the URL looks public.
func TestPodcastRestoreKeepsPrivateURLOutOfJobs(t *testing.T) {
	b, _, f := podcastEnv(t, true)
	ctx := context.Background()
	plain := "https://feeds.example.com/members/show.xml"
	if feed.IsPrivateFeedURL(plain) {
		t.Fatal("the fixture URL must look public")
	}
	f.FeedURLSHA256 = feed.FeedURLHash(plain)
	raw := []byte(mustJSON(t, f))
	if err := os.WriteFile(filepath.Join(b.lib.Root, "Example Show", "feed.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.api.scanner.IndexPodcastPath(ctx, *b.lib, "Example Show"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.ExecContext(ctx, `INSERT INTO podcast_feed_state(library_id,show_path,show_id,error_code,error,secret_missing) VALUES(?,'Example Show','a1b2c3d4e5f60718','feed_secret_missing','lost',1)`, b.lib.ID); err != nil {
		t.Fatal(err)
	}
	resp, out := b.doHeaders(t, "POST", "/api/v1/podcasts/follow", b.userTok, fmt.Sprintf(`{"library_id":%d,"feed_url":%q}`, b.lib.ID, plain), podcastHeaders())
	if resp.StatusCode != 202 || strings.Contains(out, "members") {
		t.Fatalf("restore %d %s", resp.StatusCode, out)
	}
	var stored string
	if err := b.db.QueryRowContext(ctx, `SELECT params FROM bridge_jobs WHERE kind='podcast.subscribe'`).Scan(&stored); err != nil || strings.Contains(stored, "members") || !strings.Contains(stored, `"private":true`) {
		t.Fatalf("stored %s %v", stored, err)
	}
	resp, out = b.do(t, "GET", "/api/v1/bridge/jobs/next?kind=podcast.subscribe", b.bridgeTok, "")
	if resp.StatusCode != 200 || !strings.Contains(out, plain) {
		t.Fatalf("claim %d %s", resp.StatusCode, out)
	}
}

// The console's view of a person lists the podcast libraries they reach; a
// client the old-client gate keeps podcasts from (an API-key tool) is shown none.
func TestPodcastConsoleUserDetail(t *testing.T) {
	b, uid, _ := podcastEnv(t, false)
	route := fmt.Sprintf("/api/v1/admin/users/%d", uid)
	for _, tc := range []struct {
		kind, header string
		want         bool
	}{{auth.KindSession, "AudioSilo Admin (web)", true}, {auth.KindAPI, "", false}} {
		tok, err := b.auth.IssueToken(context.Background(), b.adminID, tc.kind, tc.kind, 0)
		if err != nil {
			t.Fatal(err)
		}
		headers := map[string]string{}
		if tc.header != "" {
			headers[auth.ClientHeader] = tc.header
		}
		resp, body := b.doHeaders(t, "GET", route, tok, "", headers)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", tc.kind, resp.StatusCode, body)
		}
		var detail struct {
			Libraries []catalog.Library `json:"accessible_libraries"`
		}
		decodeInto(t, body, &detail)
		got := false
		for _, l := range detail.Libraries {
			got = got || l.ID == b.lib.ID
		}
		if got != tc.want {
			t.Fatalf("%s: %s", tc.kind, body)
		}
	}
}

// An admin's queue add in no library is a 404; a podcast library the old-client
// gate hides from the request's client is a 403, as for any path.
func TestPodcastQueueAddUnknownAndHiddenLibrary(t *testing.T) {
	b, _, f := podcastEnv(t, false)
	tok, err := b.auth.IssueToken(context.Background(), b.adminID, auth.KindAPI, "tool", 0)
	if err != nil {
		t.Fatal(err)
	}
	for lib, want := range map[int64]int{99999: 404, b.lib.ID: 403} {
		resp, out := b.do(t, "POST", "/api/v1/me/queue", tok, fmt.Sprintf(`{"library_id":%d,"path":%q}`, lib, f.Episodes[0].Path))
		if resp.StatusCode != want {
			t.Fatalf("library %d: %d %s, want %d", lib, resp.StatusCode, out, want)
		}
	}
}

// R-1: /next for an episode answers the show's next episode in the episode
// order (oldest first, whatever the file names), never a series, community
// work or file-name sibling; the show's last episode has none.
func TestNextForEpisodes(t *testing.T) {
	t.Parallel()
	b, _, f := podcastEnv(t, false)
	next := func(path string) (string, string) {
		t.Helper()
		resp, body := b.doHeaders(t, "GET", podcastRoute(b.lib.ID, "next?path="+url.QueryEscape(path)), b.userTok, "", podcastHeaders())
		if resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, body)
		}
		var got struct {
			Source string
			Next   *catalog.Ref
		}
		decodeInto(t, body, &got)
		if got.Next == nil {
			return got.Source, ""
		}
		return got.Source, got.Next.Path
	}
	// feed-v1: 2026-10-19, 2026-10-12, then an undated episode (listed first).
	undated, older, newer := f.Episodes[2].Path, f.Episodes[1].Path, f.Episodes[0].Path
	if src, p := next(undated); src != "folder" || p != older {
		t.Fatalf("after the undated episode: %s %q", src, p)
	}
	if src, p := next(older); src != "folder" || p != newer {
		t.Fatalf("after the older episode: %s %q", src, p)
	}
	if src, p := next(newer); src != "none" || p != "" {
		t.Fatalf("after the last episode: %s %q", src, p)
	}
	// An old player's /next is hidden like the rest of the library.
	resp, _ := b.doHeaders(t, "GET", podcastRoute(b.lib.ID, "next?path="+url.QueryEscape(older)), b.userTok, "", map[string]string{auth.ClientHeader: "AudioSilo/2.1.0 (ios)"})
	if resp.StatusCode == 200 {
		t.Fatal("a hidden podcast library answered /next")
	}
}
