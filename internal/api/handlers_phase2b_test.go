package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// A fresh server, where nobody has listened yet, sends an empty listening list,
// never null (the console iterates it).
func TestAdminStatsListsAreNeverNull(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	resp, body := e.do(t, "GET", "/api/v1/admin/stats", adminTok, "")
	if resp.StatusCode != 200 || !strings.Contains(body, `"listening":[]`) || strings.Contains(body, "null") {
		t.Fatalf("stats = %d %s", resp.StatusCode, body)
	}
}

// Admin book rows say whether a book is matched, by the matched= filter's rule.
func TestAdminBooksCarryMatched(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	libID, base := seedCatalog(t, e)
	if resp, body := e.do(t, "PATCH", base+"/book?path="+escape("Andy Weir/The Martian"), adminTok,
		`{"set":{"asin":"B00EMXBDMA"}}`); resp.StatusCode != 200 {
		t.Fatalf("edit = %d %s", resp.StatusCode, body)
	}
	for filter, want := range map[string]string{"true": "The Martian", "false": "Artemis"} {
		_, body := e.do(t, "GET", "/api/v1/admin/books?matched="+filter+"&library_id="+strconv.FormatInt(libID, 10), adminTok, "")
		if !strings.Contains(body, `"title":"`+want+`"`) || !strings.Contains(body, `"matched":`+filter) {
			t.Fatalf("matched=%s: %s", filter, body)
		}
	}
}

// POST /admin/shares/{id}/paths takes a batch of rules in one transaction: an
// admin's batch adds them all (allowed); a member is refused, an oversized batch
// is refused, and a batch with one bad rule adds nothing (denied).
func TestAddSharePathsBatch(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, memberTok, _ := adminAndMember(t, e)
	libID, _ := seedCatalog(t, e)
	ctx := context.Background()
	share, err := e.cat.CreateShare(ctx, catalog.Share{Name: "Martians"})
	if err != nil {
		t.Fatal(err)
	}
	url := "/api/v1/admin/shares/" + strconv.FormatInt(share.ID, 10) + "/paths"
	lib := strconv.FormatInt(libID, 10)
	rule := func(p string) string { return `{"library_id":` + lib + `,"path":"` + p + `"}` }
	paths := func() []catalog.PathRule {
		got, err := e.cat.ListSharePaths(ctx, share.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	batch := `{"rules":[` + rule("Andy Weir/The Martian") + `,` + rule("Andy Weir/Artemis") + `]}`
	if resp, _ := e.do(t, "POST", url, memberTok, batch); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member batch = %d, want 403", resp.StatusCode)
	}
	if got := paths(); len(got) != 0 {
		t.Fatalf("a refused batch added %v", got)
	}
	if resp, body := e.do(t, "POST", url, adminTok, `{"rules":[`+rule("A")+`,{"path":"B"}]}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("batch with a rule missing library_id = %d %s, want 400", resp.StatusCode, body)
	}
	if got := paths(); len(got) != 0 {
		t.Fatalf("a batch with a bad rule added %v", got)
	}
	many := make([]string, maxBulkBooks+1)
	for i := range many {
		many[i] = rule("p" + strconv.Itoa(i))
	}
	if resp, body := e.do(t, "POST", url, adminTok, `{"rules":[`+strings.Join(many, ",")+`]}`); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, codeTooLarge) {
		t.Fatalf("oversized batch = %d %s, want 400 too_large", resp.StatusCode, body)
	}
	// A share created with its rules is bounded the same way: nothing is made
	// past the cap, and a share at it is.
	create := func(name string, rules []string) (*http.Response, string) {
		return e.do(t, "POST", "/api/v1/admin/shares", adminTok, fmt.Sprintf(`{"name":%q,"paths":[%s]}`, name, strings.Join(rules, ",")))
	}
	if resp, body := create("Too many", many); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, codeTooLarge) {
		t.Fatalf("a share created with %d rules = %d %s, want 400 too_large", len(many), resp.StatusCode, body)
	}
	var made int
	if err := e.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM shares WHERE name = 'Too many'`).Scan(&made); err != nil || made != 0 {
		t.Fatalf("shares made by the refused request: %d, %v", made, err)
	}
	if resp, body := create("At the cap", many[:maxBulkBooks]); resp.StatusCode != http.StatusCreated {
		t.Fatalf("a share created with %d rules = %d %.200s, want 201", maxBulkBooks, resp.StatusCode, body)
	}

	if resp, body := e.do(t, "POST", url, adminTok, batch); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("admin batch = %d %s", resp.StatusCode, body)
	}
	if got := paths(); len(got) != 2 {
		t.Fatalf("paths after the batch = %v, want 2", got)
	}
	// The single-rule body still works (shipped consoles and scripts use it).
	if resp, _ := e.do(t, "POST", url, adminTok, rule("Andy Weir")); resp.StatusCode != http.StatusNoContent {
		t.Fatal("single-rule add failed")
	}
	if got := paths(); len(got) != 3 {
		t.Fatalf("paths after a single add = %v, want 3", got)
	}
}

