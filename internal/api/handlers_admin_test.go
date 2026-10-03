package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// adminAndMember returns a session token for the env's admin and for a fresh
// non-admin account (the denied side of every admin endpoint test).
func adminAndMember(t *testing.T, e *testEnv) (adminTok, memberTok string, memberID int64) {
	t.Helper()
	ctx := context.Background()
	adminTok, err := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	if err != nil {
		t.Fatal(err)
	}
	member, err := e.auth.CreateUser(ctx, "sam", "sam-password", auth.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	memberTok, err = e.auth.IssueToken(ctx, member.ID, auth.KindSession, "t", 0)
	if err != nil {
		t.Fatal(err)
	}
	return adminTok, memberTok, member.ID
}

// TestAdminLibrariesAvailability: the admin list carries each library's book
// count and whether its root is reachable, so the console can show the "root
// unavailable" safety state; a non-admin is refused.
func TestAdminLibrariesAvailability(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	adminTok, memberTok, _ := adminAndMember(t, e)
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	main, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	gone, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "NAS", Root: filepath.Join(t.TempDir(), "unmounted")})
	if _, err := e.api.scanner.Scan(ctx, *main); err != nil {
		t.Fatal(err)
	}

	resp, body := e.do(t, "GET", "/api/v1/admin/libraries", adminTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin libraries = %d %s", resp.StatusCode, body)
	}
	var out struct {
		Libraries []struct {
			ID        int64  `json:"id"`
			Root      string `json:"root"`
			BookCount *int   `json:"book_count"`
			Available *bool  `json:"available"`
		} `json:"libraries"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Libraries) != 2 {
		t.Fatalf("got %d libraries: %s", len(out.Libraries), body)
	}
	for _, l := range out.Libraries {
		if l.BookCount == nil || l.Available == nil {
			t.Fatalf("library %d lacks book_count/available: %s", l.ID, body)
		}
		switch l.ID {
		case main.ID:
			if !*l.Available || *l.BookCount == 0 {
				t.Errorf("Main = available %v, %d books; want available with books", *l.Available, *l.BookCount)
			}
		case gone.ID:
			if *l.Available {
				t.Error("a library whose root is missing reads as available")
			}
		}
	}

	// The reorder response is the same enriched list.
	resp, body = e.do(t, "PUT", "/api/v1/admin/libraries/order", adminTok,
		`{"ids":[`+strconv.FormatInt(gone.ID, 10)+`,`+strconv.FormatInt(main.ID, 10)+`]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"available":false`) {
		t.Errorf("reorder = %d %s, want the enriched list", resp.StatusCode, body)
	}

	if resp, _ := e.do(t, "GET", "/api/v1/admin/libraries", memberTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin GET /admin/libraries = %d, want 403", resp.StatusCode)
	}
}

// TestScanStatusReportsUnavailableRoot: after a scan stops at the safety guard
// the status says so, so the console can explain why nothing changed.
func TestScanStatusReportsUnavailableRoot(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	adminTok, _, _ := adminAndMember(t, e)
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "NAS", Root: filepath.Join(t.TempDir(), "unmounted")})
	if _, err := e.api.scanner.Scan(ctx, *lib); err == nil {
		t.Fatal("scan of a missing root succeeded")
	}
	resp, body := e.do(t, "GET", "/api/v1/admin/libraries/"+strconv.FormatInt(lib.ID, 10)+"/scan", adminTok, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"unavailable":true`) {
		t.Fatalf("scan status = %d %s, want unavailable", resp.StatusCode, body)
	}
}

