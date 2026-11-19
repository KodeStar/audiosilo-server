package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

func podcastStorageEnv(t *testing.T) (*bridgeEnv, *feed.File) {
	t.Helper()
	b, _, _ := podcastEnv(t, false)
	dir := filepath.Join(b.lib.Root, "Fixture Show")
	if err := os.MkdirAll(filepath.Join(dir, ".audiosilo"), 0755); err != nil {
		t.Fatal(err)
	}
	f, err := feed.ReadFile("../podcast/testdata/phase2c2/feed-fetch-v1.json", "Fixture Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, dest := range map[string]string{"feed-fetch-v1.json": filepath.Join(dir, "feed.json"), "chap-v24-reverse.mp3": filepath.Join(b.lib.Root, filepath.FromSlash(f.Episodes[0].Path)), "chap-v24-reverse.chapters.json": filepath.Join(dir, ".audiosilo", strings.TrimSuffix(path.Base(f.Episodes[0].Path), ".mp3")+".chapters.json")} {
		raw, err := os.ReadFile("../podcast/testdata/phase2c2/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(dest, raw, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err = b.api.scanner.IndexPodcastPath(context.Background(), *b.lib, "Fixture Show"); err != nil {
		t.Fatal(err)
	}
	b.beat(t, bridge.KindPodcastSubscribe, bridge.KindPodcastPoll, bridge.KindPodcastFetch, bridge.KindPodcastRetain)
	return b, f
}
func TestPodcastStorageRoutesAllowedDenied(t *testing.T) {
	b, f := podcastStorageEnv(t)
	adminBase := fmt.Sprintf("/api/v1/admin/libraries/%d/podcasts/", b.lib.ID)
	bridgeBase := fmt.Sprintf("/api/v1/bridge/libraries/%d/podcasts/", b.lib.ID)
	policy := `{"mode":"keep","keep":3,"days":7}`
	check := fmt.Sprintf(`{"show_id":%q,"path":%q,"generation":2,"sha256":%q,"bytes":16493}`, f.ShowID, f.Episodes[0].Path, f.Episodes[0].Fetch.SHA256)
	cast, err := b.auth.IssueToken(context.Background(), b.adminID, auth.KindCast, "cast", 0)
	if err != nil {
		t.Fatal(err)
	}
	routes := []struct {
		method, route, body string
		status              int
		paired              bool
	}{
		{"GET", adminBase + "retention?path=Fixture+Show", "", 200, false},
		{"PUT", adminBase + "retention?path=Fixture+Show", policy, 200, false},
		{"DELETE", adminBase + "retention?path=Fixture+Show", "", 204, false},
		{"POST", adminBase + "fetch", fmt.Sprintf(`{"path":%q}`, f.Episodes[2].Path), 202, false},
		{"POST", adminBase + "fetch", fmt.Sprintf(`{"path":%q}`, f.Episodes[0].Path), 200, false},
		{"GET", bridgeBase + "retention?path=Fixture+Show&show_id=" + f.ShowID, "", 200, true},
		{"POST", bridgeBase + "retention/check", check, 200, true},
	}
	for _, route := range routes {
		t.Run(route.method+" "+route.route, func(t *testing.T) {
			allowed := b.adminTok
			if route.paired {
				allowed = b.bridgeTok
			}
			resp, body := b.do(t, route.method, route.route, allowed, route.body)
			if resp.StatusCode != route.status {
				t.Fatalf("allowed: %d %s", resp.StatusCode, body)
			}
			if route.paired && resp.Header.Get("Cache-Control") != "no-store" {
				t.Fatal("aggregate cached")
			}
			denied := []string{"", b.userTok, b.bridgeTok, cast}
			if route.paired {
				denied = []string{"", b.userTok, b.adminTok, b.adminKey, cast}
			}
			for _, token := range denied {
				resp, body = b.do(t, route.method, route.route, token, route.body)
				if resp.StatusCode != 401 && resp.StatusCode != 403 {
					t.Fatalf("denied: %d %s", resp.StatusCode, body)
				}
			}
			sep := "?"
			if strings.Contains(route.route, "?") {
				sep = "&"
			}
			resp, body = b.do(t, route.method, route.route+sep+"token="+url.QueryEscape(allowed), "", route.body)
			if resp.StatusCode != 401 {
				t.Fatalf("query token: %d %s", resp.StatusCode, body)
			}
		})
	}
	resp, body := b.do(t, "POST", adminBase+"fetch", b.adminTok, fmt.Sprintf(`{"path":%q}`, f.Episodes[0].Path))
	wantDownloaded(t, "present pin", resp, body)
	for _, kind := range []string{bridge.KindPodcastFetch, bridge.KindPodcastRetain} {
		resp, body = b.do(t, "POST", "/api/v1/admin/bridge/jobs", b.adminTok, fmt.Sprintf(`{"kind":%q,"library_id":%d,"path":"Fixture Show","params":{}}`, kind, b.lib.ID))
		if resp.StatusCode != 400 {
			t.Fatalf("direct internal job: %d %s", resp.StatusCode, body)
		}
	}
}

// wantDownloaded checks the answer for a present, valid pin.
func wantDownloaded(t *testing.T, what string, resp *http.Response, body string) {
	t.Helper()
	if resp.StatusCode != 200 || strings.TrimSpace(body) != `{"downloaded":true}` {
		t.Fatalf("%s: %d %s", what, resp.StatusCode, body)
	}
}

func TestPodcastFetchPresentPinBeforeWriteChecks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                             string
		setupMethod, setupURL, setupBody string
		setupStatus, status              int
		code                             string
	}{
		{"module off", "PATCH", "/api/v1/admin/settings", `{"modules":{"podcasts":false}}`, 200, 404, "module_off"},
		{"no Bridge", "DELETE", "/api/v1/admin/bridge", "", 204, 409, "bridge_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, f := podcastStorageEnv(t)
			route := fmt.Sprintf("/api/v1/admin/libraries/%d/podcasts/fetch", b.lib.ID)
			resp, body := b.do(t, tc.setupMethod, tc.setupURL, b.adminTok, tc.setupBody)
			if resp.StatusCode != tc.setupStatus {
				t.Fatalf("setup: %d %s", resp.StatusCode, body)
			}
			present := fmt.Sprintf(`{"path":%q}`, f.Episodes[0].Path)
			resp, body = b.do(t, "POST", route, b.adminTok, present)
			wantDownloaded(t, "present pin", resp, body)
			resp, body = b.do(t, "POST", route, b.adminTok, fmt.Sprintf(`{"path":%q}`, f.Episodes[2].Path))
			wantCode(t, "absent pin", resp, body, tc.status, tc.code)
			// An indexed pin whose file vanished must still pass the write checks.
			if err := os.Remove(filepath.Join(b.lib.Root, filepath.FromSlash(f.Episodes[0].Path))); err != nil {
				t.Fatal(err)
			}
			resp, body = b.do(t, "POST", route, b.adminTok, present)
			wantCode(t, "stale pin", resp, body, tc.status, tc.code)
			var queued int
			if err := b.db.QueryRowContext(context.Background(), `SELECT count(*) FROM bridge_jobs WHERE kind='podcast.fetch'`).Scan(&queued); err != nil || queued != 0 {
				t.Fatalf("unexpected fetch jobs: %d %v", queued, err)
			}
		})
	}
}
func TestPodcastStorageValidationAndGating(t *testing.T) {
	b, f := podcastStorageEnv(t)
	base := fmt.Sprintf("/api/v1/admin/libraries/%d/podcasts/", b.lib.ID)
	for _, body := range []string{`{}`, `{"mode":"keep","keep":3}`, `{"mode":"keep","keep":0,"days":7}`, `{"mode":"keep","keep":3,"days":366}`, `{"mode":"archive","keep":3,"days":7,"url":"secret"}`} {
		resp, out := b.do(t, "PUT", base+"retention?path=Fixture+Show", b.adminTok, body)
		if resp.StatusCode != 400 || !strings.Contains(out, "invalid_request") {
			t.Fatalf("policy %d %s", resp.StatusCode, out)
		}
	}
	for _, p := range []string{"../x.mp3", "Fixture Show/.hidden.mp3", "Fixture Show/x.wav", "Fixture Show/a/b.mp3"} {
		resp, out := b.do(t, "POST", base+"fetch", b.adminTok, fmt.Sprintf(`{"path":%q}`, p))
		if resp.StatusCode != 400 {
			t.Fatalf("path %s: %d %s", p, resp.StatusCode, out)
		}
	}
	resp, out := b.do(t, "POST", base+"fetch", b.adminTok, `{"path":"Fixture Show/missing.mp3"}`)
	if resp.StatusCode != 404 || !strings.Contains(out, "episode_not_found") {
		t.Fatalf("missing: %d %s", resp.StatusCode, out)
	}
	resp, out = b.do(t, "GET", base+"retention?path=Missing", b.adminTok, "")
	if resp.StatusCode != 404 || !strings.Contains(out, "show_not_found") {
		t.Fatalf("show: %d %s", resp.StatusCode, out)
	}
	// The aggregate requires the exact indexed generation and identity.
	bridgeBase := fmt.Sprintf("/api/v1/bridge/libraries/%d/podcasts/", b.lib.ID)
	resp, out = b.do(t, "GET", bridgeBase+"retention?path=Fixture+Show&show_id=0123456789abcdef", b.bridgeTok, "")
	if resp.StatusCode != 409 || !strings.Contains(out, "identity_changed") {
		t.Fatalf("identity: %d %s", resp.StatusCode, out)
	}
	for _, payload := range []string{fmt.Sprintf(`{"show_id":%q,"path":%q,"generation":1,"sha256":%q,"bytes":16493}`, f.ShowID, f.Episodes[0].Path, f.Episodes[0].Fetch.SHA256), fmt.Sprintf(`{"show_id":%q,"path":%q,"generation":1,"sha256":%q,"bytes":16403}`, f.ShowID, f.Episodes[1].Path, f.Episodes[1].Fetch.SHA256)} {
		resp, out = b.do(t, "POST", bridgeBase+"retention/check", b.bridgeTok, payload)
		if resp.StatusCode != 409 {
			t.Fatalf("pin: %d %s", resp.StatusCode, out)
		}
	}
	b.do(t, "PATCH", "/api/v1/admin/settings", b.adminTok, `{"modules":{"podcasts":false}}`)
	for _, route := range []struct{ method, url, body string }{{"PUT", base + "retention?path=Fixture+Show", `{"mode":"keep","keep":3,"days":7}`}, {"DELETE", base + "retention?path=Fixture+Show", ""}, {"POST", base + "fetch", fmt.Sprintf(`{"path":%q}`, f.Episodes[2].Path)}} {
		resp, out = b.do(t, route.method, route.url, b.adminTok, route.body)
		if resp.StatusCode != 404 || !strings.Contains(out, "module_off") {
			t.Fatalf("module: %d %s", resp.StatusCode, out)
		}
	}
	resp, out = b.do(t, "GET", base+"retention?path=Fixture+Show", b.adminTok, "")
	if resp.StatusCode != 200 {
		t.Fatalf("core: %d %s", resp.StatusCode, out)
	}
	b.do(t, "PATCH", "/api/v1/admin/settings", b.adminTok, `{"modules":{"podcasts":true}}`)
	b.beat(t, bridge.KindPodcastPoll)
	resp, out = b.do(t, "POST", base+"fetch", b.adminTok, fmt.Sprintf(`{"path":%q}`, f.Episodes[2].Path))
	if resp.StatusCode != 409 || !strings.Contains(out, "bridge_unavailable") {
		t.Fatalf("kind: %d %s", resp.StatusCode, out)
	}
	resp, out = b.do(t, "POST", base+"fetch", b.adminTok, fmt.Sprintf(`{"path":%q}`, f.Episodes[0].Path))
	wantDownloaded(t, "present pin without fetch kind", resp, out)
	b.beat(t, bridge.KindPodcastFetch, bridge.KindPodcastRetain)
	if _, err := b.db.ExecContext(context.Background(), `UPDATE users SET is_demo=1 WHERE id=?`, b.adminID); err != nil {
		t.Fatal(err)
	}
	resp, out = b.do(t, "GET", base+"retention?path=Fixture+Show", b.adminTok, "")
	if resp.StatusCode != 403 {
		t.Fatalf("demo: %d %s", resp.StatusCode, out)
	}
	resp, out = b.do(t, "POST", base+"fetch", b.adminTok, fmt.Sprintf(`{"path":%q}`, f.Episodes[0].Path))
	if resp.StatusCode != 403 {
		t.Fatalf("demo present pin: %d %s", resp.StatusCode, out)
	}
}

