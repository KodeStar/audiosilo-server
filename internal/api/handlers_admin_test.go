package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/fsaccess"
	"github.com/kodestar/audiosilo-server/internal/library"
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
	t.Parallel()
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
			ID        int64              `json:"id"`
			Root      string             `json:"root"`
			BookCount *int               `json:"book_count"`
			Available *bool              `json:"available"`
			Access    *fsaccess.Identity `json:"folder_access"`
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
			if l.Access != nil {
				t.Errorf("online library carried folder access: %+v", l.Access)
			}
			if !*l.Available || *l.BookCount == 0 {
				t.Errorf("Main = available %v, %d books; want available with books", *l.Available, *l.BookCount)
			}
		case gone.ID:
			if user := fsaccess.User(); user != "" && (l.Access == nil || l.Access.ServerUser != user || l.Access.Owner != "") {
				t.Errorf("missing root: %+v", l.Access)
			}
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

// An admin gets the accounts seen by the bounded root probe, picker and
// setup preview. A listener cannot inspect server folder ownership.
func TestAdminFolderAccess(t *testing.T) {
	t.Parallel()
	if os.Geteuid() <= 0 {
		t.Skip("requires Unix permission checks as a non-root user")
	}
	e := newTestEnv(t)
	admin, member, _ := adminAndMember(t, e)
	root := t.TempDir()
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	want := fsaccess.IdentityOf(info)
	if err := os.Chmod(root, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	lib, err := e.cat.CreateLibrary(t.Context(), catalog.Library{Name: "Locked", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.api.scanner.Scan(t.Context(), *lib); !fsaccess.IsPermission(err) {
		t.Fatalf("scan of locked folder: %v", err)
	}
	for _, c := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/api/v1/admin/libraries", "", 200},
		{"GET", "/api/v1/admin/system", "", 200},
		{"GET", "/api/v1/admin/fs/dirs?path=" + url.QueryEscape(root), "", 404},
		{"POST", "/api/v1/admin/setup/preview", fmt.Sprintf(`{"root":%q}`, root), 404},
	} {
		resp, body := e.do(t, c.method, c.path, admin, c.body)
		var got struct {
			Access    *fsaccess.Identity `json:"folder_access"`
			Libraries []adminLibrary     `json:"libraries"`
		}
		if resp.StatusCode != c.status || json.Unmarshal([]byte(body), &got) != nil {
			t.Fatalf("%s: %d %s", c.path, resp.StatusCode, body)
		}
		if len(got.Libraries) == 1 {
			got.Access = got.Libraries[0].FolderAccess
		}
		if got.Access == nil || want.Owner == "" || *got.Access != *want {
			t.Errorf("%s: wanted %+v in %s", c.path, want, body)
		}
		resp, body = e.do(t, c.method, c.path, member, c.body)
		if resp.StatusCode != 403 || strings.Contains(body, "folder_access") {
			t.Errorf("member %s: %d %s", c.path, resp.StatusCode, body)
		}
	}
}

// TestScanStatusReportsUnavailableRoot: after a scan stops at the safety guard
// the status says so, so the console can explain why nothing changed.
func TestScanStatusReportsUnavailableRoot(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	e := newTestEnv(t)
	ctx := context.Background()
	adminTok, memberTok, memberID := adminAndMember(t, e)
	minted, err := e.auth.CreateInvite(ctx, memberID, "invite", 3, 0)
	code := minted.Code
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
	t.Parallel()
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

// A folder whose listing doesn't answer (a hung network mount) is 404
// folder_unreadable at the picker's time-out, not a request held for good
// (server#224). The setup preview's root check waits behind the same lister:
// with its one slot held by the hung listing, it is 404 at the time-out too.
func TestAdminListDirsHungMount(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	e.api.dirs = library.NewDirLister(func(string, int) (*library.DirListing, error) {
		<-hang
		return nil, errors.New("unreachable")
	}, 1, 50*time.Millisecond)
	resp, body := e.do(t, "GET", "/api/v1/admin/fs/dirs?path="+url.QueryEscape(t.TempDir()), adminTok, "")
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "folder_unreadable") {
		t.Fatalf("hung listing = %d %s, want 404 folder_unreadable", resp.StatusCode, body)
	}
	resp, body = e.do(t, "POST", "/api/v1/admin/setup/preview", adminTok, rootBody(t.TempDir()))
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "folder_unreadable") {
		t.Fatalf("setup preview with the lister's slot held = %d %s, want 404 folder_unreadable", resp.StatusCode, body)
	}
}

// TestAdminSharesListMembers: the share list says who each share is granted to.
func TestAdminSharesListMembers(t *testing.T) {
	t.Parallel()
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

// TestErrorCodes: failures a person can fix carry a machine-readable code next
// to the message, so the console doesn't depend on the English wording.
func TestErrorCodes(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	code := func(method, path, body string) (int, string) {
		resp, b := e.do(t, method, path, adminTok, body)
		var env struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		_ = json.Unmarshal([]byte(b), &env)
		if env.Error == "" {
			t.Errorf("%s %s: no error message in %s", method, path, b)
		}
		return resp.StatusCode, env.Code
	}
	for _, tc := range []struct {
		method, path, body string
		status             int
		code               string
	}{
		{"POST", "/api/v1/admin/users", `{"username":"sam","role":"user"}`, 409, "username_taken"},
		{"DELETE", "/api/v1/admin/users/" + strconv.FormatInt(e.adminID, 10), "", 400, "cannot_delete_self"},
		{"PATCH", "/api/v1/admin/users/" + strconv.FormatInt(e.adminID, 10), `{"role":"user"}`, 409, "last_admin"},
		{"PATCH", "/api/v1/admin/users/" + strconv.FormatInt(e.adminID, 10), `{"password":"short"}`, 400, "password_too_short"},
		{"GET", "/api/v1/admin/fs/dirs?path=relative", "", 400, "path_not_absolute"},
		{"GET", "/api/v1/admin/fs/dirs?path=" + url.QueryEscape(filepath.Join(t.TempDir(), "gone")), "", 404, "folder_unreadable"},
	} {
		if status, c := code(tc.method, tc.path, tc.body); status != tc.status || c != tc.code {
			t.Errorf("%s %s = %d %q, want %d %q", tc.method, tc.path, status, c, tc.status, tc.code)
		}
	}
	if _, err := e.cat.CreateShare(context.Background(), catalog.Share{Name: "Kids"}); err != nil {
		t.Fatal(err)
	}
	if status, c := code("POST", "/api/v1/admin/shares", `{"name":"Kids"}`); status != 409 || c != "name_taken" {
		t.Errorf("duplicate share = %d %q, want 409 name_taken", status, c)
	}
}

// The invite card needs to know whose certificate a visitor reaches. The
// hint changes neither the invite address nor who may mint or rotate an invite.
func TestInviteExternalHTTPS(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	admin, member, id := adminAndMember(t, e)
	for _, tc := range []struct {
		name, publicURL, trusted, header, value string
		external                                bool
	}{
		{"direct self-signed", "", "", "", "", false},
		{"trusted https", "", "127.0.0.1/32", "X-Forwarded-Proto", "https", true},
		{"trusted forwarded", "", "127.0.0.1/32", "Forwarded", `for=192.0.2.1;proto="https"`, true},
		{"untrusted header", "", "192.0.2.0/24", "X-Forwarded-Proto", "https", false},
		{"trusted http", "", "127.0.0.1/32", "X-Forwarded-Proto", "http", false},
		{"ambiguous chain", "", "127.0.0.1/32", "X-Forwarded-Proto", "https, http", false},
		{"public https", "https://books.example.com", "", "", "", true},
		{"public http", "http://books.example.com", "127.0.0.1/32", "X-Forwarded-Proto", "https", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := *e.cfg
			cfg.PublicURL = tc.publicURL
			if tc.trusted != "" {
				cfg.TrustedProxies = []string{tc.trusted}
			}
			e.api.live.Store(newLiveConfig(&cfg, &cfg))
			headers := map[string]string{}
			if tc.header != "" {
				headers[tc.header] = tc.value
			}
			check := func(path string, want int) {
				t.Helper()
				if resp, _ := e.doHeaders(t, "POST", path, member, "", headers); resp.StatusCode != http.StatusForbidden {
					t.Fatalf("member %s: %d", path, resp.StatusCode)
				}
				resp, body := e.doHeaders(t, "POST", path, admin, "", headers)
				var invite struct {
					External bool `json:"external_https"`
				}
				if resp.StatusCode != want || json.Unmarshal([]byte(body), &invite) != nil || invite.External != tc.external {
					t.Fatalf("admin %s: status %d, external_https %t, want %d, %t", path, resp.StatusCode, invite.External, want, tc.external)
				}
			}
			check(fmt.Sprintf("/api/v1/admin/users/%d/authcode", id), http.StatusCreated)
			invites, err := e.auth.ListInvites(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, invite := range invites {
				if invite.UserID == id {
					check(fmt.Sprintf("/api/v1/admin/authcodes/%d/rotate", invite.ID), http.StatusOK)
					return
				}
			}
			t.Fatal("created invite missing")
		})
	}
}

// TestInviteResponsesCarryLifetime: minting and rotating say how many devices
// the invite pairs and when it expires, so the console shows the server's values.
func TestInviteResponsesCarryLifetime(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _, memberID := adminAndMember(t, e)
	id := strconv.FormatInt(memberID, 10)
	type minted struct {
		AuthCode  string `json:"auth_code"`
		MaxUses   *int   `json:"max_uses"`
		ExpiresAt string `json:"expires_at"`
	}
	resp, body := e.do(t, "POST", "/api/v1/admin/users/"+id+"/authcode", adminTok, `{"max_uses":3,"ttl_days":7}`)
	var m minted
	if err := json.Unmarshal([]byte(body), &m); err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint = %d %s", resp.StatusCode, body)
	}
	exp, err := time.Parse(time.RFC3339, m.ExpiresAt)
	if m.MaxUses == nil || *m.MaxUses != 3 || err != nil || time.Until(exp) < 6*24*time.Hour {
		t.Fatalf("mint = %s, want 3 uses expiring in about 7 days", body)
	}

	invites, _ := e.auth.ListInvites(context.Background())
	resp, body = e.do(t, "POST", "/api/v1/admin/authcodes/"+strconv.FormatInt(invites[0].ID, 10)+"/rotate", adminTok, "")
	var r minted
	if err := json.Unmarshal([]byte(body), &r); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate = %d %s", resp.StatusCode, body)
	}
	if r.MaxUses == nil || *r.MaxUses != 3 || r.ExpiresAt == "" || r.AuthCode == m.AuthCode {
		t.Fatalf("rotate = %s, want a fresh code keeping 3 uses and an expiry", body)
	}

	resp, body = e.do(t, "POST", "/api/v1/admin/users/"+id+"/authcode", adminTok, `{"max_uses":0,"ttl_days":0}`)
	if resp.StatusCode != http.StatusCreated || strings.Contains(body, "expires_at") || !strings.Contains(body, `"max_uses":0`) {
		t.Fatalf("never-expiring mint = %s, want max_uses 0 and no expires_at", body)
	}
}

