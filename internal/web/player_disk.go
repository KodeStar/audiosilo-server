//go:build !embedplayer

package web

import "io/fs"

// embeddedPlayer reports that no web player is baked into this build; it is served
// from web_dir on disk instead (env AUDIOSILO_WEB_DIR / config web_dir). Release
// builds for native distribution use -tags embedplayer (player_embed.go) to bake a
// prebuilt export into the binary so it is self-contained.
func embeddedPlayer() (fs.FS, bool) { return nil, false }
