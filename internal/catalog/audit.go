package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// The audit log: what admins did, and when (the console's Server > Audit log).
// Rows are written by the API's audit helper after an admin action succeeds, and
// by the server itself for what it does on an admin's behalf (a restore applied at
// start). Connections, playback and scans live elsewhere (Activity, Jobs).

// AuditRetention is how long audit events are kept.
const AuditRetention = 365 * 24 * time.Hour

// maxAuditEvents bounds the log however busy the admins are.
const maxAuditEvents = 100_000

// How an audit event's actor was signed in (AuditEvent.Via).
const (
	ViaSession = "session" // the console or another signed-in app
	ViaAPIKey  = "api"     // a personal API key
	ViaSystem  = "system"  // the server itself
)

// AuditEvent is one recorded admin action.
type AuditEvent struct {
	ID int64  `json:"id"`
	At string `json:"at"`
	// ActorID is the admin's account (nil for the server itself); ActorName is
	// their username when it happened, kept when the account is renamed or deleted.
	ActorID   *int64 `json:"actor_id"`
	ActorName string `json:"actor_name"`
	Via       string `json:"via"`
	// Action is a code the console words: "<area>.<verb>", like "user.update".
	Action string `json:"action"`
	// Target is what it was done to, as a person reads it (a username, a library's
	// name, a book's path).
	Target string `json:"target"`
	// Details are the facts of the change (which fields, from what to what). Never
	// a secret.
	Details map[string]any `json:"details"`
}

// RecordAudit appends an event, stamped now.
func (c *Catalog) RecordAudit(ctx context.Context, e AuditEvent) error {
	details := []byte("{}")
	if len(e.Details) > 0 {
		b, err := json.Marshal(e.Details)
		if err != nil {
			return err
		}
		details = b
	}
	if e.Via == "" {
		e.Via = ViaSession
	}
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO audit_events(at, actor_id, actor_name, via, action, target, details) VALUES(?,?,?,?,?,?,?)`,
		formatSessionTime(c.now()), e.ActorID, e.ActorName, e.Via, e.Action, e.Target, string(details))
	return err
}

// AuditFilter narrows ListAudit. Zero values match everything.
type AuditFilter struct {
	ActorID *int64
	// Area is an action's first part ("user" matches "user.create", "user.update").
	Area string
	// Query matches the target or the actor's name, ignoring case.
	Query string
	// Before pages: only events with a smaller id.
	Before int64
	Limit  int
}

// ListAudit returns events newest first, and the Before of the next page (0 when
// this is the last).
func (c *Catalog) ListAudit(ctx context.Context, f AuditFilter) ([]AuditEvent, int64, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	var where []string
	var args []any
	if f.ActorID != nil {
		where = append(where, `actor_id = ?`)
		args = append(args, *f.ActorID)
	}
	if f.Area != "" {
		where = append(where, `action LIKE ? ESCAPE '\'`)
		args = append(args, escapeLike(f.Area)+".%")
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		where = append(where, `(target LIKE ? ESCAPE '\' OR actor_name LIKE ? ESCAPE '\')`)
		like := "%" + escapeLike(q) + "%"
		args = append(args, like, like)
	}
	if f.Before > 0 {
		where = append(where, `id < ?`)
		args = append(args, f.Before)
	}
	query := `SELECT id, at, actor_id, actor_name, via, action, target, details FROM audit_events`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, f.Limit+1)
	events, err := queryRows(ctx, c.db, scanAudit, query, args...)
	if err != nil {
		return nil, 0, err
	}
	events, next := pageBefore(events, f.Limit, func(e AuditEvent) int64 { return e.ID })
	return events, next, nil
}

func scanAudit(rows *sql.Rows, e *AuditEvent) error {
	var actor sql.NullInt64
	var details string
	if err := rows.Scan(&e.ID, &e.At, &actor, &e.ActorName, &e.Via, &e.Action, &e.Target, &details); err != nil {
		return err
	}
	if actor.Valid {
		e.ActorID = &actor.Int64
	}
	e.Details = map[string]any{}
	_ = json.Unmarshal([]byte(details), &e.Details) // a damaged row still lists
	return nil
}

// PruneAudit deletes events older than cutoff, and the oldest past the newest
// maxAuditEvents, returning how many went.
func (c *Catalog) PruneAudit(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := c.db.ExecContext(ctx,
		`DELETE FROM audit_events WHERE at < ?
		    OR id <= (SELECT id FROM audit_events ORDER BY id DESC LIMIT 1 OFFSET ?)`,
		formatSessionTime(cutoff), maxAuditEvents)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
