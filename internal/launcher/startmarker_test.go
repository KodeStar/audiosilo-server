package launcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/jsonfile"
)

// markStart writes its own marker over one an earlier start left; a rewrite
// carries the start's running time; end removes the marker, and no write
// after it (the ticker's, racing end) brings it back.
func TestStartMark(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, startMarkerFile)
	writeStartMarker(t, dir, time.Hour, time.Now().Add(-3*time.Hour)) // a crashed start's

	m := markStart(dir, discardLog())
	defer m.end()
	if running, ok := startMarked(dir, time.Now()); !ok || running > time.Minute {
		t.Fatalf("marker after markStart = %s, %v; want this start's, under a minute", running, ok)
	}

	m.began = m.began.Add(-10 * time.Minute) // the start has run ten minutes
	m.write()
	v, err := jsonfile.Read[startMarker](path)
	if err != nil || v == nil || v.StartingForSeconds < 600 || time.Since(v.UpdatedAt) > time.Minute {
		t.Fatalf("rewritten marker = %+v, %v; want ten minutes as of now", v, err)
	}

	m.end()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("marker after end: %v", err)
	}
	m.write()
	m.end()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a write after end brought the marker back: %v", err)
	}
}

// A start that can't write its marker removes an earlier one rather than
// leave it standing for this start.
func TestStartMarkWriteFailureRemovesTheOldMarker(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeStartMarker(t, dir, time.Hour, time.Now().Add(-3*time.Hour))
	// The write goes through a temporary file: a folder in its place fails it.
	if err := os.Mkdir(filepath.Join(dir, startMarkerFile+jsonfile.TempSuffix), 0o700); err != nil {
		t.Fatal(err)
	}
	m := markStart(dir, discardLog())
	defer m.end()
	if _, ok := startMarked(dir, time.Now()); ok {
		t.Error("an earlier start's marker stands after a failed write")
	}
}
