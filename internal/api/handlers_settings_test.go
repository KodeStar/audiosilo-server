package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/config"
)

// TestAdminSettingsMetadataToggle drives the full runtime toggle: GET reflects
// config, PATCH off flips the /server capability AND makes /meta 404, PATCH on
// restores both. It uses a mock metaserve so the meta lookup provably works
// before the flip and again after.
func TestAdminSettingsMetadataToggle(t *testing.T) {
	e := newMetaEnv(t, true, 0)
	libID := seedBook(t, e, "Andy Weir/The Martian", "B00FLIJJSY")
	adminTok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	metaPath := "/api/v1/libraries/" + strconv.FormatInt(libID, 10) + "/meta?path=" + escape("Andy Weir/The Martian")

	// GET settings reflects the enabled, configured service.
	resp, body := e.do(t, "GET", "/api/v1/admin/settings", adminTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get settings = %d %s, want 200", resp.StatusCode, body)
	}
	for _, want := range []string{`"metadata"`, `"enabled":true`, `"available":true`, e.cfg.Metadata.BaseURL} {
		if !strings.Contains(body, want) {
			t.Fatalf("settings envelope missing %q: %s", want, body)
		}
	}

	// Before the flip: capability true and the lookup works.
	if _, si := e.do(t, "GET", "/api/v1/server", "", ""); !strings.Contains(si, `"metadata":true`) {
		t.Fatalf("expected capability metadata:true, got %s", si)
	}
	if r, b := e.do(t, "GET", metaPath, adminTok, ""); r.StatusCode != http.StatusOK || !strings.Contains(b, `"matched":true`) {
		t.Fatalf("meta before flip = %d %s, want 200 matched", r.StatusCode, b)
	}

	// PATCH off: response shows disabled.
	resp, body = e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"metadata":{"enabled":false}}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"enabled":false`) {
		t.Fatalf("patch off = %d %s, want 200 enabled:false", resp.StatusCode, body)
	}
	// Capability flips false AND the lookup 404s.
	if _, si := e.do(t, "GET", "/api/v1/server", "", ""); !strings.Contains(si, `"metadata":false`) {
		t.Fatalf("expected capability metadata:false after off, got %s", si)
	}
	if r, b := e.do(t, "GET", metaPath, adminTok, ""); r.StatusCode != http.StatusNotFound {
		t.Fatalf("meta while off = %d %s, want 404", r.StatusCode, b)
	}

	// PATCH on: capability true and the lookup works again.
	if r, b := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"metadata":{"enabled":true}}`); r.StatusCode != http.StatusOK || !strings.Contains(b, `"enabled":true`) {
		t.Fatalf("patch on = %d %s, want 200 enabled:true", r.StatusCode, b)
	}
	if _, si := e.do(t, "GET", "/api/v1/server", "", ""); !strings.Contains(si, `"metadata":true`) {
		t.Fatalf("expected capability metadata:true after on, got %s", si)
	}
	if r, b := e.do(t, "GET", metaPath, adminTok, ""); r.StatusCode != http.StatusOK || !strings.Contains(b, `"matched":true`) {
		t.Fatalf("meta after on = %d %s, want 200 matched", r.StatusCode, b)
	}
}

// TestAdminSettingsEnableWithoutBaseURL: enabling the lookup when no base_url is
// configured (the service was never constructed) is a 400, and the envelope
// reports it as unavailable.
func TestAdminSettingsEnableWithoutBaseURL(t *testing.T) {
	e := newTestEnvWith(t, func(c *config.Config) {
		c.Metadata.Enabled = false
		c.Metadata.BaseURL = ""
	})
	adminTok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)

	if _, body := e.do(t, "GET", "/api/v1/admin/settings", adminTok, ""); !strings.Contains(body, `"available":false`) || !strings.Contains(body, `"enabled":false`) {
		t.Fatalf("expected unavailable+disabled envelope, got %s", body)
	}
	resp, body := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"metadata":{"enabled":true}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("enable without base_url = %d %s, want 400", resp.StatusCode, body)
	}
	// Still disabled + still no capability.
	if _, si := e.do(t, "GET", "/api/v1/server", "", ""); !strings.Contains(si, `"metadata":false`) {
		t.Fatalf("capability should stay false, got %s", si)
	}
}

// TestAdminSettingsRequiresAdmin is the required allowed+denied security pair:
// both endpoints refuse a non-admin (403) and serve an admin (200).
func TestAdminSettingsRequiresAdmin(t *testing.T) {
	e := newMetaEnv(t, true, 0)
	ctx := context.Background()
	member, _ := e.auth.CreateUser(ctx, "member", "member-password", auth.RoleUser)
	memberTok, _ := e.auth.IssueToken(ctx, member.ID, auth.KindSession, "t", 0)
	adminTok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)

	for _, tc := range []struct{ method, body string }{
		{"GET", ""},
		{"PATCH", `{"metadata":{"enabled":false}}`},
	} {
		if resp, _ := e.do(t, tc.method, "/api/v1/admin/settings", memberTok, tc.body); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s settings as non-admin = %d, want 403", tc.method, resp.StatusCode)
		}
		if resp, b := e.do(t, tc.method, "/api/v1/admin/settings", adminTok, tc.body); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s settings as admin = %d %s, want 200", tc.method, resp.StatusCode, b)
		}
	}
}

// TestAdminSettingsPersisted: a PATCH writes config.yaml, so re-Loading the config
// from the data dir reflects the new value (survives a restart).
func TestAdminSettingsPersisted(t *testing.T) {
	e := newMetaEnv(t, true, 0)
	adminTok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)

	if resp, b := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"metadata":{"enabled":false}}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("patch off = %d %s, want 200", resp.StatusCode, b)
	}
	loaded, _, err := config.Load(e.cfg.DataDir)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if loaded.Metadata.Enabled {
		t.Fatalf("metadata.enabled not persisted as false: %+v", loaded.Metadata)
	}
}
