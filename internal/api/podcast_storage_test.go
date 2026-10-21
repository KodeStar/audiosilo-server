package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
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
	if resp.StatusCode != 200 || strings.TrimSpace(body) != `{"downloaded":true}` {
		t.Fatalf("present pin %d %s", resp.StatusCode, body)
	}
	for _, kind := range []string{bridge.KindPodcastFetch, bridge.KindPodcastRetain} {
		resp, body = b.do(t, "POST", "/api/v1/admin/bridge/jobs", b.adminTok, fmt.Sprintf(`{"kind":%q,"library_id":%d,"path":"Fixture Show","params":{}}`, kind, b.lib.ID))
		if resp.StatusCode != 400 {
			t.Fatalf("direct internal job: %d %s", resp.StatusCode, body)
		}
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
	b.beat(t, bridge.KindPodcastFetch, bridge.KindPodcastRetain)
	if _, err := b.db.ExecContext(context.Background(), `UPDATE users SET is_demo=1 WHERE id=?`, b.adminID); err != nil {
		t.Fatal(err)
	}
	resp, out = b.do(t, "GET", base+"retention?path=Fixture+Show", b.adminTok, "")
	if resp.StatusCode != 403 {
		t.Fatalf("demo: %d %s", resp.StatusCode, out)
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
	if resp.StatusCode != 404 || !strings.Contains(body, "not_downloaded") {
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
	for _, suffix := range []string{"", "&download=1", "&transcode=1"} {
		t.Run(suffix, func(t *testing.T) {
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
			if suffix == "&transcode=1" && (w.Header().Get("ETag") != "" || w.Header().Get("Accept-Ranges") != "none") {
				t.Fatal("transformed source validator")
			}
			check, err := b.cat.PodcastRetentionCheck(context.Background(), b.lib.ID, req)
			if err != nil || check.ActiveStream {
				t.Fatalf("stream mark leaked: %+v %v", check, err)
			}
		})
	}
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
