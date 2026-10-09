package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/query/querytest"
	"github.com/kodestar/audiosilo-meta/pkg/release"
	"github.com/kodestar/audiosilo-meta/pkg/release/releasetest"

	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/metamirror"
	"github.com/kodestar/audiosilo-server/internal/mirrortest"
)

// mirrorEnv is a test env in mirror mode: a real metamirror.Mirror holding the
// querytest artifact (downloaded from a fake GitHub), in front of a remote
// metadata service that counts every request it gets.
type mirrorEnv struct {
	*testEnv
	mirror     *metamirror.Mirror
	remoteHits *atomic.Int32
	adminTok   string
	memberTok  string
	remoteURL  string
}

func newMirrorEnv(t *testing.T) *mirrorEnv {
	t.Helper()
	remote, hits := mirrortest.Remote(t)
	e := newTestEnvWith(t, func(c *config.Config) {
		c.Metadata.Enabled = true
		c.Metadata.BaseURL = remote.URL
		c.Metadata.Mode = config.MetadataMirror
	})

	gh := releasetest.NewGitHub(t, mirrortest.Releases(mirrortest.Fixture(t, 0), "data-v2026.10.09-ccccccc-ddddddd")...)
	m, err := metamirror.New(filepath.Join(e.cfg.DataDir, metamirror.DirName), metamirror.Options{
		Enabled:   e.api.MetadataOn,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		SiteURL:   remote.URL,
		FreeBytes: func(string) (uint64, error) { return 1 << 40, nil },
		Repo:      releasetest.Repo,
		Release:   []release.Option{release.WithAPIBase(gh.URL)},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done; _ = m.Close() })
	m.CheckNow()
	mirrortest.Eventually(t, "the mirror's copy", 5*time.Second, m.Ready)
	e.api.SetMetaMirror(m)
	adminTok, memberTok := opsTokens(t, e)
	return &mirrorEnv{testEnv: e, mirror: m, remoteHits: hits, adminTok: adminTok, memberTok: memberTok, remoteURL: remote.URL}
}

// A book's /meta in mirror mode is answered by the local copy: the community
// layer, the series rail, links on the configured site, and the remote service
// never asked. A work by id (and a retired slug's redirect) too.
func TestMetaFromMirror(t *testing.T) {
	e := newMirrorEnv(t)
	libID := seedBook(t, e.testEnv, "Andy Weir/Project Hail Mary", querytest.ASIN)
	tok := e.adminTok

	resp, body := e.do(t, "GET", "/api/v1/libraries/"+strconv.FormatInt(libID, 10)+"/meta?path="+escape("Andy Weir/Project Hail Mary"), tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("meta = %d %s", resp.StatusCode, body)
	}
	var env struct {
		Matched bool `json:"matched"`
		Work    struct {
			ID          string            `json:"id"`
			Characters  []json.RawMessage `json:"characters"`
			Attribution *struct {
				SourceURL string `json:"source_url"`
			} `json:"attribution"`
		} `json:"work"`
		Recording struct {
			ID string `json:"id"`
		} `json:"recording"`
		WebURL string `json:"web_url"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatal(err)
	}
	if !env.Matched || env.Work.ID != querytest.ASINWork || env.Recording.ID != querytest.ASINRecording ||
		len(env.Work.Characters) == 0 || env.Work.Attribution == nil {
		t.Fatalf("meta = %s", body)
	}
	if !strings.HasPrefix(env.WebURL, e.remoteURL+"/work?id=") || !strings.HasPrefix(env.Work.Attribution.SourceURL, e.remoteURL+"/") {
		t.Fatalf("links must stay on metadata.base_url: %s", body)
	}

	resp, body = e.do(t, "GET", "/api/v1/meta/work?id="+querytest.RetiredWork, tok, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"id":"`+querytest.RetiredWorkTarget+`"`) {
		t.Fatalf("a retired work = %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(t, "GET", "/api/v1/meta/work?id=no-such-work", tok, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown work = %d %s", resp.StatusCode, body)
	}
	if n := e.remoteHits.Load(); n != 0 {
		t.Fatalf("the remote service was asked %d times", n)
	}
}