// TestWholeLibraryGrantsAreMarked: the share a whole-library grant creates
// carries whole_library_id; a share an admin fills with a whole library doesn't.
func TestWholeLibraryGrantsAreMarked(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	ctx := context.Background()
	adminTok, _, memberID := adminAndMember(t, e)
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Fiction", Root: t.TempDir()})
	if err := e.cat.GrantWholeLibrary(ctx, memberID, lib.ID); err != nil {
		t.Fatal(err)
	}
	mine, _ := e.cat.CreateShare(ctx, catalog.Share{Name: "Everything"})
	if err := e.cat.AddSharePath(ctx, mine.ID, catalog.PathRule{LibraryID: lib.ID, Path: ""}); err != nil {
		t.Fatal(err)
	}
	_, body := e.do(t, "GET", "/api/v1/admin/shares", adminTok, "")
	var out struct {
		Shares []struct {
			Name           string `json:"name"`
			WholeLibraryID *int64 `json:"whole_library_id"`
		} `json:"shares"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	for _, s := range out.Shares {
		switch s.Name {
		case "Library: Fiction":
			if s.WholeLibraryID == nil || *s.WholeLibraryID != lib.ID {
				t.Errorf("the grant's whole_library_id = %v, want %d", s.WholeLibraryID, lib.ID)
			}
		case "Everything":
			if s.WholeLibraryID != nil {
				t.Errorf("an admin's own share is marked as a library grant: %v", *s.WholeLibraryID)
			}
		}
	}
}

// TestLibraryGrantPathEditsRefused (server#254): a library grant's share
// changes only by granting or revoking the library. Denied: a path add or
// remove of its own rule on it is 409 library_grant and leaves the whole-library
// rule. Allowed: removing a stray rule from it (server#361), and the same edits
// on an admin's own share, are 204.
func TestLibraryGrantPathEditsRefused(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	ctx := context.Background()
	adminTok, _, memberID := adminAndMember(t, e)
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Fiction", Root: t.TempDir()})
	if err := e.cat.GrantWholeLibrary(ctx, memberID, lib.ID); err != nil {
		t.Fatal(err)
	}
	shares, _ := e.cat.ListShares(ctx)
	if len(shares) != 1 || shares[0].WholeLibraryID == nil {
		t.Fatalf("precondition: shares = %+v", shares)
	}
	mine, _ := e.cat.CreateShare(ctx, catalog.Share{Name: "Mine"})
	paths := func(id int64) string { return "/api/v1/admin/shares/" + strconv.FormatInt(id, 10) + "/paths" }
	folder := fmt.Sprintf(`{"library_id":%d,"path":"Author/Book"}`, lib.ID)
	whole := fmt.Sprintf(`{"library_id":%d,"path":""}`, lib.ID)

	for _, c := range []struct{ method, body string }{{"POST", folder}, {"DELETE", whole}} {
		resp, body := e.do(t, c.method, paths(shares[0].ID), adminTok, c.body)
		if resp.StatusCode != http.StatusConflict || !strings.Contains(body, `"code":"library_grant"`) {
			t.Fatalf("%s on a grant = %d %s, want 409 library_grant", c.method, resp.StatusCode, body)
		}
	}
	if got, _ := e.cat.GetShare(ctx, shares[0].ID); len(got.Paths) != 1 || got.Paths[0] != (catalog.PathRule{LibraryID: lib.ID}) {
		t.Fatalf("the grant's rules = %+v, want the whole library only", got.Paths)
	}
	// A stray rule an older version let onto the grant goes; its own rule stays.
	if err := e.cat.AddSharePath(ctx, shares[0].ID, catalog.PathRule{LibraryID: lib.ID, Path: "Author/Book"}); err != nil {
		t.Fatal(err)
	}
	if resp, body := e.do(t, "DELETE", paths(shares[0].ID), adminTok, folder); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE of a stray rule on a grant = %d %s, want 204", resp.StatusCode, body)
	}
	if got, _ := e.cat.GetShare(ctx, shares[0].ID); len(got.Paths) != 1 || got.Paths[0] != (catalog.PathRule{LibraryID: lib.ID}) {
		t.Fatalf("the grant's rules after its stray one went = %+v, want the whole library only", got.Paths)
	}
	for _, method := range []string{"POST", "DELETE"} {
		if resp, body := e.do(t, method, paths(mine.ID), adminTok, folder); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%s on an admin's share = %d %s, want 204", method, resp.StatusCode, body)
		}
	}
}

// TestAdminLibrariesCarryScanProgress: one poll of the list shows each scan.
func TestAdminLibrariesCarryScanProgress(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	ctx := context.Background()
	adminTok, _, _ := adminAndMember(t, e)
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "NAS", Root: filepath.Join(t.TempDir(), "gone")})
	if _, err := e.api.scanner.Scan(ctx, *lib); err == nil {
		t.Fatal("scan of a missing root succeeded")
	}
	_, body := e.do(t, "GET", "/api/v1/admin/libraries", adminTok, "")
	if !strings.Contains(body, `"scan":{"running":false`) || !strings.Contains(body, `"unavailable":true`) {
		t.Fatalf("libraries = %s, want each library's scan progress", body)
	}
}

// TestLibraryAccessLeavesAnAdminShareAlone (server#223): granting a library
// whole through POST /admin/library-access never widens an admin's own share
// that happens to be named "Library: <name>"; its holder keeps exactly its
// folder, and the grant goes to a share of its own.
func TestLibraryAccessLeavesAnAdminShareAlone(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	ctx := context.Background()
	adminTok, guestTok, guest := adminAndMember(t, e)
	lib := newFixtureLibrary(t, e)
	child, childTok := newUserWithSession(t, e, "child", false)
	mine, err := e.cat.CreateShare(ctx, catalog.Share{Name: "Library: Main",
		Paths: []catalog.PathRule{{LibraryID: lib.ID, Path: "Will Wight"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.cat.GrantShare(ctx, child, mine.ID); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"user_id":%d,"library_id":%d}`, guest, lib.ID)
	if resp, b := e.do(t, "POST", "/api/v1/admin/library-access", adminTok, body); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /admin/library-access = %d %s", resp.StatusCode, b)
	}

	// Denied: the child still reaches only their folder (the share's own rules are
	// catalog's TestGrantWholeLibraryNeverAdoptsAnAdminShare).
	if code := e.item(t, childTok, lib.ID, mistbornBook); code != http.StatusForbidden {
		t.Fatalf("the child reaches a book outside their share after the grant: %d", code)
	}
	if code := e.item(t, childTok, lib.ID, cradleBook); code != http.StatusOK {
		t.Fatalf("the child's own book = %d", code)
	}
	// Allowed: the guest gets the whole library through a marked grant of its own.
	if code := e.item(t, guestTok, lib.ID, mistbornBook); code != http.StatusOK {
		t.Fatalf("the guest's whole library = %d", code)
	}
	held, err := e.cat.UserShares(ctx, guest)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || held[0].ID == mine.ID || held[0].WholeLibraryID == nil || *held[0].WholeLibraryID != lib.ID {
		t.Fatalf("the guest's shares = %+v; want one marked grant, not the admin's", held)
	}
}
