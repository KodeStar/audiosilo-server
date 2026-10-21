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
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

func TestPodcastFollowDoesNotGrantReplacementShow(t *testing.T) {
	t.Parallel()
	b, uid, f := podcastEnv(t, true)
	ctx := t.Context()
	followURL := privateFeed
	follow := func() {
		t.Helper()
		resp, body := b.doHeaders(t, "POST", "/api/v1/podcasts/follow", b.userTok,
			fmt.Sprintf(`{"library_id":%d,"feed_url":%q}`, b.lib.ID, followURL), podcastHeaders())
		if resp.StatusCode != 200 {
			t.Fatalf("follow: %d %s", resp.StatusCode, body)
		}
	}
	check := func(want bool) {
		t.Helper()
		show, err := b.cat.PodcastShow(ctx, b.lib.ID, "Example Show", uid)
		if err != nil || show.Following != want {
			t.Fatalf("following: %+v %v, want %v", show, err, want)
		}
		due, err := b.cat.DuePodcastPolls(ctx, 10)
		if err != nil || (len(due) == 1) != want {
			t.Fatalf("due polls: %+v %v, want followed=%v", due, err, want)
		}
		for _, route := range []string{podcastRoute(b.lib.ID, "podcasts"), "/api/v1/me/podcasts"} {
			resp, body := b.doHeaders(t, "GET", route, b.userTok, "", podcastHeaders())
			var list struct{ Shows []catalog.PodcastShow }
			decodeInto(t, body, &list)
			if resp.StatusCode != 200 || (len(list.Shows) == 1) != want || strings.Contains(body, `"show_id"`) {
				t.Fatalf("%s: %d %s, visible=%v", route, resp.StatusCode, body, want)
			}
		}
		for _, route := range []string{"item?path=" + url.QueryEscape(f.Episodes[0].Path), "stream?path=" + url.QueryEscape(f.Episodes[0].Path)} {
			resp, body := b.doHeaders(t, "GET", podcastRoute(b.lib.ID, route), b.userTok, "", podcastHeaders())
			code := 403
			if want {
				code = 200
				if strings.HasPrefix(route, "stream") {
					code = 404 // The allowed episode has not been downloaded.
				}
			}
			if resp.StatusCode != code {
				t.Fatalf("%s: %d %s, want %d", route, resp.StatusCode, body, code)
			}
		}
	}
	follow()
	check(true)
	for _, sameIdentity := range []bool{true, false} {
		if err := b.cat.PrunePodcastShows(ctx, b.lib.ID, []string{"Example Show"}); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM podcast_follows WHERE user_id=? AND library_id=?`, uid, b.lib.ID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("durable follow: %d %v", count, err)
		}
		if !sameIdentity {
			f.ShowID = "0123456789abcdef"
			followURL = privateFeed + "-replacement"
			f.FeedURLSHA256 = feed.FeedURLHash(followURL)
		}
		if _, err := b.cat.IndexPodcastShow(ctx, b.lib.ID, "Example Show", f.ShowID, "", "", f, nil, true); err != nil {
			t.Fatal(err)
		}
		check(sameIdentity)
	}
	// Knowing the replacement's URL can replace the stale follow at this path.
	follow()
	check(true)
}

func TestPodcastEmptyPrivateLibraryAccess(t *testing.T) {
	t.Parallel()
	b, _, f := podcastEnv(t, true)
	_, outsider := newUserWithSession(t, b.testEnv, "outsider", false)
	for _, tok := range []string{b.userTok, b.adminTok} {
		for _, route := range []string{podcastRoute(b.lib.ID, "podcasts"), podcastRoute(b.lib.ID, "fs"), podcastRoute(b.lib.ID, "books"), "/api/v1/me/podcasts"} {
			resp, body := b.doHeaders(t, "GET", route, tok, "", podcastHeaders())
			var list struct {
				Shows   []catalog.PodcastShow `json:"shows"`
				Entries []json.RawMessage     `json:"entries"`
				Books   []json.RawMessage     `json:"books"`
			}
			decodeInto(t, body, &list)
			if resp.StatusCode != 200 || len(list.Shows) != 0 || len(list.Entries) != 0 || len(list.Books) != 0 {
				t.Fatalf("empty library %s: %d %s", route, resp.StatusCode, body)
			}
		}
	}
	for _, tc := range []struct{ token, header string }{{outsider, "AudioSilo/dev (web)"}, {b.adminKey, "API tool"}} {
		for _, route := range []string{"podcasts", "fs"} {
			resp, body := b.doHeaders(t, "GET", podcastRoute(b.lib.ID, route), tc.token, "", map[string]string{"X-AudioSilo-Client": tc.header})
			if resp.StatusCode != 403 {
				t.Fatalf("denied library %s: %d %s", route, resp.StatusCode, body)
			}
		}
	}
	for _, route := range []string{"item?path=" + url.QueryEscape(f.Episodes[0].Path), "stream?path=" + url.QueryEscape(f.Episodes[0].Path)} {
		resp, body := b.doHeaders(t, "GET", podcastRoute(b.lib.ID, route), b.userTok, "", podcastHeaders())
		if resp.StatusCode != 403 {
			t.Fatalf("denied content %s: %d %s", route, resp.StatusCode, body)
		}
	}
}

func TestPodcastRestoreAfterLaterFailure(t *testing.T) {
	t.Parallel()
	for _, later := range []string{"library_not_writable", "timeout"} {
		t.Run(later, func(t *testing.T) {
			t.Parallel()
			b, _, _ := podcastEnv(t, true)
			ctx := t.Context()
			show := "Example Show"
			job := bridge.Job{ID: 1001, Kind: bridge.KindPodcastPoll, LibraryID: &b.lib.ID, Path: &show,
				State: bridge.StateFailed, Result: json.RawMessage(`{"code":"feed_secret_missing","message":"lost"}`)}
			if err := b.cat.FoldPodcastJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			if later == "library_not_writable" {
				if err := b.cat.PodcastNotWritable(ctx, catalog.Ref{LibraryID: b.lib.ID, Path: show}); err != nil {
					t.Fatal(err)
				}
			} else {
				job.ID++
				job.Result = json.RawMessage(`{"code":"timeout","message":"later failure"}`)
				if err := b.cat.FoldPodcastJob(ctx, job); err != nil {
					t.Fatal(err)
				}
			}
			resp, body := b.doHeaders(t, "POST", "/api/v1/podcasts/follow", b.userTok,
				fmt.Sprintf(`{"library_id":%d,"feed_url":%q}`, b.lib.ID, privateFeed), podcastHeaders())
			if resp.StatusCode != 202 {
				t.Fatalf("restore after %s: %d %s", later, resp.StatusCode, body)
			}
			var response struct{ Request struct{ ID int64 } }
			decodeInto(t, body, &response)
			var stored string
			if err := b.db.QueryRowContext(ctx, `SELECT params FROM bridge_jobs WHERE id=?`, response.Request.ID).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			var params bridge.SubscribeParams
			if err := json.Unmarshal([]byte(stored), &params); err != nil || !params.Private || params.FeedURL != "" || params.ShowPath != show {
				t.Fatalf("restore params: %s %v", stored, err)
			}
		})
	}
}

// Deadline is consulted when the handler derives its bounded rescan context,
// after the request's library lookup. Cancel the request at that boundary.
type podcastRescanParent struct {
	context.Context
	cancelRequest context.CancelFunc
}

func (c podcastRescanParent) Deadline() (time.Time, bool) {
	c.cancelRequest()
	return c.Context.Deadline()
}

func TestPodcastRescanOutlivesRequest(t *testing.T) {
	t.Parallel()
	b, _, f := podcastEnv(t, false)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b.api.SetBaseContext(podcastRescanParent{Context: t.Context(), cancelRequest: cancel})
	f.Show.Title = "Updated after request cancellation"
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.lib.Root, "Example Show", "feed.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	request := func(ctx context.Context) *http.Request {
		return httptest.NewRequest("POST", "/api/v1/bridge/rescan", strings.NewReader(fmt.Sprintf(`{"library_id":%d,"path":"Example Show"}`, b.lib.ID))).WithContext(ctx)
	}
	response := httptest.NewRecorder()
	b.api.handleBridgeRescan(response, request(ctx))
	if ctx.Err() != context.Canceled || response.Code != 200 {
		t.Fatalf("detached rescan: request=%v response=%d %s", ctx.Err(), response.Code, response.Body.String())
	}
	show, err := b.cat.PodcastShow(t.Context(), b.lib.ID, "Example Show", 0)
	if err != nil || show.Title != f.Show.Title {
		t.Fatalf("rescan was not saved: %+v %v", show, err)
	}
	// The server lifecycle still bounds work detached from a request.
	base, stop := context.WithCancel(t.Context())
	stop()
	b.api.SetBaseContext(base)
	response = httptest.NewRecorder()
	b.api.handleBridgeRescan(response, request(t.Context()))
	if response.Code != 500 {
		t.Fatalf("stopped server rescan: %d %s", response.Code, response.Body.String())
	}
}

func TestPodcastRootOverlapStillRejectedByAPI(t *testing.T) {
	t.Parallel()
	b, _, _ := podcastEnv(t, false)
	books, err := b.cat.GetLibraryByName(t.Context(), "Main")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, route, body string }{
		{"POST", "/api/v1/admin/libraries", fmt.Sprintf(`{"name":"Clash","root":%q}`, filepath.Join(b.lib.Root, "nested"))},
		{"PATCH", fmt.Sprintf("/api/v1/admin/libraries/%d", b.lib.ID), fmt.Sprintf(`{"root":%q}`, filepath.Join(books.Root, "nested"))},
	} {
		resp, body := b.do(t, tc.method, tc.route, b.adminTok, tc.body)
		if resp.StatusCode != 409 || !strings.Contains(body, `"code":"root_overlaps"`) {
			t.Fatalf("%s %s: %d %s", tc.method, tc.route, resp.StatusCode, body)
		}
	}
}
