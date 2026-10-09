package launcher

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/kodestar/audiosilo-meta/pkg/release"

	"github.com/kodestar/audiosilo-server/internal/api"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/metamirror"
)

// newMetaRelease builds the client the metadata mirror downloads with: the
// audiosilo-meta releases on GitHub, as AudioSilo/<version>, nothing else sent.
// size is the current copy's size (the download's decompression bound follows
// it).
func newMetaRelease(size func() int64, log *slog.Logger) *release.Client {
	opts := []release.Option{
		release.WithUserAgent("AudioSilo/" + api.Version),
		release.WithLogger(slog.NewLogLogger(log.Handler(), slog.LevelInfo)),
		release.WithBaseSize(size),
	}
	return release.New(release.DefaultRepo, "", append(opts, testReleaseOptions...)...)
}

// testReleaseOptions are added to the release client by the launcher's tests
// (a fake GitHub's address); always empty in a running server.
var testReleaseOptions []release.Option

// metaMirror prepares mirror mode's local copy of the community metadata
// (metadata.mode: mirror, chosen at start), or returns nil. Nil also when the
// server has no metadata service (an empty or invalid base_url: nothing to
// mirror for), and when the folder can't be prepared (lookups then go to the
// remote service, as in remote mode: a metadata problem never stops the server).
// A server started in remote mode deletes the folder mirror mode left: the copy
// is derived data that nothing reads there.
func metaMirror(cfg *config.Config, dataDir string, enabled func() bool, log *slog.Logger) *metamirror.Mirror {
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
	// The client's size callback reads the mirror built below; it runs only once
	// a download starts, after the mirror exists.
	var built atomic.Pointer[metamirror.Mirror]
	client := newMetaRelease(func() int64 {
		if m := built.Load(); m != nil {
			return m.SizeBytes()
		}
		return 0
	}, log)
	m, err := metamirror.New(metamirror.Dir(dataDir), client, metamirror.Options{
		Enabled: enabled,
		Logger:  log,
		SiteURL: cfg.Metadata.BaseURL,
	})
	if err != nil {
		log.Error("metadata mirror: can't prepare the local copy; lookups go to the metadata service", "err", err)
		return nil
	}
	built.Store(m)
	return m
}

// runMetaMirror runs the mirror's checks until ctx ends; the returned func stops
// them, waits for the one in flight, and closes the copy (deferred by Run).
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
