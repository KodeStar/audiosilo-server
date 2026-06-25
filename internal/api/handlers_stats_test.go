package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// TestAdminStats covers the allowed+denied pair for GET /admin/stats: the admin
// gets catalog totals + the per-library and cross-user listening arrays; a
// non-admin is rejected by requireAdmin (403).
func TestAdminStats(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})

	// Seed a progress row so the cross-user "listening" feed has something to
	// join against (LEFT-joined to books on the path).
	adminTok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	libPath := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10)
	prog := libPath + "/progress?path=" + url.QueryEscape("Will Wight/Cradle/01 - Unsouled.m4b")
	if resp, body := e.do(t, "PUT", prog, adminTok, `{"position":12.5,"duration":120}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("seed progress = %d %s, want 200", resp.StatusCode, body)
	}

	// Allowed: the admin sees the dashboard shape.
	resp, body := e.do(t, "GET", "/api/v1/admin/stats", adminTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin stats = %d %s, want 200", resp.StatusCode, body)
	}
	var stats struct {
		TotalBooks     *int `json:"total_books"`
		TotalLibraries *int `json:"total_libraries"`
		TotalUsers     *int `json:"total_users"`
		Libraries      []struct {
			ID        int64  `json:"id"`
			Name      string `json:"name"`
			BookCount int    `json:"book_count"`
		} `json:"libraries"`
		Listening []struct {
			Path string `json:"path"`
		} `json:"listening"`
	}
	if err := json.Unmarshal([]byte(body), &stats); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if stats.TotalBooks == nil || stats.TotalLibraries == nil || stats.TotalUsers == nil {
		t.Fatalf("missing total_* fields: %s", body)
	}
	if *stats.TotalLibraries != 1 {
		t.Fatalf("total_libraries = %d, want 1 (%s)", *stats.TotalLibraries, body)
	}
	if *stats.TotalUsers < 1 {
		t.Fatalf("total_users = %d, want >=1 (%s)", *stats.TotalUsers, body)
	}
	if len(stats.Libraries) != 1 || stats.Libraries[0].Name != "Main" {
		t.Fatalf("libraries[] not populated: %s", body)
	}
	// The seeded progress surfaces in the cross-user listening feed.
	if len(stats.Listening) == 0 {
		t.Fatalf("expected the seeded progress in listening[]: %s", body)
	}

	// Denied: a non-admin is blocked by requireAdmin.
	member, _ := e.auth.CreateUser(ctx, "member", "member-password", auth.RoleUser)
	memberTok, _ := e.auth.IssueToken(ctx, member.ID, auth.KindSession, "t", 0)
	if resp, _ := e.do(t, "GET", "/api/v1/admin/stats", memberTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin stats = %d, want 403", resp.StatusCode)
	}
}
