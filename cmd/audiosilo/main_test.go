package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/store"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

func TestCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args        []string
		code        int
		done        bool
		out, errOut string
	}{
		{args: nil, code: 0, done: false},
		{args: []string{"version"}, code: 0, done: true, out: "audiosilo dev\n"},
		{args: []string{"foo"}, code: 2, done: true, errOut: `unknown command "foo" (the commands are "version" and "reset-admin"`},
		{args: []string{"version", "extra"}, code: 2, done: true, errOut: `"version" takes no arguments`},
		{args: []string{"reset-admin", "a", "b"}, code: 2, done: true, errOut: "usage: audiosilo [--data <folder>] reset-admin [username]"},
		{args: []string{"reset-admin", "--data", "/x"}, code: 2, done: true, errOut: "flags go before the command"},
	} {
		var out, errOut bytes.Buffer
		code, done := command(tc.args, t.TempDir(), &out, &errOut)
		if code != tc.code || done != tc.done {
			t.Errorf("command(%q) = %d, %v; want %d, %v", tc.args, code, done, tc.code, tc.done)
		}
		if out.String() != tc.out {
			t.Errorf("command(%q) stdout = %q, want %q", tc.args, out.String(), tc.out)
		}
		if !strings.Contains(errOut.String(), tc.errOut) || (tc.errOut == "" && errOut.Len() > 0) {
			t.Errorf("command(%q) stderr = %q, want %q", tc.args, errOut.String(), tc.errOut)
		}
	}
}

// reset-admin's exit codes (launcher.ResetAdmin has the cases themselves): 0
// with the password on stdout, 2 when asked wrongly (several admins and none
// named, an unknown name), 1 when it can't (no database, no admin).
func TestResetAdminExitCodes(t *testing.T) {
	ctx := context.Background()
	dataDir := func(admins ...string) string {
		path := storetest.Path(t)
		db, err := store.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		for _, name := range admins {
			if _, err := auth.New(db, time.Now).CreateUser(ctx, name, "a-long-password", auth.RoleAdmin); err != nil {
				t.Fatal(err)
			}
		}
		return filepath.Dir(path)
	}
	for _, tc := range []struct {
		name   string
		dir    string
		args   []string
		code   int
		out    string
		errOut string
	}{
		{"the only admin", dataDir("admin"), []string{"reset-admin"}, 0, `New password for the admin account "admin"`, ""},
		{"the admin named", dataDir("admin", "alex"), []string{"reset-admin", "alex"}, 0, `"alex"`, ""},
		{"several admins", dataDir("admin", "alex"), []string{"reset-admin"}, 2, "", "several admins"},
		{"an unknown name", dataDir("admin"), []string{"reset-admin", "nobody"}, 2, "", `no account named "nobody"`},
		{"no admin", dataDir(), []string{"reset-admin"}, 1, "", "no admin account yet"},
		{"no database", t.TempDir(), []string{"reset-admin"}, 1, "", "no database at"},
	} {
		var out, errOut bytes.Buffer
		code, done := command(tc.args, tc.dir, &out, &errOut)
		if !done || code != tc.code {
			t.Errorf("%s: code %d (done %v), want %d; stderr %q", tc.name, code, done, tc.code, errOut.String())
		}
		if !strings.Contains(out.String(), tc.out) || tc.out == "" && out.Len() > 0 {
			t.Errorf("%s: stdout %q, want %q", tc.name, out.String(), tc.out)
		}
		if !strings.Contains(errOut.String(), tc.errOut) || tc.errOut == "" && errOut.Len() > 0 {
			t.Errorf("%s: stderr %q, want %q", tc.name, errOut.String(), tc.errOut)
		}
	}
}
