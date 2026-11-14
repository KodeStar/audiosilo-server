// Command audiosilo runs the AudioSilo audiobook server: a JSON API plus the
// baked-in admin/connect UI, designed to be safe to expose to the internet.
//
// On first run it generates an admin account and an auth code, printing both to
// stdout exactly once; `audiosilo --data <folder> reset-admin [username]` sets
// a new admin password when that one is lost. Configuration lives in <data>/config.yaml. The actual run
// loop lives in pkg/launcher.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/kodestar/audiosilo-server/internal/api"
	"github.com/kodestar/audiosilo-server/pkg/launcher"
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
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// command handles the positional arguments after the flags: none runs the server
// (done is false); "version" prints the version and exits 0, before anything
// touches the data directory; "reset-admin [username]" sets a new password on an
// admin of the server in dataDir (launcher.ResetAdmin) and exits 0, 1 when it
// fails, or 2 when asked wrongly (launcher.UsageError); anything else is a usage
// error (exit 2, as the flag package exits on a bad flag).
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
	default:
		fmt.Fprintf(stderr, "audiosilo: unknown command %q (the commands are \"version\" and \"reset-admin\"; run with -h for the flags)\n", args[0])
		return 2, true
	}
}

// resetAdmin runs reset-admin with the arguments after it: none, or the admin's
// username. A flag there is refused, since the flag package stops at the
// command: "--data" after it would never be read.
func resetAdmin(args []string, dataDir string, stdout, stderr io.Writer) int {
	if len(args) > 1 || len(args) == 1 && strings.HasPrefix(args[0], "-") {
		_, _ = fmt.Fprintln(stderr, "audiosilo: usage: audiosilo [--data <folder>] reset-admin [username] (flags go before the command)")
		return 2
	}
	username := ""
	if len(args) == 1 {
		username = args[0]
	}
	err := launcher.ResetAdmin(context.Background(), dataDir, username, stdout)
	var usage *launcher.UsageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &usage):
		_, _ = fmt.Fprintln(stderr, "audiosilo: reset-admin:", err)
		return 2
	default:
		_, _ = fmt.Fprintln(stderr, "audiosilo: reset-admin:", err)
		return 1
	}
}
