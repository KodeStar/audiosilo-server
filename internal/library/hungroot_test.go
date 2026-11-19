package library

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// A root that stops answering after the availability probe (a share that
// drops between two looks) fails the scan as unavailable within the probe's
// time: the root's stat and read check were plain calls that could hold the
// one scan worker for minutes. With the root answering, the same library
// scans. Not parallel: metadata.HoldOpensForTests swaps the mount lookup.
func TestAScanOfARootThatStopsAnsweringIsUnavailable(t *testing.T) {
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "Author/Book/01.mp3", make([]byte, 1<<10))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	s.probeTimeout = 200 * time.Millisecond
	if res := scanWithin(t, s, *lib, 20*time.Second); res.Added != 1 {
		t.Fatalf("added %d, want the book", res.Added)
	}

	release := metadata.HoldOpensForTests(root)
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() {
		_, err := s.Scan(context.WithoutCancel(ctx), *lib)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrLibraryUnavailable) || !errors.Is(err, metadata.ErrNotAnswered) {
			t.Errorf("the scan of a root that stopped answering = %v, want unavailable, not answered", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the scan of a root that stopped answering has not finished")
	}
	release()
	if res := scanWithin(t, s, *lib, 20*time.Second); res.Removed != 0 || res.Partial {
		t.Errorf("the scan once the root answers: removed %d, partial %v", res.Removed, res.Partial)
	}
}

// Discovery resolves the root within its time: a root that won't resolve is
// unavailable, rather than a walk from the unresolved path that finds nothing.
func TestDiscoveryOfARootThatDoesNotResolveIsUnavailable(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "Author/Book/01.mp3", make([]byte, 1<<10))
	lib := catalog.Library{Name: "L", Root: root}
	release := metadata.HoldOpensForTests(root)
	defer release()
	books, _, _, err := discoverAuto(t.Context(), lib, nil, nil, slog.New(slog.DiscardHandler), nil, nil, 4, 200*time.Millisecond)
	if !errors.Is(err, ErrLibraryUnavailable) || len(books) != 0 {
		t.Errorf("discovery of a held root = %d books, %v; want none, unavailable", len(books), err)
	}
}