// TestAdminListInvites: every account's invites in one list, metadata only (the
// code itself is never stored, so it can't be listed); denied to non-admins.
func TestAdminListInvites(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	adminTok, memberTok, memberID := adminAndMember(t, e)
	code, err := e.auth.CreateInvite(ctx, memberID, "invite", 3, 0)
	if err != nil {
		t.Fatal(err)
	}

	resp, body := e.do(t, "GET", "/api/v1/admin/invites", adminTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("invites = %d %s", resp.StatusCode, body)
	}
	var out struct {
		Invites []struct {
			ID       int64  `json:"id"`
			UserID   int64  `json:"user_id"`
			Username string `json:"username"`
			MaxUses  int    `json:"max_uses"`
		} `json:"invites"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, inv := range out.Invites {
		if inv.UserID == memberID {
			found = inv.Username == "sam" && inv.MaxUses == 3
		}
	}
	if !found {
		t.Errorf("sam's invite missing or wrong: %s", body)
	}
	if strings.Contains(body, code) {
		t.Error("the invite list leaks the plaintext code")
	}

	if resp, _ := e.do(t, "GET", "/api/v1/admin/invites", memberTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin GET /admin/invites = %d, want 403", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/admin/invites", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous GET /admin/invites = %d, want 401", resp.StatusCode)
	}
}

// TestAdminListDirs covers the add-library folder picker: an admin lists the
// folders (never files) of an absolute path; relative paths, missing folders and
// non-admins are refused.
func TestAdminListDirs(t *testing.T) {
	e := newTestEnv(t)
	adminTok, memberTok, _ := adminAndMember(t, e)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "audiobooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	list := func(tok, path string) (int, string) {
		resp, body := e.do(t, "GET", "/api/v1/admin/fs/dirs?path="+url.QueryEscape(path), tok, "")
		return resp.StatusCode, body
	}

	code, body := list(adminTok, dir)
	if code != http.StatusOK || !strings.Contains(body, `"name":"audiobooks"`) {
		t.Fatalf("admin listing = %d %s", code, body)
	}
	if strings.Contains(body, "secret.txt") {
		t.Error("the folder picker listed a file")
	}
	if code, _ := list(adminTok, ""); code != http.StatusOK {
		t.Errorf("listing the filesystem root = %d, want 200", code)
	}

	if code, _ := list(adminTok, "relative/dir"); code != http.StatusBadRequest {
		t.Errorf("relative path = %d, want 400", code)
	}
	if code, body := list(adminTok, filepath.Join(dir, "missing")); code != http.StatusNotFound || strings.Contains(body, dir) {
		t.Errorf("missing folder = %d %s, want 404 without the path echoed", code, body)
	}
	if code, _ := list(memberTok, dir); code != http.StatusForbidden {
		t.Errorf("non-admin listing = %d, want 403", code)
	}
	if code, _ := list("", dir); code != http.StatusUnauthorized {
		t.Errorf("anonymous listing = %d, want 401", code)
	}
}

// TestAdminSharesListMembers: the share list says who each share is granted to.
func TestAdminSharesListMembers(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	adminTok, memberTok, memberID := adminAndMember(t, e)
	kids, _ := e.cat.CreateShare(ctx, catalog.Share{Name: "Kids"})
	if err := e.cat.GrantShare(ctx, memberID, kids.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cat.CreateShare(ctx, catalog.Share{Name: "Empty"}); err != nil {
		t.Fatal(err)
	}

	resp, body := e.do(t, "GET", "/api/v1/admin/shares", adminTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("shares = %d %s", resp.StatusCode, body)
	}
	var out struct {
		Shares []struct {
			Name      string  `json:"name"`
			MemberIDs []int64 `json:"member_ids"`
		} `json:"shares"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	for _, s := range out.Shares {
		switch s.Name {
		case "Kids":
			if len(s.MemberIDs) != 1 || s.MemberIDs[0] != memberID {
				t.Errorf("Kids member_ids = %v, want [%d]", s.MemberIDs, memberID)
			}
		case "Empty":
			if s.MemberIDs == nil || len(s.MemberIDs) != 0 {
				t.Errorf("Empty member_ids = %v, want [] (never null)", s.MemberIDs)
			}
		}
	}
	if resp, _ := e.do(t, "GET", "/api/v1/admin/shares", memberTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin GET /admin/shares = %d, want 403", resp.StatusCode)
	}
}
