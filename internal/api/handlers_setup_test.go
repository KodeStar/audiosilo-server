package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// TestSetupWizardAccountOnly: the /setup page asks only for the admin account
// and hands over to the console's setup flow; a POST without library fields
// creates the admin and no library (201 {user}), and the console's flow then
// opens (not finished). Sending only one of the two library fields is refused
// before anything is created. TestSetupWizard covers a POST with both, which
// still creates the library as before.
func TestSetupWizardAccountOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := storetest.Open(t)
	authSvc := auth.New(db, time.Now)
	cat := catalog.New(db, time.Now)
	a := New(config.Default(t.TempDir()), authSvc, cat, library.NewScanner(cat, "", slog.Default()), "", slog.Default())
	const tok = "test-setup-token"
	a.EnableSetup(tok)
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	do := func(method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	code, page := do("GET", "/setup", "")
	if code != http.StatusOK || !strings.Contains(page, `id="password"`) || !strings.Contains(page, `href="/admin">Continue setup`) {
		t.Fatalf("GET /setup = %d, want the account form and a Continue setup link to /admin", code)
	}
	if strings.Contains(page, "library-root") || strings.Contains(page, "library-name") {
		t.Error("the setup page still asks for the library")
	}

	for _, half := range []string{`"library_name":"Books"`, `"library_root":"` + t.TempDir() + `"`} {
		body := `{"token":"` + tok + `","password":"longenough",` + half + `}`
		if code, b := do("POST", "/setup", body); code != http.StatusBadRequest {
			t.Errorf("POST /setup with only %s = %d %s, want 400", half, code, b)
		}
	}
	if ok, _ := authSvc.AdminExists(ctx); ok {
		t.Fatal("a refused setup created an admin")
	}

	code, b := do("POST", "/setup", `{"token":"`+tok+`","username":"boss","password":"longenough"}`)
	if code != http.StatusCreated || !strings.Contains(b, `"user"`) || strings.Contains(b, `"library"`) {
		t.Fatalf("POST /setup account only = %d %s, want 201 with the user and no library", code, b)
	}
	if ok, _ := authSvc.AdminExists(ctx); !ok {
		t.Fatal("setup did not create the admin")
	}
	if libs, _ := cat.ListLibraries(ctx); len(libs) != 0 {
		t.Errorf("an account-only setup created libraries: %+v", libs)
	}
	if st, _ := cat.SetupState(ctx); st.Finished || len(st.Steps) != 0 {
		t.Error("the console's setup flow is marked finished on a fresh server")
	}
	if code, _ := do("POST", "/setup", `{"token":"`+tok+`","password":"longenough"}`); code != http.StatusConflict {
		t.Errorf("setup after completion = %d, want 409", code)
	}
}
