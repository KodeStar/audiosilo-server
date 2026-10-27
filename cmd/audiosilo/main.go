// Command audiosilo runs the AudioSilo audiobook server: a JSON API plus the
// baked-in admin/connect UI, designed to be safe to expose to the internet.
//
// On first run it generates an admin account and an auth code, printing both to
// stdout exactly once. Configuration lives in <data>/config.yaml. The actual run
// loop lives in pkg/launcher so the audiosilo-manager desktop app can share it.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
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
	if code, done := command(flag.Args(), os.Stdout, os.Stderr); done {
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
// touches the data directory; anything else is a usage error (exit 2, as the
// flag package exits on a bad flag).
func command(args []string, stdout, stderr io.Writer) (code int, done bool) {
	switch {
	case len(args) == 0:
		return 0, false
	case len(args) == 1 && args[0] == "version":
		fmt.Fprintln(stdout, "audiosilo", api.Version)
		return 0, true
	default:
		fmt.Fprintf(stderr, "audiosilo: unknown command %q (the only command is \"version\"; run with -h for the flags)\n", args[0])
		return 2, true
	}
}
