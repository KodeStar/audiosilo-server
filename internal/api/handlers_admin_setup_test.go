package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
)

// rootBody is a preview request body naming a folder (quoted, so a test temp dir with
// odd characters still makes valid JSON).
func rootBody(root string) string {
	b, _ := json.Marshal(map[string]string{"root": root})
	return string(b)
}

func setupStateOf(t *testing.T, e *testEnv) catalog.SetupState {
	t.Helper()
	st, err := e.cat.SetupState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// The setup routes are the console's: a member and a demo visitor are refused
// 403, a signed-out caller 401, and none of their tries changes anything; an
// admin is served.
func TestAdminSetupRoutesAdminOnly(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, memberTok := opsTokens(t, e)
	demo, err := e.auth.CreateDemoUser(context.Background(), "demo_x")
	if err != nil {
		t.Fatal(err)
	}
	demoTok, _ := e.auth.IssueToken(context.Background(), demo.ID, auth.KindSession, "t", 0)
	books := copyFixtures(t, "Will Wight/Cradle/01 - Unsouled.m4b")
	routes := []struct{ method, path, body string }{
		{"GET", "/api/v1/admin/setup", ""},
		{"PUT", "/api/v1/admin/setup", `{"finished":true,"steps":{"folder":"done"}}`},
		{"POST", "/api/v1/admin/setup/preview", rootBody(books)},
	}
	for _, rt := range routes {
		for who, tok := range map[string]string{"member": memberTok, "demo visitor": demoTok} {
			if resp, _ := e.do(t, rt.method, rt.path, tok, rt.body); resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s as a %s = %d, want 403", rt.method, rt.path, who, resp.StatusCode)
			}
		}
		if resp, _ := e.do(t, rt.method, rt.path, "", rt.body); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s signed out = %d, want 401", rt.method, rt.path, resp.StatusCode)
		}
	}
	if st := setupStateOf(t, e); st.Finished || len(st.Steps) != 0 {
		t.Fatalf("a refused PUT changed the state: %+v", st)
	}
	for _, rt := range routes {
		if resp, body := e.do(t, rt.method, rt.path, adminTok, rt.body); resp.StatusCode != http.StatusOK {
			t.Errorf("%s %s as the admin = %d %s, want 200", rt.method, rt.path, resp.StatusCode, body)
		}
	}
}

// GET /admin/setup on a fresh server: not finished, no step seen, the request as
// the server saw it, the config's libraries and the mount hints (a list, never
// null). A request through a proxy names the proxy, and says whether it is
// trusted.
func TestAdminSetupGet(t *testing.T) {
	t.Parallel()
	e := newTestEnvWith(t, func(c *config.Config) {
		c.Libraries = []config.Library{{Name: "Audiobooks", Root: "/audiobooks"}}
	})
	adminTok, _ := opsTokens(t, e)
	var env struct {
		State   catalog.SetupState `json:"state"`
		Request map[string]any     `json:"request"`
		Config  []string           `json:"config_libraries"`
		Mounts  []string           `json:"mount_hints"`
	}
	get := func(headers map[string]string) string {
		t.Helper()
		resp, body := e.doHeaders(t, "GET", "/api/v1/admin/setup", adminTok, "", headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /admin/setup = %d %s", resp.StatusCode, body)
		}
		env.Request = nil
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatal(err)
		}
		return body
	}
	body := get(nil)
	if env.State.Finished || env.State.Dismissed || env.State.Steps == nil || len(env.State.Steps) != 0 {
		t.Errorf("fresh state = %+v", env.State)
	}
	host := strings.TrimPrefix(e.srv.URL, "http://")
	if env.Request["host"] != host || env.Request["scheme"] != "http" || env.Request["proxied"] != false ||
		env.Request["peer_trusted"] != false {
		t.Errorf("request = %v, want host %s over http, not proxied", env.Request, host)
	}
	if _, ok := env.Request["peer"]; ok {
		t.Errorf("a direct request named its peer: %v", env.Request)
	}
	if len(env.Config) != 1 || env.Config[0] != "Audiobooks" {
		t.Errorf("config_libraries = %v", env.Config)
	}
	if !strings.Contains(body, `"mount_hints":[`) {
		t.Errorf("mount_hints is not a list: %s", body)
	}

	get(map[string]string{"X-Forwarded-For": "203.0.113.9", "Host": "books.example.com"})
	if env.Request["proxied"] != true || env.Request["peer"] != "127.0.0.1" || env.Request["peer_trusted"] != false ||
		env.Request["host"] != "books.example.com" {
		t.Errorf("proxied request = %v, want the untrusted peer 127.0.0.1", env.Request)
	}

	trusted := newTestEnvWith(t, func(c *config.Config) { c.TrustedProxies = []string{"127.0.0.0/8"} })
	adminTok, _ = opsTokens(t, trusted)
	resp, raw := trusted.doHeaders(t, "GET", "/api/v1/admin/setup", adminTok, "", map[string]string{"X-Forwarded-For": "203.0.113.9"})
	if resp.StatusCode != http.StatusOK || !strings.Contains(raw, `"peer":"127.0.0.1","peer_trusted":true`) {
		t.Errorf("trusted proxy = %d %s", resp.StatusCode, raw)
	}
	if !strings.Contains(raw, `"config_libraries":[]`) {
		t.Errorf("no config libraries is not []: %s", raw)
	}
}

