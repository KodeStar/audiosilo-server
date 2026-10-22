package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/bridge"
)

func podcastLimitEnv(t *testing.T, n int) (*bridgeEnv, []string, string) {
	t.Helper()
	b, f := podcastStorageEnv(t)
	immediatePodcastWait(b)
	paths := make([]string, n)
	template := f.Episodes[2]
	for i := range paths {
		ep := template
		ep.ID = fmt.Sprintf("%016x", i+100)
		ep.Path = fmt.Sprintf("Fixture Show/limit-%d.mp3", i)
		ep.Fetch = nil
		ep.RemovedAt = ""
		paths[i] = ep.Path
		f.Episodes = append(f.Episodes, ep)
	}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(b.lib.Root, "Fixture Show", "feed.json"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err = b.api.scanner.IndexPodcastPath(context.Background(), *b.lib, "Fixture Show"); err != nil {
		t.Fatal(err)
	}
	return b, paths, f.Episodes[0].Path
}

func TestPodcastFetchLimitHTTPAllowedDenied(t *testing.T) {
	for _, kid := range []bool{false, true} {
		t.Run(fmt.Sprint("kid=", kid), func(t *testing.T) {
			b, paths, ready := podcastLimitEnv(t, 7)
			token := b.userTok
			if kid {
				id, tok := newUserWithSession(t, b.testEnv, "limit-kid", false)
				token = tok
				if _, err := b.db.ExecContext(context.Background(), `UPDATE users SET managed_by=?,member_kind='kid',member_access='all' WHERE id=?`, b.adminID, id); err != nil {
					t.Fatal(err)
				}
			}
			for i := range 5 {
				method, route, body := "POST", fetchRoute(b.lib.ID, paths[i]), fetchBody(paths[i])
				headers := podcastHeaders()
				want := 202
				if i > 0 {
					method, body, want = "GET", "", 503
					route = podcastRoute(b.lib.ID, "stream?path="+url.QueryEscape(paths[i])+[]string{"", "", "&download=1", "&transcode=1", ""}[i])
					if i == 4 {
						headers["Range"] = "bytes=0-99"
					}
				}
				resp, body := b.doHeaders(t, method, route, token, body, headers)
				if resp.StatusCode != want || strings.Contains(body, "fetch_limit") {
					t.Fatalf("allowed %d: %d %s", i, resp.StatusCode, body)
				}
			}
			resp, body := b.doHeaders(t, "POST", fetchRoute(b.lib.ID, paths[5]), token, fetchBody(paths[5]), podcastHeaders())
			if resp.StatusCode != 409 || !strings.Contains(body, `"code":"fetch_limit"`) || !strings.Contains(body, "Too many episodes are being fetched. Try again shortly.") {
				t.Fatalf("POST denial %d %s", resp.StatusCode, body)
			}
			for _, extra := range []string{"", "&download=1", "&transcode=1"} {
				headers := podcastHeaders()
				headers["Range"] = "bytes=0-99"
				resp, body = b.doHeaders(t, "GET", podcastRoute(b.lib.ID, "stream?path="+url.QueryEscape(paths[5])+extra), token, "", headers)
				var pending struct {
					Code  string             `json:"code"`
					Fetch bridge.FetchStatus `json:"fetch"`
				}
				if resp.StatusCode != 503 || json.Unmarshal([]byte(body), &pending) != nil || pending.Code != "podcast_fetch_unavailable" || pending.Fetch.State != "unavailable" || pending.Fetch.RequestID != nil || pending.Fetch.RetryAfterSeconds != 2 || pending.Fetch.Error == nil || pending.Fetch.Error.Code != "fetch_limit" || pending.Fetch.Error.Message != "Too many episodes are being fetched. Try again shortly." || resp.Header.Get("Retry-After") != "2" || resp.Header.Get("Cache-Control") != "no-store" {
					t.Fatalf("stream denial %d %s", resp.StatusCode, body)
				}
			}
			var n int
			if err := b.db.QueryRowContext(context.Background(), `SELECT count(*) FROM bridge_jobs WHERE kind='podcast.fetch'`).Scan(&n); err != nil || n != 5 {
				t.Fatalf("denials wrote jobs: %d, %v", n, err)
			}
			// Ready media and an existing own intent still work at the cap.
			resp, body = b.doHeaders(t, "GET", podcastRoute(b.lib.ID, "stream?path="+url.QueryEscape(ready)), token, "", podcastHeaders())
			if resp.StatusCode != 200 {
				t.Fatalf("ready at cap %d %s", resp.StatusCode, body)
			}
			resp, body = b.doHeaders(t, "POST", fetchRoute(b.lib.ID, paths[0]), token, fetchBody(paths[0]), podcastHeaders())
			if resp.StatusCode != 202 {
				t.Fatalf("own dedupe at cap %d %s", resp.StatusCode, body)
			}
			adminRoute := fmt.Sprintf("/api/v1/admin/libraries/%d/podcasts/fetch", b.lib.ID)
			resp, body = b.doHeaders(t, "POST", adminRoute, b.adminTok, fetchBody(paths[6]), podcastHeaders())
			if resp.StatusCode != 202 {
				t.Fatalf("admin at cap %d %s", resp.StatusCode, body)
			}
			resp, body = b.doHeaders(t, "POST", fetchRoute(b.lib.ID, paths[6]), token, fetchBody(paths[6]), podcastHeaders())
			if resp.StatusCode != 202 || strings.Contains(body, "fetch_limit") {
				t.Fatalf("household promotion at cap %d %s", resp.StatusCode, body)
			}
			var by int64
			if err := b.db.QueryRowContext(context.Background(), `SELECT requested_by FROM bridge_jobs WHERE rel_path=?`, paths[6]).Scan(&by); err != nil || by != b.adminID {
				t.Fatalf("promotion changed creator: %d, %v", by, err)
			}
		})
	}
}

func TestPodcastFetchLimitConcurrentHTTP(t *testing.T) {
	b, paths, _ := podcastLimitEnv(t, 20)
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan bool, len(paths))
	for i, path := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			method, route, body := "POST", fetchRoute(b.lib.ID, path), fetchBody(path)
			headers := podcastHeaders()
			if i%4 != 0 {
				method, body = "GET", ""
				route = podcastRoute(b.lib.ID, "stream?path="+url.QueryEscape(path))
				if i%4 == 1 {
					headers["Range"] = "bytes=0-99"
				}
				if i%4 == 2 {
					route += "&download=1"
				}
			}
			resp, body := b.doHeaders(t, method, route, b.userTok, body, headers)
			denied := strings.Contains(body, `"code":"fetch_limit"`)
			want := 503
			if method == "POST" {
				want = 202
				if denied {
					want = 409
				}
			}
			if resp.StatusCode != want {
				t.Errorf("concurrent %d %s", resp.StatusCode, body)
			}
			results <- denied
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	denied := 0
	for d := range results {
		if d {
			denied++
		}
	}
	var n int
	if err := b.db.QueryRowContext(context.Background(), `SELECT count(*) FROM bridge_jobs WHERE kind='podcast.fetch'`).Scan(&n); err != nil || n != 5 || denied != 15 {
		t.Fatalf("concurrent cap: stored=%d denied=%d, %v", n, denied, err)
	}
}
