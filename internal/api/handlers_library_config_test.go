package api

import (
	"context"
	"errors"
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
// folder resent, as the console's form sends them, which write nothing), and
// renames a library the console added as before, but not to a declared name
// with another folder (409 name_taken: the next start would re-root it), nor
// does POST add one so. A member is refused whatever the library.
func TestUpdateConfigLibrary(t *testing.T) {
	t.Parallel()
	kidsRoot, goneRoot := t.TempDir(), t.TempDir()
	e := newTestEnvWith(t, func(c *config.Config) {
		// "Gone" is declared but has no row: deleted since the start.
		c.Libraries = []config.Library{{Name: "Kids", Root: kidsRoot}, {Name: "Gone", Root: goneRoot}}
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

	// Allowed: the same name and folder resent with another setting. The folder
	// respelled is the same folder: nothing stored as typed, no rescan.
	body := fmt.Sprintf(`{"name":"Kids","root":%q,"scan_schedule":"every:6h"}`, kidsRoot+string(filepath.Separator))
	if code, out := patch(adminTok, kids.ID, body); code != http.StatusOK || strings.Contains(out, `"job"`) {
		t.Errorf("PATCH unchanged name/root + schedule = %d %s, want 200 without a job", code, out)
	}
	if got, _ := e.cat.GetLibrary(ctx, kids.ID); got.ScanSchedule != "every:6h" || got.Root != kidsRoot {
		t.Errorf("schedule %q at %q, want every:6h at %q", got.ScanSchedule, got.Root, kidsRoot)
	}

	// Allowed: a library the console added renames as before.
	if code, out := patch(adminTok, own.ID, `{"name":"Own two"}`); code != http.StatusOK {
		t.Errorf("rename a console library = %d %s, want 200", code, out)
	}

	// Denied: a console library renamed to a declared name with another folder,
	// or a library added so; allowed at the declared folder.
	if code, out := patch(adminTok, own.ID, `{"name":"Gone"}`); code != http.StatusConflict || !strings.Contains(out, `"code":"name_taken"`) {
		t.Errorf("rename to a declared name = %d %s, want 409 name_taken", code, out)
	}
	if got, _ := e.cat.GetLibrary(ctx, own.ID); got.Name != "Own two" {
		t.Errorf("after the refusal: %q, want Own two", got.Name)
	}
	create := func(root string) (int, string) {
		t.Helper()
		resp, out := e.do(t, "POST", "/api/v1/admin/libraries", adminTok, fmt.Sprintf(`{"name":"Gone","root":%q}`, root))
		return resp.StatusCode, out
	}
	if code, out := create(t.TempDir()); code != http.StatusConflict || !strings.Contains(out, `"code":"name_taken"`) {
		t.Errorf("add a declared name at another folder = %d %s, want 409 name_taken", code, out)
	}
	if code, out := create(goneRoot); code != http.StatusCreated {
		t.Errorf("add a declared name at its folder = %d %s, want 201", code, out)
	}

	// Denied: a member, for either library.
	for _, id := range []int64{kids.ID, own.ID} {
		if code, _ := patch(memberTok, id, `{"name":"Mine"}`); code != http.StatusForbidden {
			t.Errorf("member PATCH library %d = %d, want 403", id, code)
		}
	}
}

// A library config.yaml declares can't be deleted from the console (server#176):
// the next start would add it back empty, everyone's progress in it gone. DELETE
// answers 409 config_library and drops nothing; a library the console added
// still deletes, and so does any library on a server whose config declares none.
func TestDeleteConfigLibrary(t *testing.T) {
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
	del := func(tok string, id int64) (int, string) {
		t.Helper()
		resp, out := e.do(t, "DELETE", fmt.Sprintf("/api/v1/admin/libraries/%d", id), tok, "")
		return resp.StatusCode, out
	}

	// Denied: the config library, and a member for either.
	if code, out := del(adminTok, kids.ID); code != http.StatusConflict || !strings.Contains(out, `"code":"config_library"`) {
		t.Errorf("DELETE config library = %d %s, want 409 config_library", code, out)
	}
	for _, id := range []int64{kids.ID, own.ID} {
		if code, _ := del(memberTok, id); code != http.StatusForbidden {
			t.Errorf("member DELETE library %d = %d, want 403", id, code)
		}
	}
	if got, err := e.cat.GetLibrary(ctx, kids.ID); err != nil || got.Name != "Kids" {
		t.Errorf("after the refusal: %v %v, want Kids kept", got, err)
	}

	// Allowed: a library the console added.
	if code, out := del(adminTok, own.ID); code != http.StatusNoContent {
		t.Errorf("DELETE console library = %d %s, want 204", code, out)
	}
	if _, err := e.cat.GetLibrary(ctx, own.ID); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("console library after delete: %v, want not found", err)
	}
}