// A share created with as many rules as one request takes, each path at
// catalog.MaxPathBytes, is made: its body is past the 1 MiB other admin
// bodies take, so the route reads more. A body past even that is the route's
// own 400 too_large, on the create and on an add of rules, never a bare 400.
func TestShareRulesBodyBound(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	libID, _ := seedCatalog(t, e)
	lib := strconv.FormatInt(libID, 10)
	long := func(i int) string {
		p := fmt.Sprintf("%04d/", i)
		return p + strings.Repeat("a", catalog.MaxPathBytes-len(p))
	}
	rules := make([]string, maxBulkBooks)
	for i := range rules {
		rules[i] = `{"library_id":` + lib + `,"path":"` + long(i) + `"}`
	}
	body := `{"name":"Long paths","paths":[` + strings.Join(rules, ",") + `]}`
	resp, out := e.do(t, "POST", "/api/v1/admin/shares", adminTok, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("a share with %d rules at the path cap (%d bytes) = %d %.200s, want 201", maxBulkBooks, len(body), resp.StatusCode, out)
	}
	share, err := e.cat.CreateShare(context.Background(), catalog.Share{Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	huge := strings.Repeat("a", shareRulesBodyMax)
	for route, body := range map[string]string{
		"/api/v1/admin/shares": `{"name":"Huge","description":"` + huge + `"}`,
		"/api/v1/admin/shares/" + strconv.FormatInt(share.ID, 10) + "/paths": `{"rules":[{"library_id":` + lib + `,"path":"` + huge + `"}]}`,
	} {
		if resp, out := e.do(t, "POST", route, adminTok, body); resp.StatusCode != http.StatusBadRequest || !strings.Contains(out, `"code":"`+codeTooLarge+`"`) {
			t.Errorf("POST %s with a body past the bound = %d %.200s, want 400 too_large", route, resp.StatusCode, out)
		}
	}
	// The body's room is for rules: a description past what a rename's body
	// carries is refused too, and no share is made.
	desc := `{"name":"Long description","description":"` + strings.Repeat("a", maxShareDescription+1) + `"}`
	if resp, out := e.do(t, "POST", "/api/v1/admin/shares", adminTok, desc); resp.StatusCode != http.StatusBadRequest || !strings.Contains(out, `"code":"`+codeTooLarge+`"`) {
		t.Errorf("a create with a description past %d bytes = %d %.200s, want 400 too_large", maxShareDescription, resp.StatusCode, out)
	}
	// A body that doesn't decode stays a plain 400.
	if resp, out := e.do(t, "POST", "/api/v1/admin/shares", adminTok, `{"name":`); resp.StatusCode != http.StatusBadRequest || strings.Contains(out, `"code"`) {
		t.Errorf("a malformed body = %d %s, want a plain 400", resp.StatusCode, out)
	}
}

// A share's name is at most maxShareName characters, counted as characters,
// not bytes: a longer one is refused (400 invalid_name) on a create and a
// rename before it is stored, audited or logged; one at the cap is kept.
func TestShareNameBound(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	atCap := strings.Repeat("é", maxShareName) // twice as many bytes
	over := atCap + "é"
	resp, out := e.do(t, "POST", "/api/v1/admin/shares", adminTok, `{"name":"`+over+`"}`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(out, `"code":"`+codeInvalidName+`"`) {
		t.Fatalf("a create with a name of %d characters = %d %s, want 400 invalid_name", maxShareName+1, resp.StatusCode, out)
	}
	var made int
	if err := e.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM shares`).Scan(&made); err != nil || made != 0 {
		t.Fatalf("shares made by the refused create: %d, %v", made, err)
	}
	resp, out = e.do(t, "POST", "/api/v1/admin/shares", adminTok, `{"name":"`+atCap+`"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("a create with a name of %d characters = %d %s, want 201", maxShareName, resp.StatusCode, out)
	}
	shares, err := e.cat.ListShares(context.Background())
	if err != nil || len(shares) != 1 {
		t.Fatalf("shares = %v, %v", shares, err)
	}
	url := "/api/v1/admin/shares/" + strconv.FormatInt(shares[0].ID, 10)
	resp, out = e.do(t, "PATCH", url, adminTok, `{"name":"`+over+`"}`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(out, `"code":"`+codeInvalidName+`"`) {
		t.Fatalf("a rename to %d characters = %d %s, want 400 invalid_name", maxShareName+1, resp.StatusCode, out)
	}
	if got, err := e.cat.GetShare(context.Background(), shares[0].ID); err != nil || got.Name != atCap {
		t.Fatalf("after the refused rename the name is %q, %v", got.Name, err)
	}
}
