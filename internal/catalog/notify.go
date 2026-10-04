package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Notification destinations and the server's event feed (internal/notify sends;
// this is where they are kept).

// MaxNotifyTargets bounds how many destinations a server has.
const MaxNotifyTargets = 20

// ServerEventRetention is how long the event feed is kept.
const ServerEventRetention = 90 * 24 * time.Hour

// ErrTooManyTargets: the server has MaxNotifyTargets destinations already.
var ErrTooManyTargets = errors.New("too many notification destinations")

// NotifyTarget is one place notifications go. URL and Secret are credentials and
// never leave the server (json:"-"); the API shows a redacted address instead.
type NotifyTarget struct {
	ID      int64    `json:"id"`
	Kind    string   `json:"kind"` // webhook | ntfy | discord
	Name    string   `json:"name"`
	URL     string   `json:"-"`
	Secret  string   `json:"-"`
	Enabled bool     `json:"enabled"`
	Events  []string `json:"events"`
	// CreatedAt/UpdatedAt are RFC3339; Last* is the newest delivery's outcome
	// (nil before the first).
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	LastAt    *string `json:"last_at"`
	LastOK    *bool   `json:"last_ok"`
	LastError string  `json:"last_error"`
}

const targetCols = `id, kind, name, url, secret, enabled, events, created_at, updated_at, last_at, last_ok, last_error`

func scanTarget(rows *sql.Rows, t *NotifyTarget) error {
	var events string
	var lastAt sql.NullString
	var lastOK sql.NullBool
	if err := rows.Scan(&t.ID, &t.Kind, &t.Name, &t.URL, &t.Secret, &t.Enabled, &events,
		&t.CreatedAt, &t.UpdatedAt, &lastAt, &lastOK, &t.LastError); err != nil {
		return err
	}
	t.Events = []string{}
	_ = json.Unmarshal([]byte(events), &t.Events)
	if lastAt.Valid {
		t.LastAt = &lastAt.String
	}
	if lastOK.Valid {
		t.LastOK = &lastOK.Bool
	}
	return nil
}

// ListNotifyTargets returns every destination, oldest first.
func (c *Catalog) ListNotifyTargets(ctx context.Context) ([]NotifyTarget, error) {
	return queryRows(ctx, c.db, scanTarget, `SELECT `+targetCols+` FROM notification_targets ORDER BY id`)
}

// GetNotifyTarget returns one destination (ErrNotFound when there is none).
func (c *Catalog) GetNotifyTarget(ctx context.Context, id int64) (*NotifyTarget, error) {
	list, err := queryRows(ctx, c.db, scanTarget, `SELECT `+targetCols+` FROM notification_targets WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return &list[0], nil
}

func eventsJSON(events []string) string {
	if events == nil {
		events = []string{}
	}
	b, _ := json.Marshal(events)
	return string(b)
}

// CreateNotifyTarget adds a destination (already checked by the caller) and
// returns it. ErrTooManyTargets at MaxNotifyTargets.
func (c *Catalog) CreateNotifyTarget(ctx context.Context, t NotifyTarget) (*NotifyTarget, error) {
	var id int64
	err := c.db.WithTx(ctx, "notify: create target", func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM notification_targets`).Scan(&n); err != nil {
			return err
		}
		if n >= MaxNotifyTargets {
			return ErrTooManyTargets
		}
		now := c.ts()
		res, err := tx.ExecContext(ctx,
			`INSERT INTO notification_targets(kind, name, url, secret, enabled, events, created_at, updated_at)
			 VALUES(?,?,?,?,?,?,?,?)`,
			t.Kind, t.Name, t.URL, t.Secret, t.Enabled, eventsJSON(t.Events), now, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return nil, err
	}
	return c.GetNotifyTarget(ctx, id)
}