func TestPodcastStorageErrorMappers(t *testing.T) {
	b, _ := podcastStorageEnv(t)
	for _, mapper := range []struct {
		name  string
		apply func(*httptest.ResponseRecorder, error) bool
	}{
		{"bridge", func(w *httptest.ResponseRecorder, err error) bool { return b.api.writeBridgeError(w, err, "test") }},
		{"podcast", func(w *httptest.ResponseRecorder, err error) bool { return b.api.podcastQueueError(w, err) }},
	} {
		for _, tc := range []struct {
			err    error
			status int
			code   string
		}{{bridge.ErrNotPodcastLibrary, 409, "not_podcast_library"}, {bridge.ErrNotBookLibrary, 409, "not_book_library"}, {errors.New("SECRET SQLite failure"), 500, ""}} {
			t.Run(mapper.name+tc.err.Error(), func(t *testing.T) {
				w := httptest.NewRecorder()
				if mapper.apply(w, fmt.Errorf("wrapped: %w", tc.err)) || w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) || strings.Contains(w.Body.String(), "SECRET") {
					t.Fatalf("mapped %d %s", w.Code, w.Body.String())
				}
			})
		}
	}
}

func TestPodcastGenerationMediaContract(t *testing.T) {
	b, f := podcastStorageEnv(t)
	ep := f.Episodes[0]
	route := podcastRoute(b.lib.ID, "stream?path="+url.QueryEscape(ep.Path))
	headers := podcastHeaders()
	headers["Range"] = "bytes=1-10"
	etag := fmt.Sprintf(`"podcast-g2-%s"`, ep.Fetch.SHA256)
	headers["If-Range"] = etag
	resp, body := b.doHeaders(t, "GET", route, b.adminTok, "", headers)
	if resp.StatusCode != 206 || len(body) != 10 || resp.Header.Get("ETag") != etag || resp.Header.Get("Accept-Ranges") != "bytes" || resp.Header.Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("range %d %v (%d bytes)", resp.StatusCode, resp.Header, len(body))
	}
	headers["If-Range"] = `"podcast-g1-old"`
	resp, body = b.doHeaders(t, "GET", route, b.adminTok, "", headers)
	if resp.StatusCode != 200 || int64(len(body)) != ep.Fetch.Bytes {
		t.Fatalf("If-Range %d %d", resp.StatusCode, len(body))
	}
	resp, _ = b.doHeaders(t, "GET", route+"&download=1", b.adminTok, "", podcastHeaders())
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatal("download")
	}
	b.do(t, "PATCH", "/api/v1/admin/settings", b.adminTok, `{"modules":{"podcasts":false}}`)
	resp, _ = b.doHeaders(t, "GET", route, b.adminTok, "", podcastHeaders())
	if resp.StatusCode != 200 {
		t.Fatal("ready playback stopped with module")
	}
	resp, body = b.doHeaders(t, "GET", podcastRoute(b.lib.ID, "item?path="+url.QueryEscape(ep.Path)), b.adminTok, "", podcastHeaders())
	var item struct {
		Episode   catalog.PodcastEpisodeInfo `json:"episode"`
		Chapters  []json.RawMessage          `json:"chapters"`
		Generated bool                       `json:"generated"`
		Source    string                     `json:"chapters_source"`
	}
	decodeInto(t, body, &item)
	if resp.StatusCode != 200 || item.Episode.Generation != 2 || item.Episode.SHA256 != ep.Fetch.SHA256 || item.Episode.TimelineChanged == nil || !*item.Episode.TimelineChanged || len(item.Chapters) != 3 || item.Generated || item.Source != "id3_chap" {
		t.Fatalf("item %d %s", resp.StatusCode, body)
	}
	// A publication journal blocks even a ready cached generation.
	journal := filepath.Join(b.lib.Root, "Fixture Show", ".audiosilo", strings.TrimSuffix(path.Base(ep.Path), ".mp3")+".publish.json")
	if err := os.WriteFile(journal, []byte("malformed"), 0600); err != nil {
		t.Fatal(err)
	}
	resp, body = b.doHeaders(t, "GET", route, b.adminTok, "", podcastHeaders())
	if resp.StatusCode != 503 || !strings.Contains(body, "podcast_fetch_unavailable") {
		t.Fatalf("barrier %d %s", resp.StatusCode, body)
	}
}