// PUT /admin/setup merges the fields it is sent, answers the state and is
// audited; an unknown step, an unknown outcome or a bad body is 400 and stores
// nothing.
func TestAdminSetupPut(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	put := func(body string) (int, string) {
		t.Helper()
		resp, b := e.do(t, "PUT", "/api/v1/admin/setup", adminTok, body)
		return resp.StatusCode, b
	}
	if code, b := put(`{"steps":{"folder":"done","naming":"skipped"}}`); code != http.StatusOK {
		t.Fatalf("PUT steps = %d %s", code, b)
	}
	code, b := put(`{"finished":true,"steps":{"naming":"done","bridge":"skipped"}}`)
	if code != http.StatusOK {
		t.Fatalf("PUT finished = %d %s", code, b)
	}
	var st catalog.SetupState
	if err := json.Unmarshal([]byte(b), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Finished || st.Dismissed || len(st.Steps) != 3 || st.Steps["folder"] != "done" ||
		st.Steps["naming"] != "done" || st.Steps["bridge"] != "skipped" {
		t.Errorf("merged state = %s", b)
	}
	events := settingsAudit(t, e)
	if len(events) != 2 || events[0].Action != "settings.setup" {
		t.Errorf("audit = %+v, want two settings.setup events", events)
	}

	for _, body := range []string{
		`{"steps":{"wizard":"done"}}`,
		`{"dismissed":true,"steps":{"folder":"later"}}`,
		`{"steps":{"folder":true}}`,
		`{"finished":"yes"}`,
		`not json`,
	} {
		code, b := put(body)
		if code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d %s, want 400", body, code, b)
		}
		if strings.Contains(body, "wizard") || strings.Contains(body, "later") {
			if !strings.Contains(b, `"code":"invalid_setup_step"`) {
				t.Errorf("PUT %s = %s, want code invalid_setup_step", body, b)
			}
		}
	}
	if got := setupStateOf(t, e); got.Dismissed || len(got.Steps) != 3 {
		t.Errorf("a refused PUT stored something: %+v", got)
	}
	if got := len(settingsAudit(t, e)); got != 2 {
		t.Errorf("%d audit events after refused PUTs, want 2", got)
	}
}

