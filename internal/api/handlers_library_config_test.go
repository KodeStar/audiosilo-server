package api

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
)

// A library config.yaml declares keeps its declared name and folder (UPG-01):
// every start upserts it by name, so a rename would come back as a second
// library on the next restart. PATCH refuses changing either with 409
// config_library, still takes its other settings (and the same name and
// folder resent, as the console's form sends them), and renames a library the
// console added as before. A member is refused whatever the library.
func TestUpdateConfigLibrary(t *testing.T) {
	t.Parallel()
	kidsRoot := t.TempDir()
	e := newTestEnvWith(t, func(c *config.Config) {
		c.Libraries = []config.Library{{Name: "Kids", Root: kidsRoot}}
	})
	adminTok, memberTok := opsTokens(t, e)
	ctx := context.Background()
	kids, err := e.cat.UpsertLibraryByName(ctx, catalog.Library{Name: "Kids", Root: kidsRoot})
	if err != nil {
		t.Fatal(err)
	}
	own, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Own", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	patch := func(tok string, id int64, body string) (int, string) {
		t.Helper()
		resp, out := e.do(t, "PATCH", fmt.Sprintf("/api/v1/admin/libraries/%d", id), tok, body)
		return resp.StatusCode, out
	}

	// Denied: a rename, and a new folder, of the config library.
	for _, body := range []string{
		`{"name":"Kids two"}`,
		fmt.Sprintf(`{"name":"Kids","root":%q}`, t.TempDir()),
	} {
		if code, out := patch(adminTok, kids.ID, body); code != http.StatusConflict || !strings.Contains(out, `"code":"config_library"`) {
			t.Errorf("PATCH %s = %d %s, want 409 config_library", body, code, out)
		}
	}
	if got, _ := e.cat.GetLibrary(ctx, kids.ID); got.Name != "Kids" || filepath.Clean(got.Root) != filepath.Clean(kidsRoot) {
		t.Errorf("after the refusals: %q at %q, want Kids at %q", got.Name, got.Root, kidsRoot)
	}

	// Allowed: the same name and folder resent with another setting.
	body := fmt.Sprintf(`{"name":"Kids","root":%q,"scan_schedule":"every:6h"}`, kidsRoot+string(filepath.Separator))
	if code, out := patch(adminTok, kids.ID, body); code != http.StatusOK {
		t.Errorf("PATCH unchanged name/root + schedule = %d %s, want 200", code, out)
	}
	if got, _ := e.cat.GetLibrary(ctx, kids.ID); got.ScanSchedule != "every:6h" {
		t.Errorf("schedule %q, want every:6h", got.ScanSchedule)
	}

	// Allowed: a library the console added renames as before.
	if code, out := patch(adminTok, own.ID, `{"name":"Own two"}`); code != http.StatusOK {
		t.Errorf("rename a console library = %d %s, want 200", code, out)
	}

	// Denied: a member, for either library.
	for _, id := range []int64{kids.ID, own.ID} {
		if code, _ := patch(memberTok, id, `{"name":"Mine"}`); code != http.StatusForbidden {
			t.Errorf("member PATCH library %d = %d, want 403", id, code)
		}
	}
}