type podcastStreamObserver struct {
	*httptest.ResponseRecorder
	observe func()
}

func (w *podcastStreamObserver) Write(p []byte) (int, error) {
	w.observe()
	return w.ResponseRecorder.Write(p)
}
func TestPodcastStreamMarksCoverEveryMediaBranch(t *testing.T) {
	b, f := podcastStorageEnv(t)
	ep := f.Episodes[0]
	req := catalog.RetentionRequest{ShowID: f.ShowID, Path: ep.Path, Generation: ep.Fetch.Generation, SHA256: ep.Fetch.SHA256, Bytes: ep.Fetch.Bytes}
	for _, suffix := range []string{"", "&download=1", "&transcode=1", "&transcode=1&t=1", "&transcode=1&download=1"} {
		t.Run(suffix, func(t *testing.T) {
			if strings.HasPrefix(suffix, "&transcode=1") && !media.HasFFmpeg("ffmpeg") {
				t.Skip("ffmpeg not installed")
			}
			b.api.ffmpeg = "ffmpeg"
			observed := false
			w := &podcastStreamObserver{ResponseRecorder: httptest.NewRecorder(), observe: func() {
				check, err := b.cat.PodcastRetentionCheck(context.Background(), b.lib.ID, req)
				if err != nil || !check.ActiveStream {
					t.Errorf("active stream missing: %+v %v", check, err)
				}
				observed = true
			}}
			r := httptest.NewRequest("GET", podcastRoute(b.lib.ID, "stream?path="+url.QueryEscape(ep.Path)+suffix), nil)
			r.Header.Set("Authorization", "Bearer "+b.adminTok)
			r.Header.Set(auth.ClientHeader, "AudioSilo/dev (web)")
			b.api.Handler().ServeHTTP(w, r)
			if w.Code != 200 || !observed || w.Body.Len() == 0 {
				t.Fatalf("media branch: %d %s", w.Code, w.Body.String())
			}
			if strings.HasPrefix(suffix, "&transcode=1") && (w.Header().Get("ETag") != "" || w.Header().Get("Accept-Ranges") != "none") {
				t.Fatal("transformed source validator")
			}
			// A download is an attachment named for the episode, transcoded or not.
			if want := strings.Contains(suffix, "download=1"); want != strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment; filename=") {
				t.Fatalf("Content-Disposition %q, want an attachment: %v", w.Header().Get("Content-Disposition"), want)
			}
			check, err := b.cat.PodcastRetentionCheck(context.Background(), b.lib.ID, req)
			if err != nil || check.ActiveStream {
				t.Fatalf("stream mark leaked: %+v %v", check, err)
			}
		})
	}
}

