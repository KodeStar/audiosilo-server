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
	"os"
	"os/signal"
	"syscall"

	"github.com/kodestar/audiosilo-server/pkg/launcher"
)

func main() {
	dataDir := flag.String("data", "./data", "data directory (config, database, certs)")
	ffprobePath := flag.String("ffprobe", "ffprobe", "path to ffprobe binary (\"\" to disable)")
	ffmpegPath := flag.String("ffmpeg", "ffmpeg", "path to ffmpeg binary for on-the-fly transcoding (\"\" to disable)")
	setup := flag.Bool("setup", false, "first-run web setup wizard: instead of auto-creating the admin, enable a browser wizard to set the admin password and books folder")
	flag.Parse()

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
