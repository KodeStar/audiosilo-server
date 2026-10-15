package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// OnlineWindow is how recent the last heartbeat must be for the Bridge to count
// as online: the bridge capability, and the console's status. The Bridge beats
// every 30 s.
const OnlineWindow = 2 * time.Minute

// Status is what the Bridge last reported (PUT /bridge/status), as stored.
type Status struct {
	// Version is the Bridge's version.
	Version string `json:"version"`
	// Kinds are the job kinds it implements (KindVerify, ...). The server queues
	// only kinds that are both here and known to it (Kinds).
	Kinds []string `json:"kinds"`
	// Libraries is the Bridge's mapping of each server library to its own mount,
	// checked on every heartbeat.
	Libraries []LibraryStatus `json:"libraries"`
	// Workers are the ASR workers connected to it (Phase 1b).
	Workers []Worker `json:"workers"`
	// UIURL is the Bridge's own LAN page, linked from the console.
	UIURL string `json:"ui_url"`
}

// LibraryStatus is one library's mapping as the Bridge checked it.
type LibraryStatus struct {
	LibraryID int64 `json:"library_id"`
	// MountPath is where the Bridge sees the library's root.
	MountPath string `json:"mount_path"`
	// Mapped reports that a known indexed book was found at MountPath with the
	// size the server recorded; Writable that a probe file could be written and
	// removed there. Writing jobs are refused for a library that isn't both.
	Mapped   bool `json:"mapped"`
	Writable bool `json:"writable"`
	// CheckedPath is the book the check used; Error why it failed ("" when it
	// didn't).
	CheckedPath string `json:"checked_path"`
	Error       string `json:"error"`
}

// Worker is one ASR worker the Bridge reports.
type Worker struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
	LastSeen string `json:"last_seen"`
}

// Heartbeat is the stored status with when it arrived.
type Heartbeat struct {
	Status
	At time.Time
}

// Online reports whether the heartbeat is recent enough (OnlineWindow) at now.
func (h *Heartbeat) Online(now time.Time) bool {
	return h != nil && !h.At.IsZero() && now.Sub(h.At) < OnlineWindow && h.At.Sub(now) < OnlineWindow
}

// Features are the job families the Bridge offers that the server knows: the
// part of each known kind before its dot ("bridge.verify" is "bridge"), sorted,
// once each. The bridge capability carries them.
func (st Status) Features() []string {
	var out []string
	for _, k := range st.Kinds {
		if _, ok := kinds[k]; !ok {
			continue
		}
		family, _, _ := strings.Cut(k, ".")
		if !slices.Contains(out, family) {
			out = append(out, family)
		}
	}
	slices.Sort(out)
	return out
}

// Offers reports whether the Bridge implements kind.
func (st Status) Offers(kind string) bool { return slices.Contains(st.Kinds, kind) }

// Library returns the Bridge's mapping of a library, or nil when it sent none.
func (st Status) Library(id int64) *LibraryStatus {
	for i := range st.Libraries {
		if st.Libraries[i].LibraryID == id {
			return &st.Libraries[i]
		}
	}
	return nil
}

// Limits on a heartbeat: its body is the Bridge's, so the server bounds what it
// stores and later shows in the console.
const (
	MaxStatusBytes = 64 << 10
	maxKinds       = 64
	maxLibraries   = 512
	maxWorkers     = 64
	maxShortText   = 128
	maxLongText    = 1024
)

// kindPattern is a job kind: family.name, lower case.
var kindPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}\.[a-z][a-z0-9_]{0,31}$`)

// ErrInvalid wraps every rejection of what the Bridge or an admin sent (a 400).
var ErrInvalid = errors.New("invalid")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Validate checks a heartbeat before it is stored. Text is bounded and printable,
// kinds are well formed and listed once, and ui_url, which the console renders as
// a link, is an absolute http(s) URL or empty.
func (st *Status) Validate() error {
	if err := checkText("version", st.Version, maxShortText); err != nil {
		return err
	}
	if len(st.Kinds) > maxKinds {
		return invalid("too many kinds")
	}
	for i, k := range st.Kinds {
		if !kindPattern.MatchString(k) || slices.Contains(st.Kinds[:i], k) {
			return invalid("kind %q", k)
		}
	}
	if len(st.Libraries) > maxLibraries {
		return invalid("too many libraries")
	}
	for i, l := range st.Libraries {
		if l.LibraryID <= 0 || slices.ContainsFunc(st.Libraries[:i], func(o LibraryStatus) bool { return o.LibraryID == l.LibraryID }) {
			return invalid("library %d", l.LibraryID)
		}
		for name, v := range map[string]string{"mount_path": l.MountPath, "checked_path": l.CheckedPath, "error": l.Error} {
			if err := checkText(name, v, maxLongText); err != nil {
				return err
			}
		}
	}
	if len(st.Workers) > maxWorkers {
		return invalid("too many workers")
	}
	for _, w := range st.Workers {
		for name, v := range map[string]string{"name": w.Name, "version": w.Version, "platform": w.Platform, "last_seen": w.LastSeen} {
			if err := checkText(name, v, maxShortText); err != nil {
				return err
			}
		}
	}
	if st.UIURL != "" {
		if err := checkText("ui_url", st.UIURL, maxLongText); err != nil {
			return err
		}
		u, err := url.Parse(st.UIURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return invalid("ui_url must be an http(s) URL")
		}
	}
	if st.Kinds == nil {
		st.Kinds = []string{}
	}
	if st.Libraries == nil {
		st.Libraries = []LibraryStatus{}
	}
	if st.Workers == nil {
		st.Workers = []Worker{}
	}
	return nil
}

// checkText bounds one text field: valid UTF-8, at most max bytes, no control
// characters.
func checkText(name, v string, max int) error {
	if len(v) > max || !utf8.ValidString(v) || strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return invalid("%s", name)
	}
	return nil
}

// ErrNotPaired is a heartbeat with no paired Bridge to record it on (the row
// went with an unpair after the request was authenticated).
var ErrNotPaired = errors.New("no bridge is paired")

// SaveHeartbeat records st (already validated) as the Bridge's status now.
func (s *Service) SaveHeartbeat(ctx context.Context, st Status) error {
	body, err := json.Marshal(st)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE bridge_status SET heartbeat_at = ?, status = ? WHERE id = 1 AND paired_at IS NOT NULL`, s.ts(), string(body))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotPaired
	}
	return nil
}

// LastHeartbeat returns the Bridge's last heartbeat, or nil when no Bridge is
// paired or none has beaten since it paired.
func (s *Service) LastHeartbeat(ctx context.Context) (*Heartbeat, error) {
	var (
		stamp sql.NullString
		body  string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT heartbeat_at, status FROM bridge_status WHERE id = 1 AND paired_at IS NOT NULL`).Scan(&stamp, &body)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !stamp.Valid) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t, err := time.Parse(time.RFC3339, stamp.String)
	if err != nil {
		return nil, nil // an unreadable stamp is no heartbeat
	}
	h := &Heartbeat{At: t}
	if err := json.Unmarshal([]byte(body), &h.Status); err != nil {
		return nil, nil
	}
	return h, nil
}

// Online returns the last heartbeat when it is recent enough to count the Bridge
// as online, else nil.
func (s *Service) Online(ctx context.Context) (*Heartbeat, error) {
	h, err := s.LastHeartbeat(ctx)
	if err != nil || !h.Online(s.now()) {
		return nil, err
	}
	return h, nil
}
