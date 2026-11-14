package launcher

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/store"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// hintFor runs a later start's hint check and returns what it printed.
func hintFor(t *testing.T, authSvc *auth.Service) string {
	t.Helper()
	var out bytes.Buffer
	signInHint(context.Background(), config.Default(t.TempDir()), authSvc, &out, discardLog())
	return out.String()
}

// The hint (server#369): a start after the banner prints it while no admin has
// signed in with a password, and stops once one has. The admin is named as the
// account is now.
func TestSignInHintUntilAnAdminSignsIn(t *testing.T) {
	ctx := context.Background()
	authSvc := auth.New(storetest.Open(t), time.Now)
	var banner bytes.Buffer
	created, err := ensureAdmin(ctx, config.Default(t.TempDir()), authSvc, &banner, discardLog())
	if err != nil || !created || !strings.Contains(banner.String(), "Admin password") {
		t.Fatalf("ensureAdmin = %v, %v; banner:\n%s", created, err, banner.String())
	}
	// The next start made nothing, and says the admin hasn't signed in.
	if again, err := ensureAdmin(ctx, config.Default(t.TempDir()), authSvc, &banner, discardLog()); err != nil || again {
		t.Fatalf("second ensureAdmin = %v, %v; want no new admin", again, err)
	}
	got := hintFor(t, authSvc)
	for _, want := range []string{"hasn't signed in yet", `as "admin"`, "reset-admin"} {
		if !strings.Contains(got, want) {
			t.Errorf("hint lacks %q:\n%s", want, got)
		}
	}
	// A sign-in by another admin counts as well: someone holds a password.
	if err := authSvc.NoteAdminSignIn(ctx); err != nil {
		t.Fatal(err)
	}
	if got := hintFor(t, authSvc); got != "" {
		t.Errorf("hint after a sign-in = %q, want none", got)
	}
}

