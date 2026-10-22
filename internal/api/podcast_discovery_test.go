package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/podcast/discovery"
)

const phase2c3 = "../podcast/testdata/phase2c3/"

// fakeUpstream answers the directories and artwork in memory, counting every
// request: a denial must reach none of them.
type fakeUpstream struct {
	t    *testing.T
	mu   sync.Mutex
	hits []string
}

func (f *fakeUpstream) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.hits = append(f.hits, r.URL.Host+r.URL.Path)
	f.mu.Unlock()
	var body []byte
	ctype := "application/json"
	switch r.URL.Host {
	case "itunes.apple.com":
		body = readFixture(f.t, phase2c3+"providers/itunes-search.json")
	case "api.podcastindex.org":
		body = readFixture(f.t, phase2c3+"providers/podcast-index-search.json")
	default:
		var buf bytes.Buffer
		if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
			f.t.Error(err)
		}
		body, ctype = buf.Bytes(), "image/png"
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {ctype}}, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: r}, nil
}

func (f *fakeUpstream) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.hits)
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// discoveryEnv is podcastEnv with discovery answering from fakeUpstream.
func discoveryEnv(t *testing.T) (*bridgeEnv, int64, *fakeUpstream) {
	t.Helper()
	b, uid, _ := podcastEnv(t, false)
	up := &fakeUpstream{t: t}
	b.api.discovery = discovery.New(discovery.Options{
		Version: "test", IndexKey: "fixture-key", IndexSecret: "fixture-secret",
		Enabled:   func() bool { return b.api.moduleOn(bridge.ModulePodcasts) },
		Transport: up, ArtTransport: up,
	})
	return b, uid, up
}

func wantErr(t *testing.T, resp *http.Response, body string, status int, code string) {
	t.Helper()
	if resp.StatusCode != status || code != "" && !strings.Contains(body, `"code":"`+code+`"`) {
		t.Fatalf("want %d %s, got %d %s", status, code, resp.StatusCode, body)
	}
}