// getPodcastStream is the admin's GET of stream (an episode's stream route)
// from a client that sees podcasts, answered in process.
func getPodcastStream(b *bridgeEnv, stream string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", stream, nil)
	r.Header.Set("Authorization", "Bearer "+b.adminTok)
	for k, v := range podcastHeaders() {
		r.Header.Set(k, v)
	}
	b.api.Handler().ServeHTTP(w, r)
	return w
}

// An episode's open on a share that stopped answering (OpenPodcast's checks
// and open, SRV-15c) is given up on at the stream's bound: a 503
// share_not_answering with Retry-After, not a handler held for good; once the
// share answers the episode streams. Not parallel: metadata.HoldOpensForTests
// swaps the mount lookup.
func TestPodcastStreamOnAHeldShareAnswers(t *testing.T) {
	b, f := podcastStorageEnv(t)
	b.api.streamWait = 200 * time.Millisecond
	u := podcastRoute(b.lib.ID, "stream?path="+url.QueryEscape(f.Episodes[0].Path))
	release := metadata.HoldOpensForTests(filepath.Join(b.lib.Root, "Fixture Show"))
	t.Cleanup(release)
	get := func() *httptest.ResponseRecorder { return getPodcastStream(b, u) }
	start := time.Now()
	if w := get(); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), codeShareNotAnswering) || w.Header().Get("Retry-After") != "30" {
		t.Errorf("an episode on a held share = %d %s, want 503 %s", w.Code, w.Body, codeShareNotAnswering)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the stream took %v, want it answered at the open's bound", took)
	}
	release()
	b.api.streamWait = 10 * time.Second
	if w := get(); w.Code != http.StatusOK || w.Body.Len() == 0 {
		t.Errorf("the episode once the share answers = %d %s, want it streamed", w.Code, w.Body)
	}
}