// A first start that made the admin but failed before its banner (the auth
// code's write) printed the password nowhere: the admin is recorded all the
// same, so the next start's hint points at reset-admin.
func TestSignInHintAfterAStartThatFailedBeforeTheBanner(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	if _, err := db.ExecContext(ctx,
		`CREATE TRIGGER no_auth_codes BEFORE INSERT ON auth_codes BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	authSvc := auth.New(db, time.Now)
	var banner bytes.Buffer
	created, err := ensureAdmin(ctx, config.Default(t.TempDir()), authSvc, &banner, discardLog())
	if err == nil || created || banner.Len() > 0 {
		t.Fatalf("ensureAdmin = %v, %v; want a failure and no banner, got:\n%s", created, err, banner.String())
	}
	if got := hintFor(t, authSvc); !strings.Contains(got, `as "admin"`) || !strings.Contains(got, "reset-admin") {
		t.Errorf("hint after the failed start lacks the admin or reset-admin:\n%s", got)
	}
}

// No hint where no printed password was ever lost: an admin made by the --setup
// wizard (CreateUser, no MarkFirstAdmin), and an install from before the hint
// existed, whose admin has no row (the upgrade needs no backfill).
func TestNoSignInHintWithoutAPrintedPassword(t *testing.T) {
	ctx := context.Background()
	authSvc := auth.New(storetest.Open(t), time.Now)
	if _, err := authSvc.CreateUser(ctx, "alex", "a-chosen-password", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if got := hintFor(t, authSvc); got != "" {
		t.Errorf("hint = %q, want none", got)
	}
}

// The hint's own words: the docker exec form inside a container, the plain
// command outside one.
func TestWriteSignInHint(t *testing.T) {
	var out bytes.Buffer
	writeSignInHint(&out, "http://nas:8080", "admin", "audiosilo --data /data reset-admin", "docker exec -u 99:100 <container> ")
	for _, want := range []string{
		`Sign in at http://nas:8080/admin as "admin"`,
		"the container's log",
		"docker exec -u 99:100 <container> audiosilo --data /data reset-admin",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("container hint lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	writeSignInHint(&out, "https://books.example.com", "admin", "/opt/as/audiosilo --data /srv/as reset-admin", "")
	if s := out.String(); strings.Contains(s, "docker") || !strings.Contains(s, "\n   /opt/as/audiosilo --data /srv/as reset-admin\n") {
		t.Errorf("hint outside a container:\n%s", s)
	}
}

// docker exec runs the command as the server runs: as its user, or as root
// (no -u) when the server is root or its ids are unknown.
func TestExecUser(t *testing.T) {
	for _, tc := range []struct {
		uid, gid int
		want     string
	}{
		{99, 100, "-u 99:100 "},
		{1000, 0, "-u 1000:0 "},
		{0, 0, ""},
		{-1, -1, ""},
	} {
		if got := execUser(tc.uid, tc.gid); got != tc.want {
			t.Errorf("execUser(%d, %d) = %q, want %q", tc.uid, tc.gid, got, tc.want)
		}
	}
}

// The command names the binary as the shell finds it: by name when PATH finds
// this very binary (our image's /usr/local/bin), else by its path (the
// linuxserver image's /app/audiosilo/audiosilo), and quotes a folder with a space.
func TestResetAdminCommand(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "audiosilo")
	if err := os.WriteFile(exe, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	onPath := func(string) (string, error) { return exe, nil }
	notOnPath := func(string) (string, error) { return "", errors.New("not found") }
	elsewhere := func(string) (string, error) { return filepath.Join(t.TempDir(), "x"), nil }
	for _, tc := range []struct {
		name, data, exe string
		look            func(string) (string, error)
		want            string
	}{
		{"on PATH", "/data", exe, onPath, "audiosilo --data /data reset-admin"},
		{"not on PATH", "/config", exe, notOnPath, exe + " --data /config reset-admin"},
		{"another on PATH", "/config", exe, elsewhere, exe + " --data /config reset-admin"},
		{"unknown binary", "/data", "", onPath, "audiosilo --data /data reset-admin"},
		{"a space", "/srv/audio silo", exe, onPath, "audiosilo --data '/srv/audio silo' reset-admin"},
	} {
		if got := resetAdminCommand(tc.data, tc.exe, tc.look); got != tc.want {
			t.Errorf("%s: resetAdminCommand = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// On Windows a path's backslashes are plain and a word with a space takes
// double quotes: cmd.exe reads a single quote as part of the word.
func TestQuoteWord(t *testing.T) {
	for _, tc := range []struct{ goos, in, want string }{
		{"linux", "/data", "/data"},
		{"linux", "/srv/audio silo", "'/srv/audio silo'"},
		{"linux", "/srv/it's", `'/srv/it'\''s'`},
		{"windows", `C:\AudioSilo\audiosilo.exe`, `C:\AudioSilo\audiosilo.exe`},
		{"windows", `C:\Program Files\AudioSilo\data`, `"C:\Program Files\AudioSilo\data"`},
	} {
		if got := quoteWord(tc.goos, tc.in); got != tc.want {
			t.Errorf("quoteWord(%s, %q) = %q, want %q", tc.goos, tc.in, got, tc.want)
		}
	}
}

// passwordIn reads the password ResetAdmin printed.
var passwordLine = regexp.MustCompile(`(?m)^  (\S+)$`)

func passwordIn(t *testing.T, out string) string {
	t.Helper()
	m := passwordLine.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no password in:\n%s", out)
	}
	return m[1]
}

// resetDir is a data folder with a migrated database and the accounts named
// (role admin unless the name starts with "user"), returned with a handle on it
// as a running server would hold one.
func resetDir(t *testing.T, names ...string) (string, *auth.Service) {
	t.Helper()
	path := storetest.Path(t)
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	authSvc := auth.New(db, time.Now)
	for _, n := range names {
		role := auth.RoleAdmin
		if strings.HasPrefix(n, "user") {
			role = auth.RoleUser
		}
		if _, err := authSvc.CreateUser(context.Background(), n, "old-password-"+n, role); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Dir(path), authSvc
}

// reset-admin, run as a second process would beside a running server (its own
// database handle): the only admin, or the one named, gets a new password that
// the running server's sign-in takes at once, and the old one stops working.
func TestResetAdmin(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		accounts []string
		username string
		reset    string
	}{
		{"the only admin", []string{"admin", "user-sam"}, "", "admin"},
		{"the admin named", []string{"admin", "alex"}, "alex", "alex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, running := resetDir(t, tc.accounts...)
			var out bytes.Buffer
			if err := ResetAdmin(ctx, dir, tc.username, &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), `"`+tc.reset+`"`) {
				t.Errorf("output names no %q:\n%s", tc.reset, out.String())
			}
			pw := passwordIn(t, out.String())
			if _, err := running.Authenticate(ctx, tc.reset, pw); err != nil {
				t.Errorf("the running server refuses the new password: %v", err)
			}
			if _, err := running.Authenticate(ctx, tc.reset, "old-password-"+tc.reset); !errors.Is(err, auth.ErrInvalidCreds) {
				t.Errorf("old password: err = %v, want ErrInvalidCreds", err)
			}
			// Every other account keeps its password.
			for _, other := range tc.accounts {
				if other == tc.reset {
					continue
				}
				if _, err := running.Authenticate(ctx, other, "old-password-"+other); err != nil {
					t.Errorf("%s's password changed: %v", other, err)
				}
			}
		})
	}
}

// A reset ends the start's hint: it names the first start's password, which
// the reset replaced, and whoever ran the reset holds a working one.
func TestResetAdminEndsTheSignInHint(t *testing.T) {
	ctx := context.Background()
	path := storetest.Path(t)
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	authSvc := auth.New(db, time.Now)
	if _, err := ensureAdmin(ctx, config.Default(t.TempDir()), authSvc, io.Discard, discardLog()); err != nil {
		t.Fatal(err)
	}
	if hintFor(t, authSvc) == "" {
		t.Fatal("no hint before the reset")
	}
	if err := ResetAdmin(ctx, filepath.Dir(path), "", io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := hintFor(t, authSvc); got != "" {
		t.Errorf("the hint after a reset still names the first start's password:\n%s", got)
	}
}

// A session the running server already checked. reset-admin's write is another
// process's, which the server's token cache can't see, so the session ends
// when its row is next read: at the server's next content write, or once the
// session's own last-seen touch (a minute after the last) drops it from the
// cache, the request that makes the touch being the last one answered.
func TestResetAdminEndsARunningServersSessions(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		// then is what the running server does after the reset.
		then func(t *testing.T, running *auth.Service, secret string, clock *time.Time)
	}{
		{"its next write", func(t *testing.T, running *auth.Service, _ string, _ *time.Time) {
			if _, err := running.CreateUser(ctx, "user-sam", "", auth.RoleUser); err != nil {
				t.Fatal(err)
			}
		}},
		{"the session's next touch", func(_ *testing.T, running *auth.Service, secret string, clock *time.Time) {
			*clock = clock.Add(time.Minute)
			_, _ = running.ResolveToken(ctx, secret, auth.KindSession)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := storetest.Path(t)
			db, err := store.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			clock := time.Now()
			running := auth.New(db, func() time.Time { return clock })
			if _, err := running.CreateUser(ctx, "admin", "old-password", auth.RoleAdmin); err != nil {
				t.Fatal(err)
			}
			u, err := running.Authenticate(ctx, "admin", "old-password")
			if err != nil {
				t.Fatal(err)
			}
			secret, _, err := running.IssueSession(ctx, u, "browser", "")
			if err != nil {
				t.Fatal(err)
			}
			// Twice: the first use touches the row, the second leaves it cached.
			for range 2 {
				if _, err := running.ResolveToken(ctx, secret, auth.KindSession); err != nil {
					t.Fatal(err)
				}
			}
			if err := ResetAdmin(ctx, filepath.Dir(path), "", io.Discard); err != nil {
				t.Fatal(err)
			}
			tc.then(t, running, secret, &clock)
			if _, err := running.ResolveToken(ctx, secret, auth.KindSession); !errors.Is(err, auth.ErrInvalidToken) {
				t.Errorf("the old session after %s: err = %v, want ErrInvalidToken", tc.name, err)
			}
		})
	}
}

// The refusals: several admins and none named, a name that is no account or no
// admin (each a UsageError listing the admins), no admin at all, no database,
// and a disabled admin (errors).
func TestResetAdminRefusals(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		accounts []string
		username string
		usage    bool
		want     string
	}{
		{"several admins, none named", []string{"admin", "alex"}, "", true, "several admins: name one (audiosilo --data <folder> reset-admin <username>): admin, alex"},
		{"an unknown name", []string{"admin"}, "nobody", true, `no account named "nobody" (the admins: admin)`},
		{"not an admin", []string{"admin", "user-sam"}, "user-sam", true, `"user-sam" is not an admin`},
		{"no admin", []string{"user-sam"}, "", false, "no admin account yet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, running := resetDir(t, tc.accounts...)
			var out bytes.Buffer
			err := ResetAdmin(ctx, dir, tc.username, &out)
			var usage *UsageError
			if err == nil || errors.As(err, &usage) != tc.usage || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v (usage %v), want %q (usage %v)", err, errors.As(err, &usage), tc.want, tc.usage)
			}
			if out.Len() > 0 {
				t.Errorf("printed %q on a refusal", out.String())
			}
			for _, a := range tc.accounts {
				if _, err := running.Authenticate(ctx, a, "old-password-"+a); err != nil {
					t.Errorf("%s's password changed on a refusal: %v", a, err)
				}
			}
		})
	}

	t.Run("no database", func(t *testing.T) {
		dir := t.TempDir()
		err := ResetAdmin(ctx, dir, "", &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "no database at") {
			t.Fatalf("err = %v", err)
		}
		// Nothing made in a folder that held no server.
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("reset-admin left files in an empty folder: %v", entries)
		}
	})

	// A file that isn't an AudioSilo database is refused as it is, not
	// migrated into one.
	t.Run("an empty file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "audiosilo.db")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		err := ResetAdmin(ctx, dir, "", &bytes.Buffer{})
		if !errors.Is(err, store.ErrNotADatabase) {
			t.Fatalf("err = %v, want ErrNotADatabase", err)
		}
		if fi, err := os.Stat(path); err != nil || fi.Size() != 0 {
			t.Errorf("the file was written to: %v, %v", fi, err)
		}
	})

	t.Run("a disabled admin", func(t *testing.T) {
		dir, running := resetDir(t, "admin", "alex")
		alex, err := running.Authenticate(ctx, "alex", "old-password-alex")
		if err != nil {
			t.Fatal(err)
		}
		disabled := true
		if err := running.UpdateUser(ctx, alex.ID, auth.UserChanges{Disabled: &disabled}); err != nil {
			t.Fatal(err)
		}
		if err := ResetAdmin(ctx, dir, "alex", &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "is disabled") {
			t.Fatalf("err = %v, want the disabled refusal", err)
		}
	})
}

