package launcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/backup"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/fsaccess"
	"github.com/kodestar/audiosilo-server/internal/store"
)

// The first-run admin after its banner (server#369): the hint a later start
// prints while that admin has never signed in, and the reset-admin command it
// names, for a password lost with the log (a recreated container).

// signInHint prints the hint when the admin ensureAdmin made has not signed in
// with a password yet (auth.FirstAdminWaiting). A failed read is logged and
// prints nothing: the hint is a courtesy, never a reason not to start.
func signInHint(ctx context.Context, cfg *config.Config, authSvc *auth.Service, w io.Writer, log *slog.Logger) {
	u, err := authSvc.FirstAdminWaiting(ctx)
	if err != nil {
		log.Warn("reading whether the admin has signed in failed", "err", err)
		return
	}
	if u == nil {
		return
	}
	container := ""
	if fsaccess.InContainer() {
		container = "docker exec " + execUser(os.Getuid(), os.Getgid()) + "<container> "
	}
	writeSignInHint(w, baseURL(cfg), u.Username, resetAdminCommand(cfg.DataDir, executable(), exec.LookPath), container)
}

// execUser is docker exec's -u for a server running as uid:gid, so the
// command runs as the server does (docker exec is root otherwise: a file it
// made in the data folder would be root's); "" when the server is root or the
// ids are unknown (Windows: -1).
func execUser(uid, gid int) string {
	if uid <= 0 || gid < 0 {
		return ""
	}
	return fmt.Sprintf("-u %d:%d ", uid, gid)
}

// writeSignInHint writes the hint: where to sign in and as whom, where the
// password was printed, and the command that sets a new one (command, as run
// where the server runs; container: "" outside a container, else the docker
// exec prefix that runs it from the host).
func writeSignInHint(w io.Writer, base, username, command, container string) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("\n========================================================\n")
	p(" AudioSilo: the admin account hasn't signed in yet\n")
	p("========================================================\n")
	p(" Sign in at %s/admin as %q with the password printed\n", base, username)
	if container != "" {
		p(" when this server first started (the start of the container's log).\n")
	} else {
		p(" when this server first started (the start of its log).\n")
	}
	p(" Lost it? Set a new one:\n")
	p("   %s%s\n", container, command)
	p("========================================================\n")
}

// executable is the running binary's path ("" when it can't be read).
func executable() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}

// resetAdminCommand is the reset-admin command line for this server: its own
// binary (by name when that name on PATH is this binary, else by its path: the
// linuxserver image's /app/audiosilo/audiosilo is not on PATH) and its data
// folder.
func resetAdminCommand(dataDir, exe string, lookPath func(string) (string, error)) string {
	bin := "audiosilo"
	if exe != "" {
		bin = exe
		if found, err := lookPath(filepath.Base(exe)); err == nil && sameFile(found, exe) {
			bin = filepath.Base(exe)
		}
	}
	return shellQuote(bin) + " --data " + shellQuote(dataDir) + " reset-admin"
}

func sameFile(a, b string) bool {
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	return err == nil && os.SameFile(sa, sb)
}

// plainWord is a word a POSIX shell takes as it is; plainWindowsWord one
// Windows' shells do (a path's backslashes are plain there).
var (
	plainWord        = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)
	plainWindowsWord = regexp.MustCompile(`^[A-Za-z0-9_@+=:,./\\-]+$`)
)

// shellQuote is s as one word of the shell the admin types the command into
// on this system (quoteWord).
func shellQuote(s string) string { return quoteWord(runtime.GOOS, s) }