func TestDiscoverySearchAndArtwork(t *testing.T) {
	b, _, up := discoveryEnv(t)
	h := podcastHeaders()
	resp, body := b.doHeaders(t, "GET", "/api/v1/podcasts/discovery/providers", b.userTok, "", h)
	wantErr(t, resp, body, 200, "")
	if resp.Header.Get("Cache-Control") != "no-store" || !strings.Contains(body, `{"id":"itunes","available":true}`) || !strings.Contains(body, `{"id":"podcast_index","available":true}`) {
		t.Fatalf("providers %v %s", resp.Header, body)
	}
	resp, body = b.doHeaders(t, "GET", "/api/v1/podcasts/discovery/search?q=fixture&provider=itunes", b.userTok, "", h)
	wantErr(t, resp, body, 200, "")
	if resp.Header.Get("Cache-Control") != "no-store" || up.count() != 1 {
		t.Fatalf("search %v hits %d", resp.Header, up.count())
	}
	var res discovery.Search
	decodeInto(t, body, &res)
	var art string
	for _, r := range res.Results {
		if r.ArtworkURL != nil {
			art = *r.ArtworkURL
			break
		}
	}
	if !strings.HasPrefix(art, discovery.ArtworkPath) {
		t.Fatalf("no proxied artwork in %s", body)
	}
	resp, body = b.doHeaders(t, "GET", art, b.userTok, "", h)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/jpeg" || resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Cache-Control") != "private, max-age=300" {
		t.Fatalf("artwork %d %v %.80s", resp.StatusCode, resp.Header, body)
	}
	// An <img> source: the session's ?token= works on the artwork alone.
	resp, _ = b.do(t, "GET", art+"&token="+url.QueryEscape(b.userTok), "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("artwork by query token %d", resp.StatusCode)
	}
	before := up.count()
	resp, _ = b.do(t, "GET", "/api/v1/podcasts/discovery/search?q=fixture&token="+url.QueryEscape(b.userTok), "", "")
	if resp.StatusCode != 401 {
		t.Fatalf("search by query token %d", resp.StatusCode)
	}
	ctx := context.Background()
	pairing, err := b.auth.IssueToken(ctx, b.adminID, auth.KindPairing, "pair", 0)
	if err != nil {
		t.Fatal(err)
	}
	cast, err := b.auth.IssueToken(ctx, b.adminID, auth.KindCast, "cast", 0)
	if err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{"bridge": b.bridgeTok, "pairing": pairing, "cast": cast, "bogus": "not-a-token"} {
		for _, route := range []string{art, "/api/v1/podcasts/discovery/search?q=x", "/api/v1/podcasts/discovery/providers"} {
			resp, body = b.doHeaders(t, "GET", route, tok, "", h)
			if resp.StatusCode != 401 {
				t.Fatalf("%s on %s: %d %s", name, route, resp.StatusCode, body)
			}
		}
		resp, _ = b.do(t, "GET", art+"&token="+url.QueryEscape(tok), "", "")
		if resp.StatusCode != 401 {
			t.Fatalf("%s query token on artwork: %d", name, resp.StatusCode)
		}
	}
	// HEAD is refused before anything runs.
	for _, route := range []string{"/api/v1/podcasts/discovery/providers", "/api/v1/podcasts/discovery/search?q=new", art, "/api/v1/podcasts/opml/export"} {
		resp, _ = b.doHeaders(t, "HEAD", route, b.userTok, "", h)
		if resp.StatusCode != 405 {
			t.Fatalf("HEAD %s %d", route, resp.StatusCode)
		}
	}
	// Unknown and repeated parameters.
	for _, route := range []string{"/api/v1/podcasts/discovery/search?q=new&user_id=1", "/api/v1/podcasts/discovery/search?q=new&q=other", "/api/v1/podcasts/discovery/providers?x=1", art + "&id=again", art + "&size=100"} {
		resp, body = b.doHeaders(t, "GET", route, b.userTok, "", h)
		wantErr(t, resp, body, 400, "")
	}
	resp, body = b.doHeaders(t, "GET", "/api/v1/podcasts/discovery/artwork?id="+strings.Repeat("a", 64), b.userTok, "", h)
	wantErr(t, resp, body, 404, "")
	if up.count() != before {
		t.Fatalf("denied requests reached upstream: %d", up.count()-before)
	}
}