// Health > System in mirror mode: the mode, the copy's status, and the health
// check answered by the ready copy (no outbound request); the settings show the
// mode, a restart setting.
func TestSystemStatusMirror(t *testing.T) {
	e := newMirrorEnv(t)
	_, body := e.do(t, "GET", "/api/v1/admin/system", e.adminTok, "")
	var sys struct {
		Metadata struct {
			Mode   string `json:"mode"`
			Health *struct {
				Reachable bool `json:"reachable"`
			} `json:"health"`
			Mirror *metamirror.Status `json:"mirror"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(body), &sys); err != nil {
		t.Fatal(err)
	}
	md := sys.Metadata
	if md.Mode != "mirror" || md.Health == nil || !md.Health.Reachable || md.Mirror == nil ||
		md.Mirror.State != metamirror.StateReady || md.Mirror.Tag != "data-v2026.10.09-ccccccc-ddddddd" || md.Mirror.Fallback {
		t.Fatalf("system metadata = %s", body)
	}
	if e.remoteHits.Load() != 0 {
		t.Fatal("the health check must not ask the remote service in mirror mode")
	}

	_, body = e.do(t, "GET", "/api/v1/admin/settings", e.adminTok, "")
	if !strings.Contains(body, `"mode":"mirror"`) || !strings.Contains(body, `"metadata.mode"`) {
		t.Fatalf("settings = %s", body)
	}
}

// Remote mode reports its mode and no mirror; a saved switch to mirror mode
// waits for a restart.
func TestSystemStatusRemoteMode(t *testing.T) {
	e := newMetaEnv(t, true, 0)
	adminTok, _ := opsTokens(t, e)
	_, body := e.do(t, "GET", "/api/v1/admin/system", adminTok, "")
	if !strings.Contains(body, `"mode":"remote"`) || strings.Contains(body, `"mirror"`) {
		t.Fatalf("system = %s", body)
	}
	resp, body := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"metadata":{"mode":"Mirror"}}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"mode":"mirror"`) {
		t.Fatalf("save mirror mode = %d %s", resp.StatusCode, body)
	}
	var env struct {
		RestartPending []string `json:"restart_pending"`
	}
	_ = json.Unmarshal([]byte(body), &env)
	if len(env.RestartPending) != 1 || env.RestartPending[0] != "metadata.mode" {
		t.Fatalf("restart_pending = %v", env.RestartPending)
	}
	if _, body := e.do(t, "GET", "/api/v1/admin/system", adminTok, ""); !strings.Contains(body, `"mode":"remote"`) {
		t.Fatalf("the running mode stays remote until a restart: %s", body)
	}
	if resp, body := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"metadata":{"mode":"local"}}`); resp.StatusCode != http.StatusBadRequest ||
		!strings.Contains(body, codeInvalidSetting) {
		t.Fatalf("an unknown mode = %d %s", resp.StatusCode, body)
	}
}

// The mirror's two routes, GET /admin/meta/mirror (its status) and POST
// /admin/meta/mirror/check (202 with its status after the wake: the next check
// due now), for an admin in mirror mode; 403 for a member and 401 signed out.
func TestMetaMirrorRoutes(t *testing.T) {
	e := newMirrorEnv(t)
	resp, body := e.do(t, "GET", "/api/v1/admin/meta/mirror", e.adminTok, "")
	var st metamirror.Status
	if err := json.Unmarshal([]byte(body), &st); err != nil || resp.StatusCode != http.StatusOK ||
		st.State != metamirror.StateReady || st.Tag != "data-v2026.10.09-ccccccc-ddddddd" {
		t.Fatalf("status = %d %s", resp.StatusCode, body)
	}
	if time.Until(st.NextCheckAt) < time.Hour {
		t.Fatalf("a fresh copy's next check is a day away, got %v", st.NextCheckAt)
	}

	before := time.Now()
	resp, body = e.do(t, "POST", "/api/v1/admin/meta/mirror/check", e.adminTok, "")
	st = metamirror.Status{}
	if err := json.Unmarshal([]byte(body), &st); err != nil || resp.StatusCode != http.StatusAccepted || st.State == "" {
		t.Fatalf("check = %d %s", resp.StatusCode, body)
	}
	// Woken: the next check is due now (or, the runner already on it, not set).
	if n := st.NextCheckAt; !n.IsZero() && (n.Before(before) || n.After(time.Now())) {
		t.Fatalf("the check's answer must reflect the wake, next_check_at = %v", st.NextCheckAt)
	}

	for _, r := range []struct{ method, path string }{
		{"GET", "/api/v1/admin/meta/mirror"},
		{"POST", "/api/v1/admin/meta/mirror/check"},
	} {
		if resp, _ := e.do(t, r.method, r.path, e.memberTok, ""); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("a member's %s %s = %d, want 403", r.method, r.path, resp.StatusCode)
		}
		if resp, _ := e.do(t, r.method, r.path, "", ""); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("a signed-out %s %s = %d, want 401", r.method, r.path, resp.StatusCode)
		}
	}

	// While metadata is off: 404 metadata_off.
	if resp, b := e.do(t, "PATCH", "/api/v1/admin/settings", e.adminTok, `{"metadata":{"enabled":false}}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("turn off = %d %s", resp.StatusCode, b)
	}
	for _, method := range []string{"GET", "POST"} {
		path := "/api/v1/admin/meta/mirror"
		if method == "POST" {
			path += "/check"
		}
		resp, body = e.do(t, method, path, e.adminTok, "")
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, `"code":"`+codeMetadataOff+`"`) {
			t.Fatalf("%s %s while off = %d %s", method, path, resp.StatusCode, body)
		}
	}
}

// In remote mode there is no copy: 409 not_mirror_mode on both routes, in the
// coded envelope the console branches on.
func TestMetaMirrorRoutesRemoteMode(t *testing.T) {
	e := newMetaEnv(t, true, 0)
	adminTok, _ := opsTokens(t, e)
	for _, r := range []struct{ method, path string }{
		{"GET", "/api/v1/admin/meta/mirror"},
		{"POST", "/api/v1/admin/meta/mirror/check"},
	} {
		resp, body := e.do(t, r.method, r.path, adminTok, "")
		if resp.StatusCode != http.StatusConflict || !strings.Contains(body, `"code":"`+codeNotMirrorMode+`"`) {
			t.Fatalf("%s %s in remote mode = %d %s", r.method, r.path, resp.StatusCode, body)
		}
	}
}
