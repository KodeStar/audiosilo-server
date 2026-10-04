package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/logring"
	"github.com/kodestar/audiosilo-server/internal/updates"
)

func opsTokens(t *testing.T, e *testEnv) (adminTok, memberTok string) {
	t.Helper()
	ctx := context.Background()
	member, err := e.auth.CreateUser(ctx, "member", "member-password", auth.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	memberTok, _ = e.auth.IssueToken(ctx, member.ID, auth.KindSession, "t", 0)
	adminTok, _ = e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	return adminTok, memberTok
}

// Every Phase 5a endpoint is admin-only: a member is refused (403), an admin served.
func TestServerOpsEndpointsRequireAdmin(t *testing.T) {
	e := newTestEnv(t)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotModified) }))
	defer gh.Close()
	e.api.SetRuntime(Runtime{Logs: logring.NewRing(10), Updates: updates.New("v1.0.0", gh.URL, true, nil)})
	adminTok, memberTok := opsTokens(t, e)

	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/v1/admin/system", ""},
		{"GET", "/api/v1/admin/update", ""},
		{"POST", "/api/v1/admin/update/check", ""},
		{"GET", "/api/v1/admin/logs", ""},
		{"PATCH", "/api/v1/admin/settings", `{"general":{"name":"Den"}}`},
	} {
		if resp, _ := e.do(t, tc.method, tc.path, memberTok, tc.body); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s as a member = %d, want 403", tc.method, tc.path, resp.StatusCode)
		}
		if resp, _ := e.do(t, tc.method, tc.path, "", tc.body); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s signed out = %d, want 401", tc.method, tc.path, resp.StatusCode)
		}
		if resp, b := e.do(t, tc.method, tc.path, adminTok, tc.body); resp.StatusCode != http.StatusOK {
			t.Errorf("%s %s as admin = %d %s, want 200", tc.method, tc.path, resp.StatusCode, b)
		}
	}
}

func TestSettingsEnvelope(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	_, body := e.do(t, "GET", "/api/v1/admin/settings", adminTok, "")
	var env struct {
		General         map[string]any    `json:"general"`
		Network         map[string]any    `json:"network"`
		Players         map[string]any    `json:"players"`
		Metadata        map[string]any    `json:"metadata"`
		Demo            map[string]any    `json:"demo"`
		Locked          map[string]string `json:"locked"`
		RestartSettings []string          `json:"restart_settings"`
		RestartPending  []string          `json:"restart_pending"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatal(err)
	}
	if env.General["update_check"] != true || env.Network["tls_mode"] != "selfsigned" ||
		env.Metadata["available"] != true || env.Players["web_player"] != "" ||
		env.Demo["max_users_default"] != float64(config.DefaultDemoMaxUsers) || env.Demo["max_users"] != nil {
		t.Fatalf("envelope = %s", body)
	}
	if env.Locked == nil || env.RestartPending == nil || len(env.RestartSettings) == 0 {
		t.Fatalf("locked/restart lists must be present, never null: %s", body)
	}
	if strings.Contains(body, ":null,") && strings.Contains(body, `"cors_origins":null`) {
		t.Fatalf("lists must be [] not null: %s", body)
	}
}

// Settings that apply at once do so for the very next request; a restart
// setting is saved, listed as pending, and the running server keeps the old value.
func TestSettingsApplyLiveOrAfterRestart(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)

	resp, body := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{
		"general": {"name": "Hearthside", "public_url": "https://books.example.com/"},
		"network": {"cors_origins": ["https://app.example.com"], "bind": "0.0.0.0:9443"}
	}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"restart_pending":["network.bind"]`) {
		t.Fatalf("the listen address waits for a restart: %s", body)
	}
	if _, si := e.do(t, "GET", "/api/v1/server", "", ""); !strings.Contains(si, `"name":"Hearthside"`) {
		t.Fatalf("the name applies at once: %s", si)
	}
	r, b := e.doHeaders(t, "OPTIONS", "/api/v1/server", "", "", map[string]string{"Origin": "https://app.example.com"})
	if r.Header.Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Fatalf("CORS applies at once, got %v %s", r.Header, b)
	}
	if got := e.api.boot.Bind; got != "0.0.0.0:8080" {
		t.Fatalf("the running bind must not change before a restart, got %q", got)
	}
	// Invite links use the new public address.
	if u := e.api.baseURL(httptest.NewRequest("GET", "/", nil)); u != "https://books.example.com" {
		t.Fatalf("baseURL = %q", u)
	}
	loaded, _, err := config.Load(e.cfg.DataDir)
	if err != nil || loaded.Name != "Hearthside" || loaded.Bind != "0.0.0.0:9443" {
		t.Fatalf("settings must be saved to config.yaml: %v %+v", err, loaded)
	}
}

