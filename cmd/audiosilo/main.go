// Command audiosilo runs the AudioSilo audiobook server: a JSON API plus the
// baked-in admin/connect UI, designed to be safe to expose to the internet.
//
// On first run it generates an admin account and an auth code, printing both to
// stdout exactly once; `audiosilo --data <folder> reset-admin [username]` sets
// a new admin password when that one is lost, and `audiosilo --data <folder>
// healthcheck` asks the running server's /healthz (the Docker image's
// HEALTHCHECK). Configuration lives in <data>/config.yaml. The actual run
// loop lives in internal/launcher.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/kodestar/audiosilo-server/internal/api"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/fsaccess"
	"github.com/kodestar/audiosilo-server/internal/launcher"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

func main() {
	dataDir := flag.String("data", "./data", "data directory (config, database, certs)")
	ffprobePath := flag.String("ffprobe", "ffprobe", "path to ffprobe binary (\"\" to disable)")
	ffmpegPath := flag.String("ffmpeg", "ffmpeg", "path to ffmpeg binary for on-the-fly transcoding (\"\" to disable)")
	setup := flag.Bool("setup", false, "first-run web setup wizard: instead of auto-creating the admin, enable a browser wizard to set the admin password (the admin console then walks through the rest)")
	flag.Parse()
	if code, done := command(flag.Args(), *dataDir, os.Stdout, os.Stderr); done {
		os.Exit(code)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := launcher.Run(ctx, launcher.Options{
		DataDir:     *dataDir,
		FFprobePath: *ffprobePath,
		FFmpegPath:  *ffmpegPath,
		Setup:       *setup,
	}); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// command handles the positional arguments after the flags: none runs the server
// (done is false); "version" prints the version and exits 0, before anything
// touches the data directory; "reset-admin [username]" sets a new password on an
// admin of the server in dataDir (launcher.ResetAdmin) and exits 0, 1 when it
// fails, or 2 when asked wrongly (launcher.UsageError); anything else is a usage
// error (exit 2, as the flag package exits on a bad flag). "healthcheck" asks the
// server running from dataDir whether it is healthy (launcher.Healthcheck) and
// exits 0 when it is, 1 when it is not. "checkdirs" prints folder warnings as the server user.
// "chowndata UID:GID" is the image's root ownership walk of dataDir (fsaccess.ChownTree).
func command(args []string, dataDir string, stdout, stderr io.Writer) (code int, done bool) {
	switch {
	case len(args) == 0:
		return 0, false
	case len(args) == 1 && args[0] == "version":
		_, _ = fmt.Fprintln(stdout, "audiosilo", api.Version)
		return 0, true
	case args[0] == "version":
		_, _ = fmt.Fprintln(stderr, "audiosilo: \"version\" takes no arguments (flags go before it; run with -h for them)")
		return 2, true
	case args[0] == "reset-admin":
		return resetAdmin(args[1:], dataDir, stdout, stderr), true
	case len(args) == 1 && args[0] == "healthcheck":
		return healthcheck(dataDir, stdout, stderr), true
	case args[0] == "checkdirs":
		return checkDirs(args[1:], dataDir, stderr), true
	case args[0] == "chowndata":
		return chownData(args[1:], dataDir, stderr), true
	case args[0] == "healthcheck":
		_, _ = fmt.Fprintln(stderr, "audiosilo: usage: audiosilo [--data <folder>] healthcheck (flags go before the command)")
		return 2, true
	default:
		_, _ = fmt.Fprintf(stderr, "audiosilo: unknown command %q (the commands are \"version\", \"reset-admin\", \"healthcheck\", \"checkdirs\" and \"chowndata\"; run with -h for the flags)\n", args[0])
		return 2, true
	}
}

// checkDirs runs the checkdirs command: the folder warnings on stderr, exit 0
// whatever they say (they never hold the start), 2 when asked wrongly.
func checkDirs(args []string, dataDir string, stderr io.Writer) int {
	flags := flag.NewFlagSet("checkdirs", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dataFailed := flags.Bool("data-chown-failed", false, "the entrypoint could not change the data folder's owner")
	backupFailed := flags.Bool("backups-chown-failed", false, "the entrypoint could not change the backups folder's owner")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "audiosilo: usage: audiosilo [--data <folder>] checkdirs")
		return 2
	}
	fsaccess.CheckDirs(dataDir, configuredBackups(dataDir), *dataFailed, *backupFailed, stderr)
	return 0
}

// chownData runs the chowndata command: the data folder's contents given to
// UID:GID (the entrypoint's PUID:PGID), 0 when all changed, 1 when some could
// not be (on stderr), 2 when asked wrongly.
func chownData(args []string, dataDir string, stderr io.Writer) int {
	uid, gid, ok := -1, -1, len(args) == 1
	if ok {
		u, g, found := strings.Cut(args[0], ":")
		var errU, errG error
		uid, errU = strconv.Atoi(u)
		gid, errG = strconv.Atoi(g)
		ok = found && errU == nil && errG == nil && uid >= 0 && gid >= 0
	}
	if !ok {
		_, _ = fmt.Fprintln(stderr, "audiosilo: usage: audiosilo [--data <folder>] chowndata UID:GID")
		return 2
	}
	if err := fsaccess.ChownTree(dataDir, uid, gid); err != nil {
		_, _ = fmt.Fprintln(stderr, "audiosilo: chowndata:", err)
		return 1
	}
	return 0
}

// The root ownership pass is opt-in by environment only. The unprivileged
// probe checks the folder the server will use, including a YAML-only override.
func configuredBackups(dataDir string) string {
	if dir := os.Getenv("AUDIOSILO_BACKUP_DIR"); dir != "" {
		return dir
	}
	dir, _ := metadata.LookBounded(context.Background(), config.Path(dataDir), fsaccess.DirCheckTimeout, func() (string, error) {
		cfg, _, err := config.Load(dataDir)
		if err != nil {
			return "", err // startup reports a config it cannot load
		}
		return cfg.Backups.Dir, nil
	})
	return dir
}

// healthcheck runs the healthcheck command: 0 when the server answers healthy,
// 1 with the reason on stderr when it doesn't.
func healthcheck(dataDir string, stdout, stderr io.Writer) int {
	if err := launcher.Healthcheck(context.Background(), dataDir, stdout); err != nil {
		_, _ = fmt.Fprintln(stderr, "audiosilo: healthcheck:", err)
		return 1
	}
	return 0
}

// resetAdmin runs reset-admin with the arguments after it: none, or the admin's
// username. A flag there is refused, since the flag package stops at the
// command: "--data" after it would never be read.
func resetAdmin(args []string, dataDir string, stdout, stderr io.Writer) int {
	username := ""
	for _, arg := range args {
		if len(args) > 1 || strings.HasPrefix(arg, "-") {
			_, _ = fmt.Fprintln(stderr, "audiosilo: usage: audiosilo [--data <folder>] reset-admin [username] (flags go before the command)")
			return 2
		}
		username = arg
	}
	err := launcher.ResetAdmin(context.Background(), dataDir, username, stdout)
	if err == nil {
		return 0
	}
	_, _ = fmt.Fprintln(stderr, "audiosilo: reset-admin:", err)
	if usage := (*launcher.UsageError)(nil); errors.As(err, &usage) {
		return 2
	}
	return 1
}
