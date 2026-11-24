package api

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/bridge"
)

// A follow by URL may name the show (v2.7.0, fe#126): the request row carries
// that title, cleaned to one line, never a private feed's; a title beside a
// path is refused.
func TestPodcastFollowRequestTitle(t *testing.T) {
	b, _, _ := podcastEnv(t, false)
	h := podcastHeaders()
	follow := func(body string) (int, string) {
		t.Helper()
		resp, out := b.doHeaders(t, "POST", "/api/v1/podcasts/follow", b.userTok, body, h)
		return resp.StatusCode, out
	}
	code, out := follow(fmt.Sprintf(`{"library_id":%d,"feed_url":"https://feeds.example.com/new.xml","title":"  New\nShow  "}`, b.lib.ID))
	if code != 202 || !strings.Contains(out, `"title":"New Show"`) {
		t.Fatalf("titled follow %d %s", code, out)
	}
	code, out = follow(fmt.Sprintf(`{"library_id":%d,"feed_url":%q,"title":"Members only"}`, b.lib.ID, privateFeed))
	if code != 202 || strings.Contains(out, "Members only") || !strings.Contains(out, `"title":null`) {
		t.Fatalf("private follow %d %s", code, out)
	}
	if code, out = follow(fmt.Sprintf(`{"library_id":%d,"path":"Example Show","title":"x"}`, b.lib.ID)); code != 400 {
		t.Fatalf("title beside a path %d %s", code, out)
	}
	resp, out := b.doHeaders(t, "GET", "/api/v1/me/podcasts", b.userTok, "", h)
	if resp.StatusCode != 200 || !strings.Contains(out, `"title":"New Show"`) || strings.Contains(out, "Members only") {
		t.Fatalf("requests %d %s", resp.StatusCode, out)
	}
}

// DELETE /podcasts/requests/{id}: the caller's own failed request goes
// (allowed); an open one is 409, another user's or a gone one 404, an old
// client 403 (denied). It works with the module off.
func TestPodcastRequestDismiss(t *testing.T) {
	b, uid, _ := podcastEnv(t, false)
	h := podcastHeaders()
	resp, out := b.doHeaders(t, "POST", "/api/v1/podcasts/follow", b.userTok, fmt.Sprintf(`{"library_id":%d,"feed_url":"https://feeds.example.com/broken.xml"}`, b.lib.ID), h)
	if resp.StatusCode != 202 {
		t.Fatalf("follow %d %s", resp.StatusCode, out)
	}
	var answer struct{ Request struct{ ID int64 } }
	decodeInto(t, out, &answer)
	id := answer.Request.ID
	route := fmt.Sprintf("/api/v1/podcasts/requests/%d", id)
	dismiss := func(tok string, headers map[string]string, status int, code string) {
		t.Helper()
		resp, out := b.doHeaders(t, "DELETE", route, tok, "", headers)
		if resp.StatusCode != status || code != "" && !strings.Contains(out, `"code":"`+code+`"`) {
			t.Fatalf("dismiss %d %s, want %d %s", resp.StatusCode, out, status, code)
		}
	}
	dismiss(b.userTok, h, 409, "request_open")

	resp, out = b.do(t, "GET", "/api/v1/bridge/jobs/next?kind=podcast.subscribe", b.bridgeTok, "")
	var claim bridge.Job
	decodeInto(t, out, &claim)
	if resp.StatusCode != 200 || claim.ID != id {
		t.Fatalf("claim %d %s", resp.StatusCode, out)
	}
	resp, out = b.do(t, "PATCH", fmt.Sprintf("/api/v1/bridge/jobs/%d", id), b.bridgeTok, fmt.Sprintf(`{"attempt":%d,"state":"failed","result":{"code":"not_a_feed","message":"no"}}`, claim.Attempts))
	if resp.StatusCode != 200 {
		t.Fatalf("fail %d %s", resp.StatusCode, out)
	}

	_, bobTok := newUserWithSession(t, b.testEnv, "bob", false)
	dismiss(bobTok, h, 404, "request_not_found")
	// The request's owner, from a player older than podcasts.
	old, err := b.auth.IssueToken(context.Background(), uid, auth.KindSession, "old", 0)
	if err != nil {
		t.Fatal(err)
	}
	dismiss(old, map[string]string{auth.ClientHeader: "AudioSilo/2.1.0 (web)"}, 403, "no_library_access")
	podcastsModule(t, b, false)
	dismiss(b.userTok, h, 204, "")
	dismiss(b.userTok, h, 404, "request_not_found")
	var n int
	if err := b.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM bridge_jobs WHERE id=?`, id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("row left %d %v", n, err)
	}
	resp, out = b.doHeaders(t, "GET", "/api/v1/me/podcasts", b.userTok, "", h)
	if resp.StatusCode != 200 || strings.Contains(out, "broken") {
		t.Fatalf("listed after dismiss %d %s", resp.StatusCode, out)
	}
	for _, bad := range []string{"/api/v1/podcasts/requests/0", "/api/v1/podcasts/requests/x"} {
		resp, out = b.doHeaders(t, "DELETE", bad, b.userTok, "", h)
		if resp.StatusCode != 404 {
			t.Fatalf("%s %d %s", bad, resp.StatusCode, out)
		}
	}
}

// An OPML import's new public requests carry their outline's title (fe#126),
// a request without one null; the rows themselves never echo the document.
func TestOPMLImportRequestTitles(t *testing.T) {
	b, _, _ := podcastEnv(t, false)
	doc := `<?xml version="1.0" encoding="UTF-8"?><opml version="2.0"><body>` +
		`<outline type="rss" text="Listed Show" xmlUrl="https://feeds.example.com/listed.xml"/>` +
		`<outline type="rss" xmlUrl="https://feeds.example.com/untitled.xml"/>` +
		`</body></opml>`
	h := map[string]string{"Content-Type": "text/x-opml", auth.ClientHeader: podcastHeaders()[auth.ClientHeader]}
	resp, out := b.doHeaders(t, "POST", fmt.Sprintf("/api/v1/podcasts/opml/import?library_id=%d", b.lib.ID), b.userTok, doc, h)
	if resp.StatusCode != 200 || strings.Contains(out, "Listed Show") || strings.Count(out, `"status":"queued"`) != 2 {
		t.Fatalf("import %d %s", resp.StatusCode, out)
	}
	resp, out = b.doHeaders(t, "GET", "/api/v1/me/podcasts", b.userTok, "", podcastHeaders())
	var mine struct {
		Requests []struct {
			Title *string `json:"title"`
		}
	}
	decodeInto(t, out, &mine)
	var titles []string
	for _, r := range mine.Requests {
		if r.Title == nil {
			titles = append(titles, "<null>")
		} else {
			titles = append(titles, *r.Title)
		}
	}
	// Newest first.
	if resp.StatusCode != 200 || strings.Join(titles, "|") != "<null>|Listed Show" {
		t.Fatalf("requests %d %v %s", resp.StatusCode, titles, out)
	}
}
