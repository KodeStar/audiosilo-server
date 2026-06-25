//go:build embedplayer

package web

import (
	"embed"
	"io/fs"
)

// playerEmbed holds a prebuilt web-player export (the audiosilo-frontend Expo
// static site, built with baseUrl=/web) baked into the binary, so native single-
// file distributions serve /web without a separate web_dir on disk.
//
// internal/web/player/ is gitignored (only a .gitkeep is committed); the release
// pipeline populates it from the pinned web image/export before building with
// `-tags embedplayer`. A build without that population still compiles (the
// committed .gitkeep satisfies the embed) but exposes no player — HasPlayer is
// false because there's no index.html — degrading exactly like an empty web_dir.
//
//go:embed all:player
var playerEmbed embed.FS

func embeddedPlayer() (fs.FS, bool) {
	sub, err := fs.Sub(playerEmbed, "player")
	if err != nil {
		return nil, false
	}
	return sub, true
}
