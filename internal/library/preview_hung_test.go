package library

import (
	"log/slog"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// A sample whose open doesn't answer, as on a share mounted below the root
// that stopped answering, is given up on at the preview's time (the sample
// read's open is bounded): the preview answers then, without that sample,
// and frees its mount's place, so the previews after it on that mount run
// too, more than previewsPerMount of them; a preview on another mount reads
// as ever. Not parallel:
// metadata.HoldOpensForTests swaps the mount lookup.
func TestPreviewHungFileIsBounded(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "Author/Book/01.mp3", make([]byte, 1<<10))
	release := metadata.HoldOpensForTests(root)
	t.Cleanup(release)
	s := NewScanner(nil, "", slog.Default())
	lim := PreviewLimits{MaxTime: 100 * time.Millisecond}
	for range previewsPerMount + 1 {
		start := time.Now()
		p, err := s.Preview(t.Context(), root, nil, lim)
		if err != nil || p.Counts.AudioFiles != 1 || len(p.Samples) != 0 {
			t.Fatalf("a preview whose sample's open hangs: %+v, %v; want it counted, no sample", p, err)
		}
		if took := time.Since(start); took > lim.MaxTime+previewGrace {
			t.Errorf("the preview answered after %v, want it by its time", took)
		}
	}
	if p, err := s.Preview(t.Context(), previewFixture(t), nil, PreviewLimits{}); err != nil || p.Counts.AudioFiles == 0 {
		t.Errorf("a preview on another mount: %+v, %v, want it read", p, err)
	}
}