func TestDiscoveryDeniedMatrix(t *testing.T) {
	b, _, up := discoveryEnv(t)
	h := podcastHeaders()
	ctx := context.Background()
	_, demoTok := newUserWithSession(t, b.testEnv, "demo-disc", true)
	kid, kidTok := newUserWithSession(t, b.testEnv, "kid-disc", false)
	if _, err := b.db.ExecContext(ctx, `UPDATE users SET managed_by=?,member_kind='kid',member_access='all' WHERE id=?`, b.adminID, kid); err != nil {
		t.Fatal(err)
	}
	search := "/api/v1/podcasts/discovery/search?q=fixture"
	for _, tc := range []struct {
		name, tok string
		headers   map[string]string
		status    int
		code      string
	}{
		{"demo", demoTok, h, 403, "demo_account"},
		{"kid", kidTok, h, 403, "kid_account"},
		{"old client", b.userTok, map[string]string{auth.ClientHeader: "AudioSilo/2.1.0 (web)"}, 403, "no_library_access"},
		{"old admin", b.adminTok, map[string]string{auth.ClientHeader: "AudioSilo/2.1.0 (web)"}, 403, "no_library_access"},
		{"anonymous", "", h, 401, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, route := range []string{search, "/api/v1/podcasts/discovery/providers"} {
				resp, body := b.doHeaders(t, "GET", route, tc.tok, "", tc.headers)
				wantErr(t, resp, body, tc.status, tc.code)
			}
		})
	}
	// The module off: 404 module_off, the flag false, nothing fetched.
	_, _ = b.do(t, "PATCH", "/api/v1/admin/settings", b.adminTok, `{"modules":{"podcasts":false}}`)
	resp, body := b.doHeaders(t, "GET", search, b.userTok, "", h)
	wantErr(t, resp, body, 404, "module_off")
	if caps := serverCapabilities(t, b.testEnv); caps["podcast_discovery"] || !caps["podcast_opml"] {
		t.Fatalf("module off caps %v", caps)
	}
	_, _ = b.do(t, "PATCH", "/api/v1/admin/settings", b.adminTok, `{"modules":{"podcasts":true}}`)
	if caps := serverCapabilities(t, b.testEnv); !caps["podcast_discovery"] || !caps["podcast_opml"] {
		t.Fatalf("module on caps %v", caps)
	}
	if up.count() != 0 {
		t.Fatalf("denials reached upstream %d", up.count())
	}
	// Discovery off at start: 404 discovery_off, the flag false, the OPML
	// transfer unaffected.
	b.api.discovery = nil
	resp, body = b.doHeaders(t, "GET", search, b.userTok, "", h)
	wantErr(t, resp, body, 404, "discovery_off")
	if caps := serverCapabilities(t, b.testEnv); caps["podcast_discovery"] || !caps["podcast_opml"] {
		t.Fatalf("discovery off caps %v", caps)
	}
	resp, body = b.doHeaders(t, "GET", "/api/v1/podcasts/opml/export", b.userTok, "", h)
	wantErr(t, resp, body, 200, "")
}

