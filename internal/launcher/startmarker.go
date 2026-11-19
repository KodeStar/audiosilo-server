package launcher

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/jsonfile"
)

// startMarkerFile, in the data folder, says a start is under way: Run writes
// it before it listens and removes it once the gate opens or the start ends.
// It is the healthcheck's view of a start it cannot ask about: under autocert
// with no certificate it may name, the check only sends a nameless TLS hello,
// which a hung start answers as well as a finished one (Healthcheck).
const startMarkerFile = "starting.json"

// startMarkerEvery is how often a start under way rewrites its marker, so the
// marker's age reads the start's own (monotonic) clock: a wall clock set
// mid-start (a board with no battery, at its first NTP answer) moves the
// marker's updated_at for at most this long. Well inside the image's check
// interval times its retries, so such a jump never fails three checks in a row.
const startMarkerEvery = 30 * time.Second

// startMarker is the start marker's content.
type startMarker struct {
	// StartedAt is when the start began, for a person reading the file.
	StartedAt time.Time `json:"started_at"`
	// StartingForSeconds is how long the start had run at UpdatedAt, read
	// on the server's monotonic clock.
	StartingForSeconds int64     `json:"starting_for_seconds"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// startMark keeps a start's marker until end.
type startMark struct {
	path  string
	log   *slog.Logger
	began time.Time

	mu     sync.Mutex
	warned bool          // under mu: a failing write is logged once
	stop   chan struct{} // closed by end, under mu: no write after it, so end's remove is the last word
}

// markStart writes the marker of a start that begins now, over any marker an
// earlier start left (a process that crashed mid-start), and rewrites it every
// startMarkerEvery until end. Written before the listener opens, it is this
// start's own whenever the server answers: the rule the healthcheck relies on
// to never read an earlier start's leftover as this one stuck.
func markStart(dataDir string, log *slog.Logger) *startMark {
	m := &startMark{
		path:  filepath.Join(dataDir, startMarkerFile),
		log:   log,
		began: time.Now(),
		stop:  make(chan struct{}),
	}
	m.write()
	go func() {
		t := time.NewTicker(startMarkerEvery)
		defer t.Stop()
		for {
			select {
			case <-m.stop:
				return
			case <-t.C:
				m.write()
			}
		}
	}()
	return m
}

// write stores the marker as of now; a write that fails removes the marker
// rather than leave an older one standing for this start.
func (m *startMark) write() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ended() {
		return
	}
	err := jsonfile.Write(m.path, startMarker{
		StartedAt:          m.began.Round(0),
		StartingForSeconds: int64(time.Since(m.began) / time.Second),
		UpdatedAt:          time.Now().Round(0),
	})
	if err == nil {
		return
	}
	if rerr := os.Remove(m.path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		err = errors.Join(err, rerr)
	}
	if !m.warned {
		m.warned = true
		m.log.Warn("could not write the start marker: the healthcheck under autocert can't tell a stuck start",
			"path", m.path, "err", err)
	}
}

// end removes the marker, once: the start has finished (the gate opened) or
// failed. No write follows it.
func (m *startMark) end() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ended() {
		return
	}
	close(m.stop)
	if err := os.Remove(m.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.log.Warn("could not remove the start marker", "path", m.path, "err", err)
	}
}

// ended reports whether end has run; the caller holds mu.
func (m *startMark) ended() bool {
	select {
	case <-m.stop:
		return true
	default:
		return false
	}
}

// startMarked reads the start marker in dataDir: how long the start under way
// has run as of now, false when there is none (or it can't be read: the check
// then says what it would without one). The age is the start's own reading at
// its last rewrite plus the time since, which grows while a start too stuck to
// rewrite its marker (its data folder hung) stays put. One with no updated_at
// (a "null" or "{}") is unreadable too: its age from the zero time would
// saturate, read as stuck, or overflow into a negative running time.
func startMarked(dataDir string, now time.Time) (time.Duration, bool) {
	m, err := jsonfile.Read[startMarker](filepath.Join(dataDir, startMarkerFile))
	if err != nil || m == nil || m.UpdatedAt.IsZero() {
		return 0, false
	}
	return time.Duration(m.StartingForSeconds)*time.Second + max(0, now.Sub(m.UpdatedAt)), true
}