// TestPodcastStreamRefusedFolderIs404 (seen live): an episode whose show
// folder the share refuses for good (chmod 000: a permission, a lasting
// fault) is the 404 an episode gone is on the stream, and no fetch is asked
// of the Bridge, which would meet the same refusal (it was a fetch, and with
// a feed's episode ids it couldn't queue, a 500). Allowed: the folder
// readable again, the episode streams; an episode file gone is still fetched
// again. Not parallel only for the chmod: the folder is this test's own.
func TestPodcastStreamRefusedFolderIs404(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a folder whatever its mode")
	}
	b, f := podcastStorageEnv(t)
	dir := filepath.Join(b.lib.Root, "Fixture Show")
	ep := f.Episodes[0]
	stream := podcastRoute(b.lib.ID, "stream?path="+url.QueryEscape(ep.Path))
	get := func() *httptest.ResponseRecorder { return getPodcastStream(b, stream) }
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if w := get(); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "episode_not_found") {
		t.Errorf("an episode in a folder refused for good = %d %s, want 404 episode_not_found", w.Code, w.Body)
	}
	if n := podcastFetches(t, b); n != 0 {
		t.Errorf("%d fetches asked of the Bridge for an episode the share refuses, want none", n)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if w := get(); w.Code != http.StatusOK || w.Body.Len() == 0 {
		t.Errorf("the episode once its folder reads = %d %s, want it streamed", w.Code, w.Body)
	}
	if err := os.Remove(filepath.Join(b.lib.Root, filepath.FromSlash(ep.Path))); err != nil {
		t.Fatal(err)
	}
	if w := get(); w.Code == http.StatusNotFound || podcastFetches(t, b) == 0 {
		t.Errorf("an episode file gone = %d %s with %d fetches, want it fetched again", w.Code, w.Body, podcastFetches(t, b))
	}
}