// The preview refuses, with the folder picker's codes, a missing root, a
// relative path and a folder that isn't there (or is a file); with library
// create's, a root overlapping a podcast library (which create refuses too); and
// a bad ignore rule. A books library already at the root is a warning, not a
// refusal.
func TestAdminSetupPreviewRefusals(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	base := copyFixtures(t, "Will Wight/Cradle/01 - Unsouled.m4b")
	pods := filepath.Join(base, "Pods")
	copyFixture(t, base, "Will Wight/Cradle/02 - Soulsmith.m4b", "Pods/Show/ep.m4b")
	if _, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Pods", Root: pods, Kind: catalog.KindPodcasts}); err != nil {
		t.Fatal(err)
	}
	preview := func(body string) (int, string) {
		t.Helper()
		resp, b := e.do(t, "POST", "/api/v1/admin/setup/preview", adminTok, body)
		return resp.StatusCode, b
	}
	for _, tc := range []struct {
		name, body string
		status     int
		code       string
	}{
		{"no root", `{}`, http.StatusBadRequest, ""},
		{"not json", `{`, http.StatusBadRequest, ""},
		{"relative", rootBody("Audiobooks"), http.StatusBadRequest, codePathNotAbsolute},
		{"missing", rootBody(filepath.Join(base, "nope")), http.StatusNotFound, codeFolderUnreadable},
		{"a file", rootBody(filepath.Join(base, "Will Wight/Cradle/01 - Unsouled.m4b")), http.StatusNotFound, codeFolderUnreadable},
		{"podcast root", rootBody(pods), http.StatusConflict, "root_overlaps"},
		{"inside a podcast root", rootBody(filepath.Join(pods, "Show")), http.StatusConflict, "root_overlaps"},
		{"holding a podcast root", rootBody(base), http.StatusConflict, "root_overlaps"},
		{"bad ignore rule", `{"root":` + mustJSON(t, filepath.Join(base, "Will Wight")) + `,"ignore_patterns":["Book [0-"]}`,
			http.StatusBadRequest, codeInvalidPattern},
	} {
		code, b := preview(tc.body)
		if code != tc.status || (tc.code != "" && !strings.Contains(b, `"code":"`+tc.code+`"`)) {
			t.Errorf("%s: %d %s, want %d %s", tc.name, code, b, tc.status, tc.code)
		}
		if strings.Contains(b, "Unsouled") {
			t.Errorf("%s: a refusal named the folder's contents: %s", tc.name, b)
		}
	}
	// Library create refuses the podcast-overlapping root the preview refused.
	createBody := `{"name":"Books","root":` + mustJSON(t, filepath.Join(pods, "Show")) + `}`
	if resp, b := e.do(t, "POST", "/api/v1/admin/libraries", adminTok, createBody); resp.StatusCode != http.StatusConflict {
		t.Errorf("create inside the podcast root = %d %s, want 409", resp.StatusCode, b)
	}

	books := filepath.Join(base, "Will Wight")
	code, b := preview(rootBody(books))
	if code != http.StatusOK || !strings.Contains(b, `"same_root":null`) {
		t.Fatalf("preview = %d %s, want 200 with same_root null", code, b)
	}
	if _, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Cradle books", Root: books}); err != nil {
		t.Fatal(err)
	}
	if code, b := preview(rootBody(books + "/")); code != http.StatusOK || !strings.Contains(b, `"same_root":"Cradle books"`) {
		t.Errorf("preview of a library's root = %d %s, want same_root Cradle books", code, b)
	}
	libs, _ := e.cat.ListLibraries(context.Background())
	if len(libs) != 2 {
		t.Errorf("the preview created a library: %+v", libs)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The preview of a folder whose tags and folders disagree: the counts, both
// columns per sample (from the tags over the path, from the folders over the
// tags) and the suggested source, with nothing indexed.
func TestAdminSetupPreviewTwoColumns(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	root := t.TempDir()
	// Tagged Unsouled by Will Wight, filed as #3 of Some Series by Somebody Else.
	copyFixture(t, root, "Will Wight/Cradle/01 - Unsouled.m4b", "Somebody Else/Some Series/03 - Other Title/01.m4b")
	// Tagged The Final Empire by Brandon Sanderson, loose at the root.
	copyFixture(t, root, "Brandon Sanderson/Mistborn/01 - The Final Empire.m4b", "Loose.m4b")
	resp, b := e.do(t, "POST", "/api/v1/admin/setup/preview", adminTok, rootBody(root))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview = %d %s", resp.StatusCode, b)
	}
	var p struct {
		Root   string `json:"root"`
		Counts struct {
			AudioFiles  int  `json:"audio_files"`
			FolderBooks int  `json:"folder_books"`
			FileBooks   int  `json:"file_books"`
			Capped      bool `json:"capped"`
		} `json:"counts"`
		SameRoot *string `json:"same_root"`
		Samples  []struct {
			RelPath string               `json:"rel_path"`
			Tags    catalog.ScannedNames `json:"tags"`
			Path    catalog.ScannedNames `json:"path"`
			ABS     bool                 `json:"abs"`
		} `json:"samples"`
		Blanks    map[string]int `json:"blanks"`
		Suggested string         `json:"suggested"`
	}
	if err := json.Unmarshal([]byte(b), &p); err != nil {
		t.Fatal(err)
	}
	if p.Root != root || p.Counts.AudioFiles != 2 || p.Counts.FolderBooks != 1 || p.Counts.FileBooks != 1 || p.Counts.Capped || p.SameRoot != nil {
		t.Errorf("preview = %s", b)
	}
	if len(p.Samples) != 2 {
		t.Fatalf("samples = %s", b)
	}
	loose, filed := p.Samples[0], p.Samples[1]
	if loose.RelPath != "Loose.m4b" || loose.Tags.Title != "The Final Empire" || loose.Path.Title != "Loose" ||
		loose.Tags.Author != "Brandon Sanderson" || loose.Path.Author != "Brandon Sanderson" {
		t.Errorf("loose sample = %+v", loose)
	}
	wantTags := catalog.ScannedNames{Title: "Unsouled", Author: "Will Wight", Series: "Some Series", Position: "3"}
	wantPath := catalog.ScannedNames{Title: "Other Title", Author: "Somebody Else", Series: "Some Series", Position: "3"}
	if filed.RelPath != "Somebody Else/Some Series/03 - Other Title" || filed.Tags != wantTags || filed.Path != wantPath || filed.ABS {
		t.Errorf("filed sample = %+v, want tags %+v and path %+v", filed, wantTags, wantPath)
	}
	// Each source on its own, narrators left out: the tags lack both series (2);
	// the folders the loose file's author and series (2). A tie goes to the tags.
	if p.Blanks["tags"] != 2 || p.Blanks["path"] != 2 || p.Suggested != catalog.MetadataFromTags {
		t.Errorf("blanks %v, suggested %q; want 2 each and tags", p.Blanks, p.Suggested)
	}
	if n, _ := e.cat.CountBooksByLibrary(context.Background()); len(n) != 0 {
		t.Errorf("the preview indexed books: %v", n)
	}
}