func TestSettingsRefused(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	cases := []struct {
		body, code, field string
		status            int
	}{
		{`{"general":{"public_url":"books.example.com"}}`, codeInvalidSetting, "general.public_url", 400},
		{`{"general":{"colour":"pink"}}`, codeUnknownSetting, "general.colour", 400},
		{`{"players":{"web_dir":"/tmp"}}`, codeSettingReadOnly, "players.web_dir", 400},
		{`{"demo":{"library":"Nope"}}`, codeInvalidSetting, "demo.library", 400},
		{`{"network":{"tls_mode":"autocert"}}`, codeInvalidSetting, "network.tls_hosts", 400},
	}
	for _, tc := range cases {
		resp, body := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, tc.body)
		var got struct{ Code, Field string }
		_ = json.Unmarshal([]byte(body), &got)
		if resp.StatusCode != tc.status || got.Code != tc.code || got.Field != tc.field {
			t.Errorf("%s = %d %s, want %d %s/%s", tc.body, resp.StatusCode, body, tc.status, tc.code, tc.field)
		}
	}
	// Nothing changed: one refused setting refuses the whole save.
	resp, _ := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"general":{"name":"Den","public_url":"nope"}}`)
	if resp.StatusCode != http.StatusBadRequest || e.api.config().Name != "" {
		t.Fatalf("a refused save must change nothing (name %q)", e.api.config().Name)
	}
	// A real library name is accepted.
	if _, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Demo", Root: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if resp, b := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"demo":{"library":"Demo"}}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("existing demo library = %d %s", resp.StatusCode, b)
	}
}

// A setting the environment supplies can't be changed from the console (409).
func TestSettingsLockedByEnvironment(t *testing.T) {
	t.Setenv("AUDIOSILO_PUBLIC_URL", "https://env.example.com")
	cfg, _, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := newTestEnvWith(t, func(c *config.Config) { *c = *cfg })
	adminTok, _ := opsTokens(t, e)
	if _, body := e.do(t, "GET", "/api/v1/admin/settings", adminTok, ""); !strings.Contains(body, `"general.public_url":"AUDIOSILO_PUBLIC_URL"`) {
		t.Fatalf("locked map must name the variable: %s", body)
	}
	resp, body := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"general":{"public_url":"https://x.example.com"}}`)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, codeSettingLocked) {
		t.Fatalf("locked setting = %d %s, want 409 %s", resp.StatusCode, body, codeSettingLocked)
	}
}