// podcastFetches is how many episode fetches have been asked of the Bridge.
func podcastFetches(t *testing.T, b *bridgeEnv) (n int) {
	t.Helper()
	if err := b.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM bridge_jobs WHERE kind = ?`, bridge.KindPodcastFetch).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestPodcastAdminDownloadFetchesARefusedEpisode: an episode whose file the
// share refuses the server for good (chmod 000) is the stream's 404 with no
// fetch queued, but an admin's download of it is asked of the Bridge all the
// same (202, one fetch), which writes a new file in its place; the stream
// after it is still the 404 and queues none of its own. Denied the fetch: the
// stream, before and after. Not parallel only for the chmod: the file is this
// test's own.
func TestPodcastAdminDownloadFetchesARefusedEpisode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	b, f := podcastStorageEnv(t)
	ep := f.Episodes[0]
	file := filepath.Join(b.lib.Root, filepath.FromSlash(ep.Path))
	if err := os.Chmod(file, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(file, 0o644) })
	stream := podcastRoute(b.lib.ID, "stream?path="+url.QueryEscape(ep.Path))
	if w := getPodcastStream(b, stream); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "episode_not_found") || podcastFetches(t, b) != 0 {
		t.Fatalf("the stream of an episode whose file is refused for good = %d %s with %d fetches, want 404 episode_not_found and none",
			w.Code, w.Body, podcastFetches(t, b))
	}
	resp, out := b.do(t, "POST", fmt.Sprintf("/api/v1/admin/libraries/%d/podcasts/fetch", b.lib.ID), b.adminTok, fmt.Sprintf(`{"path":%q}`, ep.Path))
	if resp.StatusCode != http.StatusAccepted || podcastFetches(t, b) != 1 {
		t.Fatalf("an admin's download of that episode = %d %s with %d fetches, want 202 and its fetch", resp.StatusCode, out, podcastFetches(t, b))
	}
	if w := getPodcastStream(b, stream); w.Code != http.StatusNotFound || podcastFetches(t, b) != 1 {
		t.Errorf("the stream after that download = %d %s with %d fetches, want 404 and none of its own", w.Code, w.Body, podcastFetches(t, b))
	}
}

// TestPodcastListenerFetchQueuesARefusedEpisode: a listener's fetch routes
// read an episode whose file the share refuses for good (chmod 000) as absent,
// as they did before the stream told the two apart (shipped players use them):
// GET answers its fetch status (200, nothing queued) and POST queues its fetch
// (202, one job), while the stream is the 404 and queues none, before and
// after. Not parallel only for the chmod: the file is this test's own.
func TestPodcastListenerFetchQueuesARefusedEpisode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	b, f := podcastStorageEnv(t)
	ep := f.Episodes[0].Path
	file := filepath.Join(b.lib.Root, filepath.FromSlash(ep))
	if err := os.Chmod(file, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(file, 0o644) })
	stream := podcastRoute(b.lib.ID, "stream?path="+url.QueryEscape(ep))
	if w := getPodcastStream(b, stream); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "episode_not_found") || podcastFetches(t, b) != 0 {
		t.Fatalf("the stream of an episode whose file is refused for good = %d %s with %d fetches, want 404 episode_not_found and none",
			w.Code, w.Body, podcastFetches(t, b))
	}
	fetchState := func(method string, want int) string {
		t.Helper()
		resp, out := b.doHeaders(t, method, fetchRoute(b.lib.ID, ep), b.userTok, fetchBody(ep), podcastHeaders())
		var got struct {
			Fetch *bridge.FetchStatus `json:"fetch"`
		}
		if resp.StatusCode != want || json.Unmarshal([]byte(out), &got) != nil || got.Fetch == nil {
			t.Fatalf("a listener's %s fetch of that episode = %d %s, want %d and its fetch status", method, resp.StatusCode, out, want)
		}
		return got.Fetch.State
	}
	if st := fetchState("GET", http.StatusOK); st == "ready" || podcastFetches(t, b) != 0 {
		t.Fatalf("a listener's GET of that episode = state %q with %d fetches, want it not ready and none queued", st, podcastFetches(t, b))
	}
	if st := fetchState("POST", http.StatusAccepted); st == "ready" || podcastFetches(t, b) != 1 {
		t.Fatalf("a listener's POST of that episode = state %q with %d fetches, want its fetch queued", st, podcastFetches(t, b))
	}
	if st := fetchState("GET", http.StatusOK); st != "queued" {
		t.Errorf("a listener's GET after that POST = state %q, want queued", st)
	}
	if w := getPodcastStream(b, stream); w.Code != http.StatusNotFound || podcastFetches(t, b) != 1 {
		t.Errorf("the stream after that fetch = %d %s with %d fetches, want 404 and none of its own", w.Code, w.Body, podcastFetches(t, b))
	}
}

// The download's presence check (podcastPresent: OpenPodcast's checks and
// open) on a share that stopped answering is a bounded look too (fileWait): a
// 503 share_not_answering, where it was a plain open that held the handler
// for good; once the share answers the episode reads as downloaded. Not
// parallel: metadata.HoldOpensForTests swaps the mount lookup.
func TestPodcastDownloadOnAHeldShareAnswers(t *testing.T) {
	b, f := podcastStorageEnv(t)
	b.api.fileWait = 200 * time.Millisecond
	route := fmt.Sprintf("/api/v1/admin/libraries/%d/podcasts/fetch", b.lib.ID)
	body := fmt.Sprintf(`{"path":%q}`, f.Episodes[0].Path)
	release := metadata.HoldOpensForTests(filepath.Join(b.lib.Root, "Fixture Show"))
	t.Cleanup(release)
	start := time.Now()
	if resp, out := b.do(t, "POST", route, b.adminTok, body); resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(out, codeShareNotAnswering) {
		t.Errorf("a download on a held share = %d %s, want 503 %s", resp.StatusCode, out, codeShareNotAnswering)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the download took %v, want it answered at the open's bound", took)
	}
	release()
	resp, out := b.do(t, "POST", route, b.adminTok, body)
	wantDownloaded(t, "the episode once the share answers", resp, out)
}

func TestPodcastStorageMappingLibraryAndFailureErrors(t *testing.T) {
	b, f := podcastStorageEnv(t)
	ctx := context.Background()
	base := fmt.Sprintf("/api/v1/admin/libraries/%d/podcasts/", b.lib.ID)
	beat := fmt.Sprintf(`{"version":"0.9.0","kinds":["podcast.fetch","podcast.retain"],"libraries":[{"library_id":%d,"mount_path":"/podcasts","mapped":true,"writable":false}]}`, b.lib.ID)
	resp, out := b.do(t, "PUT", "/api/v1/bridge/status", b.bridgeTok, beat)
	if resp.StatusCode != 200 {
		t.Fatal(out)
	}
	resp, out = b.do(t, "POST", base+"fetch", b.adminTok, fmt.Sprintf(`{"path":%q}`, f.Episodes[0].Path))
	wantDownloaded(t, "present pin without writable mapping", resp, out)
	for _, route := range []struct{ method, url, body string }{{"PUT", base + "retention?path=Fixture+Show", `{"mode":"keep","keep":3,"days":7}`}, {"DELETE", base + "retention?path=Fixture+Show", ""}, {"POST", base + "fetch", fmt.Sprintf(`{"path":%q}`, f.Episodes[2].Path)}} {
		resp, out = b.do(t, route.method, route.url, b.adminTok, route.body)
		if resp.StatusCode != 409 || !strings.Contains(out, "library_not_writable") {
			t.Fatalf("mapping: %d %s", resp.StatusCode, out)
		}
	}
	var n int
	if err := b.db.QueryRowContext(ctx, `SELECT count(*) FROM bridge_jobs WHERE kind IN ('podcast.fetch','podcast.retain')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("denial queued work: %d %v", n, err)
	}
	books, err := b.cat.CreateLibrary(ctx, catalog.Library{Name: "Other books", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{fmt.Sprintf("/api/v1/admin/libraries/%d/podcasts/", books.ID), fmt.Sprintf("/api/v1/bridge/libraries/%d/podcasts/", books.ID)} {
		token := b.adminTok
		if strings.Contains(root, "/bridge/") {
			token = b.bridgeTok
		}
		resp, out = b.do(t, "GET", root+"retention?path=Show&show_id="+f.ShowID, token, "")
		if resp.StatusCode != 409 || !strings.Contains(out, "not_podcast_library") {
			t.Fatalf("kind: %d %s", resp.StatusCode, out)
		}
	}
	resp, out = b.do(t, "GET", "/api/v1/admin/libraries/99999/podcasts/retention?path=Show", b.adminTok, "")
	if resp.StatusCode != 404 || !strings.Contains(out, "library_not_found") {
		t.Fatalf("library: %d %s", resp.StatusCode, out)
	}
	b.beat(t, bridge.KindPodcastFetch, bridge.KindPodcastRetain)
	if _, err = b.db.ExecContext(ctx, `DROP TABLE podcast_policies`); err != nil {
		t.Fatal(err)
	}
	resp, out = b.do(t, "GET", base+"retention?path=Fixture+Show", b.adminTok, "")
	if resp.StatusCode != 500 || strings.Contains(out, "SQL") || strings.Contains(out, "podcast_policies") {
		t.Fatalf("sanitized failure: %d %s", resp.StatusCode, out)
	}
}