// SaveNotifyTarget writes a destination's settings (kind, name, url, secret,
// enabled, events) as given. ErrNotFound when it is gone.
func (c *Catalog) SaveNotifyTarget(ctx context.Context, t NotifyTarget) (*NotifyTarget, error) {
	res, err := c.db.ExecContext(ctx,
		`UPDATE notification_targets SET kind = ?, name = ?, url = ?, secret = ?, enabled = ?, events = ?, updated_at = ?
		 WHERE id = ?`,
		t.Kind, t.Name, t.URL, t.Secret, t.Enabled, eventsJSON(t.Events), c.ts(), t.ID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return c.GetNotifyTarget(ctx, t.ID)
}

// DeleteNotifyTarget removes a destination (ErrNotFound when there is none).
func (c *Catalog) DeleteNotifyTarget(ctx context.Context, id int64) error {
	res, err := c.db.ExecContext(ctx, `DELETE FROM notification_targets WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordDelivery records how a delivery to a destination went (errText is a short
// reason, never the URL or a response body). A destination deleted meanwhile is
// fine.
func (c *Catalog) RecordDelivery(ctx context.Context, id int64, ok bool, errText string) error {
	_, err := c.db.ExecContext(ctx,
		`UPDATE notification_targets SET last_at = ?, last_ok = ?, last_error = ? WHERE id = ?`,
		c.ts(), ok, errText, id)
	return err
}

// ServerEvent is one entry of the event feed. Data are its facts (names, counts,
// a version; never a secret or an IP).
type ServerEvent struct {
	ID   int64          `json:"id"`
	At   string         `json:"at"`
	Kind string         `json:"kind"`
	Data map[string]any `json:"data"`
}

// RecordServerEvent appends an event, stamped now, and returns it. dedupKey, when
// not empty, is what ServerEventSeen looks for.
func (c *Catalog) RecordServerEvent(ctx context.Context, kind string, data map[string]any, dedupKey string) (ServerEvent, error) {
	if data == nil {
		data = map[string]any{}
	}
	b, err := json.Marshal(data)
	if err != nil {
		return ServerEvent{}, err
	}
	at := formatSessionTime(c.now())
	res, err := c.db.ExecContext(ctx,
		`INSERT INTO server_events(at, kind, data, dedup_key) VALUES(?,?,?,?)`, at, kind, string(b), dedupKey)
	if err != nil {
		return ServerEvent{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return ServerEvent{}, err
	}
	// Round-trip the data so the caller sees what a reader will (numbers as float64).
	ev := ServerEvent{ID: id, At: at, Kind: kind, Data: map[string]any{}}
	_ = json.Unmarshal(b, &ev.Data)
	return ev, nil
}

// ServerEventSeen reports whether an event of this kind and key was recorded (and
// is still kept).
func (c *Catalog) ServerEventSeen(ctx context.Context, kind, dedupKey string) (bool, error) {
	var one int
	err := c.db.QueryRowContext(ctx,
		`SELECT 1 FROM server_events WHERE kind = ? AND dedup_key = ? LIMIT 1`, kind, dedupKey).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ListServerEvents returns events newest first (before > 0 pages: ids below it),
// and the before of the next page (0 when this is the last). A kind keeps only
// events of that kind.
func (c *Catalog) ListServerEvents(ctx context.Context, before int64, limit int, kind string) ([]ServerEvent, int64, error) {
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, 100)
	q := `SELECT id, at, kind, data FROM server_events WHERE 1 = 1`
	args := []any{}
	if before > 0 {
		q += ` AND id < ?`
		args = append(args, before)
	}
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit+1)
	events, err := queryRows(ctx, c.db, func(rows *sql.Rows, e *ServerEvent) error {
		var data string
		if err := rows.Scan(&e.ID, &e.At, &e.Kind, &data); err != nil {
			return err
		}
		e.Data = map[string]any{}
		_ = json.Unmarshal([]byte(data), &e.Data)
		return nil
	}, q, args...)
	if err != nil {
		return nil, 0, err
	}
	events, next := pageBefore(events, limit, func(e ServerEvent) int64 { return e.ID })
	return events, next, nil
}

// PruneServerEvents deletes events older than cutoff.
func (c *Catalog) PruneServerEvents(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := c.db.ExecContext(ctx, `DELETE FROM server_events WHERE at < ?`, formatSessionTime(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PreviousRunStatus is the status of the library's scan before the run runID ("" when
// there was none), so an offline library is announced once, not at every scan. Runs
// that never got to an answer (cancelled, cut short by a restart, still running)
// are passed over.
func (c *Catalog) PreviousRunStatus(ctx context.Context, libraryID, runID int64) (string, error) {
	var status string
	err := c.db.QueryRowContext(ctx,
		`SELECT status FROM scan_runs WHERE library_id = ? AND id < ? AND status NOT IN (?, ?, ?)
		 ORDER BY id DESC LIMIT 1`, libraryID, runID, RunCancelled, RunInterrupted, RunRunning).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return status, err
}
