package launcher

import (
	"context"
	"log/slog"

	"github.com/kodestar/audiosilo-meta/pkg/release"

	"github.com/kodestar/audiosilo-server/internal/api"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/metamirror"
)

// releaseSource is a GitHub the metadata mirror's release client talks to
// instead of the real one: a repository and an API base. Tests only.
type releaseSource struct {
	repo, apiBase string
}

// metaMirror prepares mirror mode's local copy of the community metadata
// (metadata.mode: mirror, chosen at start), or returns nil. Nil also when the
// server has no metadata service (an empty or invalid base_url: nothing to
// mirror for), and when the folder can't be prepared (lookups then go to the
// remote service, as in remote mode: a metadata problem never stops the server).
// A server started in remote mode deletes the folder mirror mode left: the copy
// is derived data that nothing reads there. The release client asks the
// audiosilo-meta releases on GitHub as AudioSilo/<version>, nothing else sent.
func metaMirror(cfg *config.Config, dataDir string, enabled func() bool, src releaseSource, log *slog.Logger) *metamirror.Mirror {
	if !cfg.Metadata.ValidBaseURL() {
		return nil
	}
	if !cfg.Metadata.Mirror() {
		if had, err := metamirror.Remove(dataDir); err != nil {
			log.Warn("metadata mirror: delete the local copy left by mirror mode", "err", err)
		} else if had {
			log.Info("metadata mirror: deleted the local copy (the server runs in remote mode)",
				"dir", metamirror.Dir(dataDir))
		}
		return nil
	}
	rel := []release.Option{
		release.WithUserAgent("AudioSilo/" + api.Version),
		release.WithLogger(slog.NewLogLogger(log.Handler(), slog.LevelInfo)),
	}
	if src.apiBase != "" {
		rel = append(rel, release.WithAPIBase(src.apiBase))
	}
	m, err := metamirror.New(metamirror.Dir(dataDir), metamirror.Options{
		Enabled: enabled,
		Logger:  log,
		SiteURL: cfg.Metadata.BaseURL,
		Repo:    src.repo,
		Release: rel,
	})
	if err != nil {
		log.Error("metadata mirror: can't prepare the local copy; lookups go to the metadata service", "err", err)
		return nil
	}
	return m
}

// runMetaMirror runs the mirror (it opens its copy, then checks on its
// schedule) until ctx ends; the returned func stops it, waits for the check in
// flight, and closes the copy (deferred by Run).
func runMetaMirror(ctx context.Context, m *metamirror.Mirror, log *slog.Logger) (stop func()) {
	if m == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	return func() {
		cancel()
		<-done
		if err := m.Close(); err != nil {
			log.Warn("metadata mirror: close the local copy", "err", err)
		}
	}
}
