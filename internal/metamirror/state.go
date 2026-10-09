package metamirror

import (
	"path/filepath"
	"time"

	"github.com/kodestar/audiosilo-server/internal/jsonfile"
)

// The state file and the temporary name it is written under (jsonfile.Write).
const (
	stateFile = "state.json"
	stateTemp = stateFile + jsonfile.TempSuffix
)

// state is what the mirror records about its copy and its checks
// (<data>/meta-mirror/state.json), so a restart neither downloads again within
// the day nor forgets which copy is current. It is the one record of those
// facts: Status reads them here.
type state struct {
	// The copy: its release, what the artifact says of itself, and the
	// decompressed file's size.
	Tag           string    `json:"tag,omitempty"`
	BuiltAt       time.Time `json:"built_at,omitzero"`
	SchemaVersion int       `json:"schema_version,omitempty"`
	SizeBytes     int64     `json:"size_bytes,omitempty"`
	DownloadedAt  time.Time `json:"downloaded_at,omitzero"`
	// On-disk diagnostics only (nothing reads them back): when the release was
	// published and the decompressed file's sha256, for an admin comparing the
	// copy with the release on GitHub.
	PublishedAt time.Time `json:"published_at,omitzero"`
	SHA256      string    `json:"sha256,omitempty"`
	// The checks: the release list's validator, the last check and how it
	// failed ("" when it didn't).
	ETag      string    `json:"etag,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitzero"`
	LastError string    `json:"last_error,omitempty"`
}

// forgetCopy drops the copy from the state, keeping the schedule's last check.
// The ETag goes too: without a copy the next check must list the releases.
func (s *state) forgetCopy() {
	*s = state{CheckedAt: s.CheckedAt, LastError: s.LastError}
}

// loadState reads the state file; none (or one that doesn't parse) is a mirror
// with no copy and no check yet.
func (m *Mirror) loadState() state {
	st, err := jsonfile.Read[state](filepath.Join(m.dir, stateFile))
	if err != nil {
		m.log.Warn("metadata mirror: its state can't be read; starting afresh", "err", err)
	}
	if st == nil {
		return state{}
	}
	return *st
}

// saveState writes the state (owner-only, all or nothing: jsonfile.Write). A
// failure is logged: the mirror keeps working, and the next save tries again.
// Callers hold m.mu.
func (m *Mirror) saveState() {
	if err := jsonfile.Write(filepath.Join(m.dir, stateFile), m.st); err != nil {
		m.log.Warn("metadata mirror: save its state", "err", err)
	}
}
