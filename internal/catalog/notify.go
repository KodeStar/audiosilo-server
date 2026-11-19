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
// not empty, is what ServerEventSeen looks for. In the same transaction it
// queues the event in the outbox for every enabled destination subscribed to
// its kind (server#465), so a delivery still owed when the server stops is made
// at the next start (PendingDeliveries). On a snapshot (onSnapshot) both join
// the caller's transaction: the write that raised the event.
func (c *Catalog) RecordServerEvent(ctx context.Context, kind string, data map[string]any, dedupKey string) (ServerEvent, error) {
	if data == nil {
		data = map[string]any{}
	}
	b, err := json.Marshal(data)
	if err != nil {
		return ServerEvent{}, err
	}
	at := formatSessionTime(c.now())
	var id int64
	err = c.db.WithUserStateTx(ctx, "RecordServerEvent", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO server_events(at, kind, data, dedup_key) VALUES(?,?,?,?)`, at, kind, string(b), dedupKey)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO notify_outbox(event_id, target_id)
			 SELECT ?, id FROM notification_targets
			 WHERE enabled = 1 AND EXISTS (SELECT 1 FROM json_each(events) WHERE value = ?)`, id, kind)
		return err
	})
	if err != nil {
		return ServerEvent{}, err
	}
	// Round-trip the data so the caller sees what a reader will (numbers as float64).
	return ServerEvent{ID: id, At: at, Kind: kind, Data: eventData(b)}, nil
}

// eventData decodes an event's stored data (an empty map when it can't be read).
func eventData(b []byte) map[string]any {
	data := map[string]any{}
	_ = json.Unmarshal(b, &data)
	return data
}

// NotifyOutboxRetention is how long an event's outbox rows are kept: a
// delivery still pending then (a server stopped that long) is not made.
const NotifyOutboxRetention = 7 * 24 * time.Hour

// Outbox delivery states (notify_outbox.state), which starts 'pending' (written
// as a literal in the queries, so the pending rows' partial index serves them).
const (
	DeliveryDelivered = "delivered"
	// DeliveryFailed: the attempts ran out, or the destination refused it.
	DeliveryFailed = "failed"
	// DeliverySkipped: the destination was switched off or unsubscribed first.
	DeliverySkipped = "skipped"
)

// OutboxDelivery is one event owed to one destination. Attempts counts the
// sends begun, across restarts.
type OutboxDelivery struct {
	ID       int64
	TargetID int64
	Attempts int
	Event    ServerEvent
}

// PendingDeliveries returns the deliveries still owed (state pending) for one
// event, or for every event when eventID is 0 (the start's redelivery), oldest
// first. An event older than NotifyOutboxRetention is passed over.
func (c *Catalog) PendingDeliveries(ctx context.Context, eventID int64) ([]OutboxDelivery, error) {
	q := `SELECT o.id, o.target_id, o.attempts, e.id, e.at, e.kind, e.data
		FROM notify_outbox o JOIN server_events e ON e.id = o.event_id
		WHERE o.state = 'pending' AND e.at >= ?`
	args := []any{formatSessionTime(c.now().Add(-NotifyOutboxRetention))}
	if eventID != 0 {
		q += ` AND o.event_id = ?`
		args = append(args, eventID)
	}
	return queryRows(ctx, c.db, func(rows *sql.Rows, d *OutboxDelivery) error {
		var data []byte
		if err := rows.Scan(&d.ID, &d.TargetID, &d.Attempts, &d.Event.ID, &d.Event.At, &d.Event.Kind, &data); err != nil {
			return err
		}
		d.Event.Data = eventData(data)
		return nil
	}, q+` ORDER BY o.id`, args...)
}

// ClaimDelivery counts one more attempt at a pending delivery that has had
// attempts so far, and reports whether this caller has it: false when another
// took it first (the start's redelivery and the event's own Deliver both
// queue a new row) or it is no longer pending.
func (c *Catalog) ClaimDelivery(ctx context.Context, id int64, attempts int) (bool, error) {
	res, err := c.db.ExecUserState(ctx,
		`UPDATE notify_outbox SET attempts = attempts + 1 WHERE id = ? AND state = 'pending' AND attempts = ?`,
		id, attempts)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FinishDelivery ends a pending delivery in state (delivered, failed or
// skipped), with the failure's short reason.
func (c *Catalog) FinishDelivery(ctx context.Context, id int64, state, errText string) error {
	_, err := c.db.ExecUserState(ctx,
		`UPDATE notify_outbox SET state = ?, last_error = ? WHERE id = ? AND state = 'pending'`,
		state, errText, id)
	return err
}

// PruneNotifyOutbox deletes the outbox rows of events older than cutoff,
// whatever their state.
func (c *Catalog) PruneNotifyOutbox(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := c.db.ExecUserState(ctx,
		`DELETE FROM notify_outbox WHERE event_id IN (SELECT id FROM server_events WHERE at < ?)`,
		formatSessionTime(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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
		var data []byte
		if err := rows.Scan(&e.ID, &e.At, &e.Kind, &data); err != nil {
			return err
		}
		e.Data = eventData(data)
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
