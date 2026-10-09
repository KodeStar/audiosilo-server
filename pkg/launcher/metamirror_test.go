package launcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/query/querytest"
	"github.com/kodestar/audiosilo-meta/pkg/release"

	"github.com/kodestar/audiosilo-server/internal/api"
	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/metamirror"
	"github.com/kodestar/audiosilo-server/internal/metamirror/ghfake"
	"github.com/kodestar/audiosilo-server/internal/store"
)

// A server started in remote mode deletes the copy mirror mode left; one with
// no metadata service builds nothing and deletes nothing.
func TestMetaMirrorRemoteModeDeletesCopy(t *testing.T) {
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(metamirror.Dir(data), "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default(data)
	cfg.Metadata.BaseURL = ""
	cfg.Metadata.Mode = config.MetadataRemote
	if m := metaMirror(cfg, data, nil, discardLog()); m != nil {
		t.Fatal("no mirror without a metadata service")
	}
	if _, err := os.Stat(metamirror.Dir(data)); err != nil {
		t.Fatal("without a metadata service the folder is left alone")
	}

	cfg = config.Default(data)
	if m := metaMirror(cfg, data, nil, discardLog()); m != nil {
		t.Fatal("no mirror in remote mode")
	}
	if _, err := os.Stat(metamirror.Dir(data)); !os.IsNotExist(err) {
		t.Fatalf("remote mode must delete the mirror folder: %v", err)
	}
}

// The live smoke of mirror mode, through the real launcher: a server started
// with metadata.mode: mirror downloads the copy from (a fake) GitHub when an
// admin asks, swaps it in, reports it on /admin/system, and answers a book's
// /meta from it without a single request to the remote metadata service.
func TestRunMirrorMode(t *testing.T) {
	gh := ghfake.New(t, ghfake.Releases(ghfake.Fixture(t, 0), "data-v2026.10.09-ccccccc-ddddddd")...)
	testReleaseOptions = []release.Option{release.WithAPIBase(gh.URL)}
	t.Cleanup(func() { testReleaseOptions = nil })

	var remoteHits atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		remoteHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(remote.Close)

	// A library with one book, an admin, and a config in mirror mode.
	data := t.TempDir()
	root := t.TempDir()
	const rel = "Andy Weir/Project Hail Mary"
	if err := os.MkdirAll(filepath.Join(root, rel), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("..", "..", "testdata", "library", "Brandon Sanderson", "Mistborn", "01 - The Final Empire.m4b"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, rel, "book.m4b"), fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(data, "audiosilo.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.New(db, time.Now).CreateUser(ctx, "admin", "admin-password", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t)
	cfgYAML := fmt.Sprintf(`bind: %q
tls:
  mode: "off"
update_check: false
libraries:
  - name: Books
    root: %q
metadata:
  enabled: true
  base_url: %q
  mode: mirror
`, addr, root, remote.URL)
	if err := os.WriteFile(config.Path(data), []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- Run(runCtx, Options{DataDir: data, Log: discardLog()}) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Run did not stop")
		}
	})
	base := "http://" + addr + "/api/v1"
	waitFor(t, "the server", func() bool {
		resp, err := http.Get(base + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	var login struct {
		Token string `json:"token"`
	}
	call(t, "POST", base+"/auth/login", "", `{"username":"admin","password":"admin-password"}`, http.StatusOK, &login)
	tok := login.Token

	// No copy yet: the console says so, and asking for a check starts the download.
	var sys struct {
		Metadata struct {
			Mode   string             `json:"mode"`
			Mirror *metamirror.Status `json:"mirror"`
			Health *struct {
				Reachable bool `json:"reachable"`
			} `json:"health"`
		} `json:"metadata"`
	}
	call(t, "GET", base+"/admin/system", tok, "", http.StatusOK, &sys)
	if sys.Metadata.Mode != "mirror" || sys.Metadata.Mirror == nil || !sys.Metadata.Mirror.Fallback || sys.Metadata.Mirror.State != metamirror.StateEmpty {
		t.Fatalf("before the download: %+v", sys.Metadata)
	}
	call(t, "POST", base+"/admin/meta/mirror/check", tok, "", http.StatusAccepted, nil)
	waitFor(t, "the copy", func() bool {
		call(t, "GET", base+"/admin/system", tok, "", http.StatusOK, &sys)
		return sys.Metadata.Mirror != nil && sys.Metadata.Mirror.State == metamirror.StateReady
	})
	if m := sys.Metadata.Mirror; m.Tag != "data-v2026.10.09-ccccccc-ddddddd" || m.Fallback || m.SizeBytes == 0 || m.Error != "" ||
		sys.Metadata.Health == nil || !sys.Metadata.Health.Reachable {
		t.Fatalf("after the download: %+v %+v", m, sys.Metadata.Health)
	}
	if ua := gh.UserAgents(); len(ua) == 0 || ua[0] != "AudioSilo/"+api.Version {
		t.Fatalf("User-Agent = %v", ua)
	}

	// The book, once indexed, gets the ASIN the copy knows, and its /meta comes
	// from the copy.
	var libs struct {
		Libraries []struct {
			ID int64 `json:"id"`
		} `json:"libraries"`
	}
	call(t, "GET", base+"/libraries", tok, "", http.StatusOK, &libs)
	if len(libs.Libraries) != 1 {
		t.Fatalf("libraries = %+v", libs)
	}
	lib := strconv.FormatInt(libs.Libraries[0].ID, 10)
	waitFor(t, "the scan", func() bool {
		var page struct {
			Books []struct {
				Path string `json:"rel_path"`
			} `json:"books"`
		}
		call(t, "GET", base+"/libraries/"+lib+"/books", tok, "", http.StatusOK, &page)
		return len(page.Books) == 1 && page.Books[0].Path == rel
	})
	call(t, "PATCH", base+"/admin/libraries/"+lib+"/book?path="+url.QueryEscape(rel), tok,
		`{"set":{"asin":"`+querytest.ASIN+`"}}`, http.StatusOK, nil)
	var meta struct {
		Matched bool `json:"matched"`
		Work    struct {
			ID string `json:"id"`
		} `json:"work"`
	}
	call(t, "GET", base+"/libraries/"+lib+"/meta?path="+url.QueryEscape(rel), tok, "", http.StatusOK, &meta)
	if !meta.Matched || meta.Work.ID != querytest.ASINWork {
		t.Fatalf("meta = %+v", meta)
	}
	if n := remoteHits.Load(); n != 0 {
		t.Fatalf("the remote metadata service was asked %d times", n)
	}
	entries, err := os.ReadDir(metamirror.Dir(data))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "meta-data-v2026.10.09-ccccccc-ddddddd.sqlite,state.json" {
		t.Fatalf("the mirror folder holds %v", names)
	}
}

// freeAddr is a loopback address with a port nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// waitFor polls cond for up to ten seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// call sends one API request, checks its status and decodes its answer into out
// (when non-nil).
func call(t *testing.T, method, u, token, body string, want int, out any) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, u, r)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s = %d %s, want %d", method, u, resp.StatusCode, b, want)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s: %v (%s)", method, u, err, b)
		}
	}
}