// discovery.enabled false in the config builds no service; a bad Podcast
// Index pair turns that provider off with a warning that never names a value.
func TestDiscoveryBootConfig(t *testing.T) {
	off := newTestEnvWith(t, func(c *config.Config) { c.Podcasts.Discovery.Enabled = false })
	if off.api.discovery != nil {
		t.Fatal("discovery built while disabled")
	}
	on := newTestEnvWith(t, func(c *config.Config) {
		c.Podcasts.Discovery.IndexKey = "KEY_CANARY_2C3"
		c.Podcasts.Discovery.IndexSecret = ""
	})
	if on.api.discovery == nil {
		t.Fatal("discovery not built")
	}
	for _, p := range on.api.discovery.Providers() {
		if p.ID == discovery.PodcastIndex && p.Available {
			t.Fatal("a half pair left Podcast Index on")
		}
	}
	// Settings never take or echo the pair.
	adminTok, err := on.auth.IssueToken(context.Background(), on.adminID, auth.KindSession, "console", 0)
	if err != nil {
		t.Fatal(err)
	}
	resp, body := on.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"podcasts":{"index_key":"KEY_CANARY_2C3"}}`)
	if resp.StatusCode == 200 || strings.Contains(body, "KEY_CANARY_2C3") {
		t.Fatalf("settings patch %d %s", resp.StatusCode, body)
	}
	_, body = on.do(t, "GET", "/api/v1/admin/settings", adminTok, "")
	if strings.Contains(body, "KEY_CANARY_2C3") || strings.Contains(body, "index_key") {
		t.Fatalf("settings echo the pair: %s", body)
	}
}

// opmlPost uploads an OPML document, fresh budgets first unless keep.
func opmlPost(t *testing.T, b *bridgeEnv, tok string, lib int64, body, ctype string, extra map[string]string) (*http.Response, string) {
	t.Helper()
	b.api.opml = newOPMLTransfers()
	h := podcastHeaders()
	h["Content-Type"] = ctype
	for k, v := range extra {
		h[k] = v
	}
	return b.doHeaders(t, "POST", fmt.Sprintf("/api/v1/podcasts/opml/import?library_id=%d", lib), tok, body, h)
}

func openSubscribes(t *testing.T, b *bridgeEnv) int {
	t.Helper()
	var n int
	if err := b.db.QueryRowContext(context.Background(), `SELECT count(*) FROM bridge_jobs WHERE kind=? AND state IN ('queued','running','parked')`, bridge.KindPodcastSubscribe).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOPMLImport(t *testing.T) {
	b, _, _ := podcastEnv(t, false)
	capDoc := string(readFixture(t, phase2c3+"opml/queue-cap.opml"))
	resp, body := opmlPost(t, b, b.userTok, b.lib.ID, capDoc, "text/x-opml", nil)
	wantErr(t, resp, body, 200, "")
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("import cacheable")
	}
	var res struct {
		LibraryID int64 `json:"library_id"`
		Rows      []struct {
			OutlineIndex int     `json:"outline_index"`
			Status       string  `json:"status"`
			Feed         *string `json:"feed"`
			RequestID    *int64  `json:"request_id"`
			Error        *struct{ Code string }
		} `json:"rows"`
	}
	decodeInto(t, body, &res)
	statuses := func() string {
		var s []string
		for _, r := range res.Rows {
			s = append(s, r.Status)
		}
		return strings.Join(s, ",")
	}
	if res.LibraryID != b.lib.ID || statuses() != "queued,queued,queued,queued,queued,limited,limited" || res.Rows[6].Error.Code != "too_many_requests" {
		t.Fatalf("first wave %s", body)
	}
	if openSubscribes(t, b) != 5 {
		t.Fatal("open subscribes")
	}
	// The retry adopts the same five; the single follow is capped too.
	resp, body = opmlPost(t, b, b.userTok, b.lib.ID, capDoc, "application/xml; charset=UTF-8", nil)
	wantErr(t, resp, body, 200, "")
	decodeInto(t, body, &res)
	if statuses() != "already_queued,already_queued,already_queued,already_queued,already_queued,limited,limited" {
		t.Fatalf("retry %s", body)
	}
	resp, body = b.doHeaders(t, "POST", "/api/v1/podcasts/follow", b.userTok, fmt.Sprintf(`{"library_id":%d,"feed_url":"https://feeds.example.org/other.xml"}`, b.lib.ID), podcastHeaders())
	wantErr(t, resp, body, 429, "too_many_requests")
	if openSubscribes(t, b) != 5 {
		t.Fatal("the cap was exceeded")
	}
	// The known show follows at once, cap or not, Bridge or not.
	b.beat(t, bridge.KindVerify)
	known := `<?xml version="1.0"?><opml version="1.0"><body><outline type="rss" xmlUrl="https://feeds.example.com/show.xml"/><outline type="rss" xmlUrl="https://feeds.example.org/brand-new.xml"/></body></opml>`
	resp, body = opmlPost(t, b, b.userTok, b.lib.ID, known, "text/xml", nil)
	wantErr(t, resp, body, 200, "")
	decodeInto(t, body, &res)
	if statuses() != "followed,unavailable" || res.Rows[1].Error.Code != "bridge_unavailable" {
		t.Fatalf("offline %s", body)
	}
}

func TestOPMLImportPrivateRowsStayInMemory(t *testing.T) {
	b, _, _ := podcastEnv(t, false)
	doc := string(readFixture(t, phase2c3+"opml/duplicates-private.opml"))
	resp, body := opmlPost(t, b, b.userTok, b.lib.ID, doc, "text/x-opml", nil)
	wantErr(t, resp, body, 200, "")
	if strings.Contains(body, "OPML_PRIVATE_CANARY_2C3") || strings.Contains(body, "/etc/hosts") || strings.Contains(body, "Fixture") {
		t.Fatalf("the result echoes the document: %s", body)
	}
	var params string
	if err := b.db.QueryRowContext(context.Background(), `SELECT group_concat(params) FROM bridge_jobs`).Scan(&params); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(params, "OPML_PRIVATE_CANARY_2C3") {
		t.Fatal("a private URL reached bridge_jobs")
	}
}

func TestOPMLImportRefusals(t *testing.T) {
	b, uid, _ := podcastEnv(t, false)
	ctx := context.Background()
	valid := string(readFixture(t, phase2c3+"opml/valid-v2.opml"))
	for _, tc := range []struct {
		name, body, ctype string
		extra             map[string]string
		status            int
		code              string
	}{
		{"plain text", valid, "text/plain", nil, 415, "unsupported_opml_type"},
		{"json", valid, "application/json", nil, 415, "unsupported_opml_type"},
		{"latin-1", valid, "text/xml; charset=iso-8859-1", nil, 415, "unsupported_opml_type"},
		{"multipart", valid, "multipart/form-data; boundary=x", nil, 415, "unsupported_opml_type"},
		{"gzip", valid, "text/xml", map[string]string{"Content-Encoding": "gzip"}, 415, "unsupported_opml_type"},
		{"entity bomb", string(readFixture(t, phase2c3+"opml/entity-bomb.opml")), "text/xml", nil, 400, "invalid_opml"},
		{"external entity", string(readFixture(t, phase2c3+"opml/external-entity.opml")), "text/xml", nil, 400, "invalid_opml"},
		{"duplicate attribute", string(readFixture(t, phase2c3+"opml/duplicate-attribute.opml")), "text/xml", nil, 400, "invalid_opml"},
		{"malformed tail", string(readFixture(t, phase2c3+"opml/malformed-tail.opml")), "text/xml", nil, 400, "invalid_opml"},
		{"huge", string(readFixture(t, phase2c3+"opml/huge.opml")), "text/xml", nil, 413, "opml_limit"},
		{"outline cap", string(readFixture(t, phase2c3+"opml/outline-cap.opml")), "text/xml", nil, 413, "opml_limit"},
		{"deep", string(readFixture(t, phase2c3+"opml/deep.opml")), "text/xml", nil, 413, "opml_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := opmlPost(t, b, b.userTok, b.lib.ID, tc.body, tc.ctype, tc.extra)
			wantErr(t, resp, body, tc.status, tc.code)
			if openSubscribes(t, b) != 0 {
				t.Fatal("a refused upload had effects")
			}
		})
	}
	h := podcastHeaders()
	h["Content-Type"] = "text/xml"
	for _, q := range []string{"", "?library_id=0", "?library_id=x", fmt.Sprintf("?library_id=%d&library_id=%d", b.lib.ID, b.lib.ID), fmt.Sprintf("?library_id=%d&user_id=1", b.lib.ID)} {
		b.api.opml = newOPMLTransfers()
		resp, body := b.doHeaders(t, "POST", "/api/v1/podcasts/opml/import"+q, b.userTok, valid, h)
		wantErr(t, resp, body, 400, "invalid_request")
	}
	resp, body := opmlPost(t, b, b.userTok, 999, valid, "text/xml", nil)
	wantErr(t, resp, body, 404, "library_not_found")
	books, err := b.cat.CreateLibrary(ctx, catalog.Library{Name: "Books", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// Without a grant its kind stays hidden: 403, not 409.
	resp, body = opmlPost(t, b, b.userTok, books.ID, valid, "text/xml", nil)
	wantErr(t, resp, body, 403, "no_library_access")
	if err := b.cat.GrantWholeLibrary(ctx, uid, books.ID); err != nil {
		t.Fatal(err)
	}
	resp, body = opmlPost(t, b, b.userTok, books.ID, valid, "text/xml", nil)
	wantErr(t, resp, body, 409, "not_podcast_library")
	// A narrow grant, a demo, a kid and an old client are refused.
	other, partial := newUserWithSession(t, b.testEnv, "partial-opml", false)
	share, err := b.cat.CreateShare(ctx, catalog.Share{Name: "One show", Paths: []catalog.PathRule{{LibraryID: b.lib.ID, Path: "Example Show"}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = b.cat.GrantShare(ctx, other, share.ID)
	resp, body = opmlPost(t, b, partial, b.lib.ID, valid, "text/xml", nil)
	wantErr(t, resp, body, 403, "no_library_access")
	_, demoTok := newUserWithSession(t, b.testEnv, "demo-opml", true)
	resp, body = opmlPost(t, b, demoTok, b.lib.ID, valid, "text/xml", nil)
	wantErr(t, resp, body, 403, "demo_account")
	kid, kidTok := newUserWithSession(t, b.testEnv, "kid-opml", false)
	if _, err := b.db.ExecContext(ctx, `UPDATE users SET managed_by=?,member_kind='kid',member_access='all' WHERE id=?`, b.adminID, kid); err != nil {
		t.Fatal(err)
	}
	resp, body = opmlPost(t, b, kidTok, b.lib.ID, valid, "text/xml", nil)
	wantErr(t, resp, body, 403, "kid_account")
	resp, body = opmlPost(t, b, b.userTok, b.lib.ID, valid, "text/xml", map[string]string{auth.ClientHeader: "AudioSilo/2.1.0 (web)"})
	wantErr(t, resp, body, 403, "no_library_access")
	// Budgets: one burst per person, two uploads at a time server-wide.
	resp, _ = opmlPost(t, b, b.userTok, b.lib.ID, valid, "text/xml", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("valid upload %d", resp.StatusCode)
	}
	h = podcastHeaders()
	h["Content-Type"] = "text/xml"
	route := fmt.Sprintf("/api/v1/podcasts/opml/import?library_id=%d", b.lib.ID)
	resp, body = b.doHeaders(t, "POST", route, b.userTok, valid, h)
	wantErr(t, resp, body, 429, "opml_rate_limited")
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	b.api.opml = newOPMLTransfers()
	b.api.opml.slots.TryAcquire()
	b.api.opml.slots.TryAcquire()
	resp, body = b.doHeaders(t, "POST", route, b.userTok, valid, h)
	wantErr(t, resp, body, 503, "opml_busy")
	if resp.Header.Get("Retry-After") != "1" {
		t.Fatal("busy Retry-After")
	}
	// The module off.
	b.api.opml = newOPMLTransfers()
	_, _ = b.do(t, "PATCH", "/api/v1/admin/settings", b.adminTok, `{"modules":{"podcasts":false}}`)
	resp, body = b.doHeaders(t, "POST", route, b.userTok, valid, h)
	wantErr(t, resp, body, 404, "module_off")
}

func TestOPMLExport(t *testing.T) {
	b, uid, f := podcastEnv(t, false)
	h := podcastHeaders()
	ctx := context.Background()
	resp, body := b.doHeaders(t, "GET", "/api/v1/podcasts/opml/export", b.userTok, "", h)
	wantErr(t, resp, body, 200, "")
	if resp.Header.Get("X-OPML-Exported") != "0" || !strings.Contains(body, "<body>\n  </body>") {
		t.Fatalf("empty export %v %q", resp.Header, body)
	}
	show, err := b.cat.PodcastShow(ctx, b.lib.ID, "Example Show", uid)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.cat.FollowPodcast(ctx, uid, b.lib.ID, show.Path, show.ShowID); err != nil {
		t.Fatal(err)
	}
	// The module off and no Bridge: the export still answers.
	_, _ = b.do(t, "PATCH", "/api/v1/admin/settings", b.adminTok, `{"modules":{"podcasts":false}}`)
	resp, body = b.doHeaders(t, "GET", "/api/v1/podcasts/opml/export", b.userTok, "", h)
	wantErr(t, resp, body, 200, "")
	want := map[string]string{
		"Content-Type":               "text/x-opml; charset=utf-8",
		"Content-Disposition":        `attachment; filename="audiosilo-subscriptions.opml"`,
		"Cache-Control":              "no-store",
		"X-Content-Type-Options":     "nosniff",
		"X-OPML-Exported":            "1",
		"X-OPML-Omitted-Private":     "0",
		"X-OPML-Omitted-Unavailable": "0",
	}
	for k, v := range want {
		if resp.Header.Get(k) != v {
			t.Fatalf("%s = %q", k, resp.Header.Get(k))
		}
	}
	if !strings.Contains(body, `xmlUrl="`+f.FeedURL+`"`) {
		t.Fatalf("export %s", body)
	}
	// Another person exports nothing of it; an old client, a demo and a query
	// token are refused; HEAD is 405.
	_, otherTok := newUserWithSession(t, b.testEnv, "other-export", false)
	_, body = b.doHeaders(t, "GET", "/api/v1/podcasts/opml/export", otherTok, "", h)
	if strings.Contains(body, f.FeedURL) {
		t.Fatal("another person's follow exported")
	}
	resp, body = b.doHeaders(t, "GET", "/api/v1/podcasts/opml/export", b.adminTok, "", map[string]string{auth.ClientHeader: "AudioSilo/2.1.0 (web)"})
	wantErr(t, resp, body, 403, "no_library_access")
	_, demoTok := newUserWithSession(t, b.testEnv, "demo-export", true)
	resp, body = b.doHeaders(t, "GET", "/api/v1/podcasts/opml/export", demoTok, "", h)
	wantErr(t, resp, body, 403, "demo_account")
	resp, _ = b.do(t, "GET", "/api/v1/podcasts/opml/export?token="+url.QueryEscape(b.userTok), "", "")
	if resp.StatusCode != 401 {
		t.Fatalf("query token %d", resp.StatusCode)
	}
	resp, _ = b.doHeaders(t, "GET", "/api/v1/podcasts/opml/export?library_id=1", b.userTok, "", h)
	if resp.StatusCode != 400 {
		t.Fatalf("unknown parameter %d", resp.StatusCode)
	}
	// Six a minute, a burst of two.
	b.api.opml = newOPMLTransfers()
	for i := range 3 {
		resp, body = b.doHeaders(t, "GET", "/api/v1/podcasts/opml/export", b.userTok, "", h)
		if i < 2 && resp.StatusCode != 200 {
			t.Fatalf("export %d: %d", i, resp.StatusCode)
		}
	}
	wantErr(t, resp, body, 429, "opml_rate_limited")
	if resp.Header.Get("Retry-After") == "" || resp.Header.Get("X-OPML-Exported") != "" {
		t.Fatalf("rate limited headers %v", resp.Header)
	}
}

// The single follow keeps its envelopes over the shared follower, and a
// request for an open feed at the other privacy is refused.
func TestPodcastFollowPrivacyConflict(t *testing.T) {
	b, _, _ := podcastEnv(t, false)
	h := podcastHeaders()
	post := func(body string) (*http.Response, string) {
		return b.doHeaders(t, "POST", "/api/v1/podcasts/follow", b.userTok, body, h)
	}
	resp, body := post(fmt.Sprintf(`{"library_id":%d,"feed_url":"https://example.com/members.xml"}`, b.lib.ID))
	wantErr(t, resp, body, 202, "")
	var out map[string]json.RawMessage
	decodeInto(t, body, &out)
	if string(out["state"]) != `"subscribing"` || out["request"] == nil {
		t.Fatalf("envelope %s", body)
	}
	resp, body = post(fmt.Sprintf(`{"library_id":%d,"feed_url":"https://example.com/members.xml","private":true}`, b.lib.ID))
	wantErr(t, resp, body, 409, "privacy_conflict")
	resp, body = post(fmt.Sprintf(`{"library_id":%d,"feed_url":"https://example.com/members.xml"}`, b.lib.ID))
	wantErr(t, resp, body, 202, "")
	if openSubscribes(t, b) != 1 {
		t.Fatal("the adoption made another job")
	}
}