// Verification C1's own sequence, on a running server's handle: a console
// session made with the old password is in the server's token cache; the reset
// (another process) ends it in the database; the admin signs in with the new
// password, and from then on the old session is refused. Between the reset and
// the server's next write of its own, the running server still accepts a
// session it had cached (its cache follows its own writes only, store.DB
// ContentGeneration; the command says so); the sign-in is such a write.
func TestResetAdminBesideARunningServer(t *testing.T) {
	ctx := context.Background()
	dir, running := resetDir(t, "admin")
	u, err := running.Authenticate(ctx, "admin", "old-password-admin")
	if err != nil {
		t.Fatal(err)
	}
	old, _, err := running.IssueSession(ctx, u, "console", "")
	if err != nil {
		t.Fatal(err)
	}
	// Twice: the first use touches the token (last_seen), which drops its row
	// from the cache; the second puts it back, as a console in use holds it.
	for range 2 {
		if _, err := running.ResolveToken(ctx, old, auth.KindSession); err != nil {
			t.Fatalf("the old session doesn't work before the reset: %v", err)
		}
	}

	var out bytes.Buffer
	if err := ResetAdmin(ctx, dir, "", &out); err != nil {
		t.Fatal(err)
	}
	signedIn, err := running.Authenticate(ctx, "admin", passwordIn(t, out.String()))
	if err != nil {
		t.Fatalf("sign-in with the new password: %v", err)
	}
	fresh, _, err := running.IssueSession(ctx, signedIn, "console", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := running.ResolveToken(ctx, fresh, auth.KindSession); err != nil {
		t.Errorf("the new session: %v", err)
	}
	if _, err := running.ResolveToken(ctx, old, auth.KindSession); err == nil {
		t.Error("the old session still works after the reset and a new sign-in")
	}
}

// A database an older release left is refused, not migrated behind the running
// server's back: the server upgrades (and backs up) at its next start.
func TestResetAdminRefusesAnOlderDatabase(t *testing.T) {
	ctx := context.Background()
	dir, _ := resetDir(t, "admin")
	db, err := store.Open(ctx, filepath.Join(dir, "audiosilo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx,
		`DELETE FROM schema_migrations WHERE name = (SELECT MAX(name) FROM schema_migrations)`); err != nil {
		t.Fatal(err)
	}
	err = ResetAdmin(ctx, dir, "", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "from an older release") {
		t.Fatalf("err = %v, want the older-release refusal", err)
	}
}