// The mount hints are read only while the folder step may still show: while
// the flow is open, or once finished with the folder step skipped (reopened
// from the Overview card). A finished flow whose folder step is done sends []
// without reading the mount table.
func TestAdminSetupMountHintsWhileOpen(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	var reads atomic.Int32
	e.api.mountHints = func(exclude ...string) []string {
		reads.Add(1)
		return []string{"/audiobooks"}
	}
	hints := func() string {
		t.Helper()
		resp, body := e.do(t, "GET", "/api/v1/admin/setup", adminTok, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /admin/setup = %d %s", resp.StatusCode, body)
		}
		var env struct {
			Mounts []string `json:"mount_hints"`
		}
		if err := json.Unmarshal([]byte(body), &env); err != nil || env.Mounts == nil {
			t.Fatalf("mount_hints in %s (%v)", body, err)
		}
		return strings.Join(env.Mounts, ",")
	}
	if got := hints(); got != "/audiobooks" || reads.Load() != 1 {
		t.Errorf("open flow: hints %q after %d reads, want /audiobooks read once", got, reads.Load())
	}
	if resp, body := e.do(t, "PUT", "/api/v1/admin/setup", adminTok, `{"finished":true}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT finished = %d %s", resp.StatusCode, body)
	}
	if got := hints(); got != "" || reads.Load() != 1 {
		t.Errorf("finished flow: hints %q after %d reads, want [] and no read", got, reads.Load())
	}
	if resp, body := e.do(t, "PUT", "/api/v1/admin/setup", adminTok, `{"steps":{"folder":"skipped"}}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT folder skipped = %d %s", resp.StatusCode, body)
	}
	if got := hints(); got != "/audiobooks" || reads.Load() != 2 {
		t.Errorf("finished, folder skipped: hints %q after %d reads, want /audiobooks read again", got, reads.Load())
	}
	if resp, body := e.do(t, "PUT", "/api/v1/admin/setup", adminTok, `{"steps":{"folder":"done"}}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT folder done = %d %s", resp.StatusCode, body)
	}
	if got := hints(); got != "" || reads.Load() != 2 {
		t.Errorf("finished, folder done: hints %q after %d reads, want [] and no read", got, reads.Load())
	}
}