// The update switch drives the checker: off refuses Check now and makes no
// request; on lets it ask.
func TestUpdateCheckSwitch(t *testing.T) {
	e := newTestEnv(t)
	hits := 0
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"tag_name":"v9.0.0","name":"Nine","html_url":"https://github.com/KodeStar/audiosilo-server/releases/tag/v9.0.0","published_at":"2026-10-01T00:00:00Z"}`))
	}))
	defer gh.Close()
	e.api.SetRuntime(Runtime{Updates: updates.New("v1.0.0", gh.URL, true, nil)})
	adminTok, _ := opsTokens(t, e)

	if resp, b := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"general":{"update_check":false}}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("turn off = %d %s", resp.StatusCode, b)
	}
	if resp, body := e.do(t, "POST", "/api/v1/admin/update/check", adminTok, ""); resp.StatusCode != http.StatusConflict || !strings.Contains(body, codeUpdateCheckOff) {
		t.Fatalf("check while off = %d %s, want 409", resp.StatusCode, body)
	}
	if hits != 0 {
		t.Fatal("no request may reach GitHub while the check is off")
	}
	e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"general":{"update_check":true}}`)
	resp, body := e.do(t, "POST", "/api/v1/admin/update/check", adminTok, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"update_available":true`) || !strings.Contains(body, `"version":"v9.0.0"`) {
		t.Fatalf("check = %d %s", resp.StatusCode, body)
	}
}

func TestSystemStatus(t *testing.T) {
	e := newTestEnvWith(t, func(c *config.Config) { c.Metadata.Enabled = false })
	adminTok, _ := opsTokens(t, e)
	root := t.TempDir()
	if _, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Fiction", Root: root}); err != nil {
		t.Fatal(err)
	}
	resp, body := e.do(t, "GET", "/api/v1/admin/system", adminTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("system = %d %s", resp.StatusCode, body)
	}
	var sys struct {
		Version   string
		Install   string
		Database  catalog.DatabaseInfo
		Tools     []Tool
		Metadata  MetadataStatus
		TLS       TLSStatus
		Libraries []LibraryStatus
		Update    map[string]any
	}
	if err := json.Unmarshal([]byte(body), &sys); err != nil {
		t.Fatal(err)
	}
	if sys.Version != Version || sys.Install != installSource || sys.Database.Bytes <= 0 || sys.Database.Schema == "" {
		t.Fatalf("system = %s", body)
	}
	if len(sys.Tools) != 2 || sys.Tools[0].Name != "ffmpeg" || sys.Tools[0].Path != "" {
		t.Fatalf("tools (none configured) = %+v", sys.Tools)
	}
	if sys.Metadata.Enabled || sys.Metadata.Health != nil {
		t.Fatalf("metadata off must not be pinged: %+v", sys.Metadata)
	}
	if sys.TLS.Mode != "selfsigned" || sys.TLS.Error == "" { // no certificate written in a test env
		t.Fatalf("tls = %+v", sys.TLS)
	}
	if len(sys.Libraries) != 1 || !sys.Libraries[0].Available || sys.Libraries[0].Disk == nil || sys.Libraries[0].Disk.Total == 0 {
		t.Fatalf("libraries = %+v", sys.Libraries)
	}
	if sys.Update["enabled"] != false || sys.Update["install"] != installSource {
		t.Fatalf("no checker: update = %v", sys.Update)
	}
}

func TestLogsEndpoint(t *testing.T) {
	e := newTestEnv(t)
	ring := logring.NewRing(100)
	e.api.SetRuntime(Runtime{Logs: ring})
	log := slog.New(logring.NewHandler(slog.DiscardHandler, ring, slog.LevelInfo))
	log.Info("scan started", "library", "Fiction")
	log.Warn("root unavailable", "library", "Lectures")
	log.Error("ffmpeg failed", "token", "s3cret")
	adminTok, _ := opsTokens(t, e)

	var res struct {
		Entries []logring.Entry `json:"entries"`
		LastSeq uint64          `json:"last_seq"`
	}
	_, body := e.do(t, "GET", "/api/v1/admin/logs?level=warn", adminTok, "")
	_ = json.Unmarshal([]byte(body), &res)
	if len(res.Entries) != 2 || res.LastSeq != 3 || strings.Contains(body, "s3cret") {
		t.Fatalf("warn+ = %s", body)
	}
	_, body = e.do(t, "GET", "/api/v1/admin/logs?after=2", adminTok, "")
	_ = json.Unmarshal([]byte(body), &res)
	if len(res.Entries) != 1 || res.Entries[0].Message != "ffmpeg failed" {
		t.Fatalf("after=2 = %s", body)
	}
	_, body = e.do(t, "GET", "/api/v1/admin/logs?q=fiction", adminTok, "")
	_ = json.Unmarshal([]byte(body), &res)
	if len(res.Entries) != 1 {
		t.Fatalf("q=fiction = %s", body)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/admin/logs?level=loud", adminTok, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad level = %d, want 400", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/admin/logs?after=-1", adminTok, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad after = %d, want 400", resp.StatusCode)
	}
}