// quoteWord is s as one shell word on goos: as it is when plain, else in
// single quotes for a POSIX shell, in double quotes on Windows (cmd.exe takes
// a single quote as part of the word, and a path there holds no double quote).
func quoteWord(goos, s string) string {
	if goos == "windows" {
		if plainWindowsWord.MatchString(s) {
			return s
		}
		return `"` + s + `"`
	}
	if plainWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// UsageError is a reset-admin refusal about how it was asked (no such admin,
// several to choose from): the command exits 2, as for a bad flag.
type UsageError struct{ msg string }

func (e *UsageError) Error() string { return e.msg }

// ResetAdmin is the reset-admin command: it sets a new random password on an
// admin account of the server whose data folder is dataDir and writes it to w.
// username picks the admin; "" picks the only one (a UsageError, listing them,
// when there are several). The password is set as the console sets one
// (auth.SetPassword, nothing kept): the account's sessions, open pairing codes
// and recovery code end and its browsers are forgotten, and the start's sign-in
// hint ends with them. It runs beside a running server (SQLite in WAL mode
// waits its turn): that server checks the new password at once, since a
// sign-in reads the database, but its token cache can't see this process's
// write, so a session it already checked is refused only once its row is read
// again: at the server's next content write, or a minute after it last read
// the row (auth's maxCachedTokenAge). It touches
// nothing else: no config.yaml written, no folder made, a file that isn't an
// AudioSilo database (an empty one included) refused before it is opened for
// writing, and a database an older release left (migrations pending) refused
// too, for the server to upgrade (and back up) at its next start.
func ResetAdmin(ctx context.Context, dataDir, username string, w io.Writer) error {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	dbPath := filepath.Join(abs, "audiosilo.db")
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no database at %s: name the server's data folder with --data before reset-admin", dbPath)
	} else if err != nil {
		return err
	}
	// A file that isn't this server's database (an empty one a failed start
	// left, another program's) is refused before anything opens it for writing:
	// opening it would migrate it. One a newer release made is the server's to
	// warn about; it is opened as the server opens it.
	if _, err := store.Inspect(ctx, dbPath, false); err != nil && !errors.Is(err, store.ErrNewerDatabase) {
		return fmt.Errorf("reading the database at %s: %w", dbPath, err)
	}
	// A restore waiting for the next start replaces this database, and the
	// password with it: refused, so the password printed is never one that
	// stops working at the next start.
	if restore, err := backup.RestorePending(abs); err != nil {
		return fmt.Errorf("reading the restore waiting in %s: %w (start the server once, which reports and drops it, then run reset-admin again)", abs, err)
	} else if restore != nil {
		return fmt.Errorf("a restore of %s is waiting for the next start, which replaces this database (and a password set now): start the server once to apply it, or cancel it in the console (Settings > Backups), then run reset-admin again", restore.Name)
	}
	pending, err := store.PendingMigrations(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("reading the database at %s: %w", dbPath, err)
	}
	if len(pending) > 0 {
		return fmt.Errorf("the database at %s is from an older release: start the server once (it upgrades the database), then run reset-admin again", dbPath)
	}
	db, err := store.Open(ctx, dbPath, store.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	authSvc := auth.New(db, time.Now)
	u, err := pickAdmin(ctx, authSvc, username)
	if err != nil {
		return err
	}
	password := randomSecret(18)
	if err := authSvc.SetPassword(ctx, u.ID, password, 0); err != nil {
		return err
	}
	// The auth code the first start printed sits in the same log as the
	// password this replaces: it goes too. A failure is reported after the
	// password is printed: the old one is gone, so this one must be shown.
	revoked, revokeErr := authSvc.RevokeInitialAdminCode(ctx, u.ID)
	// Whoever ran this holds a working admin password now, so later starts stop
	// pointing at the first start's, and when this is the first-run admin the
	// password in the log is gone, so the console's account step stops saying
	// it came from there (auth.NoteResetPassword). A failed note only leaves
	// the hint (and that notice) up.
	_ = authSvc.NoteResetPassword(ctx, u.ID)
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("New password for the admin account %q:\n\n  %s\n\n", u.Username, password)
	p("Sign in to the admin console with it. Every session that used the old password\n")
	p("is signed out (a running server may answer one it already checked for up to a\n")
	p("minute; restart the server to end them at once).\n")
	if revokeErr != nil {
		return fmt.Errorf("the password is set, but revoking the first-run auth code failed (revoke it in the console, People > Invites): %w", revokeErr)
	}
	if revoked {
		p("\nThe auth code printed at first start no longer works either: invite a device\n")
		p("from People in the console.\n")
	}
	return nil
}

// pickAdmin is the admin reset-admin resets: the one named, else the only one.
func pickAdmin(ctx context.Context, authSvc *auth.Service, username string) (*auth.User, error) {
	users, err := authSvc.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	var admins []auth.User
	for _, u := range users {
		if u.Role == auth.RoleAdmin {
			admins = append(admins, u)
		}
	}
	names := make([]string, len(admins))
	for i, u := range admins {
		names[i] = u.Username
	}
	slices.Sort(names)
	list := strings.Join(names, ", ")
	if username != "" {
		i := slices.IndexFunc(users, func(u auth.User) bool { return u.Username == username })
		switch {
		case i < 0:
			return nil, &UsageError{fmt.Sprintf("no account named %q (the admins: %s)", username, list)}
		case users[i].Role != auth.RoleAdmin:
			return nil, &UsageError{fmt.Sprintf("%q is not an admin (the admins: %s)", username, list)}
		}
		return checkEnabled(&users[i])
	}
	switch len(admins) {
	case 0:
		return nil, errors.New("there is no admin account yet: start the server, which makes one and prints its password (or opens /setup with --setup)")
	case 1:
		return checkEnabled(&admins[0])
	default:
		return nil, &UsageError{fmt.Sprintf("there are several admins: name one (audiosilo --data <folder> reset-admin <username>): %s", list)}
	}
}

// checkEnabled refuses a disabled admin: a new password wouldn't sign them in.
func checkEnabled(u *auth.User) (*auth.User, error) {
	if u.Disabled {
		return nil, fmt.Errorf("the admin account %q is disabled: sign in as another admin and enable it", u.Username)
	}
	return u, nil
}
