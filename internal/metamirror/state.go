package metamirror

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// The state file and the temporary name it is written under.
const (
	stateFile = "state.json"
	stateTemp = "state.json.tmp"
)

// state is what the mirror records about its copy and its checks
// (<data>/meta-mirror/state.json), so a restart neither downloads again within
// the day nor forgets which copy is current.
type state struct {
	// The copy: its release, what the artifact says of itself, and the
	// decompressed file's digest and size.
	Tag           string    `json:"tag,omitempty"`
	PublishedAt   time.Time `json:"published_at,omitzero"`
	BuiltAt       time.Time `json:"built_at,omitzero"`
	SchemaVersion int       `json:"schema_version,omitempty"`
	SHA256        string    `json:"sha256,omitempty"`
	SizeBytes     int64     `json:"size_bytes,omitempty"`
	DownloadedAt  time.Time `json:"downloaded_at,omitzero"`
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
	var st state
	b, err := os.ReadFile(filepath.Join(m.dir, stateFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return st
	case err != nil:
		m.log.Warn("metadata mirror: read its state", "err", err)
		return st
	}
	if err := json.Unmarshal(b, &st); err != nil {
		m.log.Warn("metadata mirror: its state doesn't parse; starting afresh", "err", err)
		return state{}
	}
	return st
}

// saveState writes the state (owner-only, under a temporary name then renamed,
// so a crash leaves the old state or the new one, never half of one). A failure
// is logged: the mirror keeps working, and the next save tries again. Callers
// hold m.mu.
func (m *Mirror) saveState() {
	b, err := json.MarshalIndent(m.st, "", "  ")
	if err == nil {
		tmp := filepath.Join(m.dir, stateTemp)
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, filepath.Join(m.dir, stateFile))
		}
	}
	if err != nil {
		m.log.Warn("metadata mirror: save its state", "err", err)
	}
}
