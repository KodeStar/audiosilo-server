package api

import (
	"context"
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
