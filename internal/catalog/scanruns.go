package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Scan history: one scan_runs row per library scan the job queue ran (the
// console's Jobs page). Records of the rebuildable index, so they go with their
// library and are kept to the newest maxScanRunsPerLibrary.

// maxScanRunsPerLibrary bounds the history kept for each library.
const maxScanRunsPerLibrary = 100

// Scan run statuses. A run is "running" until it finishes; "partial" finished but
// skipped pruning (part of the tree couldn't be read); "unavailable" stopped at the
// unavailable-root guard; "interrupted" belonged to a server that stopped mid-scan.
const (
	RunRunning     = "running"
	RunOK          = "ok"
	RunPartial     = "partial"
	RunUnavailable = "unavailable"
	RunFailed      = "failed"
	RunCancelled   = "cancelled"
	RunInterrupted = "interrupted"
)

// RunEvent is one line of a scan's log. Kind is a code the console words
// (started, discovered, unreadable, moved, problem, error, removed, partial,
// unavailable, failed, cancelled, finished, truncated); Path/To/Code/Detail/Count
// carry its facts. Code is a problem's code (books.scan_error); Detail is a tool's
// or the OS's own message, shown as-is.
type RunEvent struct {
	At     string `json:"at"`
	Level  string `json:"level"` // info | warn | error
	Kind   string `json:"kind"`
	Path   string `json:"path,omitempty"`
	Code   string `json:"code,omitempty"`
	To     string `json:"to,omitempty"`
	Detail string `json:"detail,omitempty"`
	Count  int    `json:"count,omitempty"`
}

// ScanCounts are what a scan found and changed.
type ScanCounts struct {
	Books   int `json:"books"` // discovered on disk
	Added   int `json:"added"`
	Updated int `json:"updated"`
	Moved   int `json:"moved"`
	Removed int `json:"removed"`
	Errors  int `json:"errors"`
}

// ScanRun is one recorded scan.
type ScanRun struct {
	ID            int64      `json:"id"`
	LibraryID     int64      `json:"library_id"`
	LibraryName   string     `json:"library_name"`
	Trigger       string     `json:"trigger"`
	StartedBy     *int64     `json:"started_by"`
	StartedByName string     `json:"started_by_name,omitempty"`
	StartedAt     string     `json:"started_at"`
	FinishedAt    *string    `json:"finished_at"`
	Status        string     `json:"status"`
	Log           []RunEvent `json:"log,omitempty"` // only on a single run
	ScanCounts
}

// StartScanRun records that a scan of a library started and returns the run's id.
// startedBy is the admin who asked for it (nil for a schedule or startup).
func (c *Catalog) StartScanRun(ctx context.Context, libraryID int64, trigger string, startedBy *int64) (int64, error) {
	res, err := c.db.ExecContext(ctx,
		`INSERT INTO scan_runs(library_id, trigger, started_by, started_at, status) VALUES(?,?,?,?,?)`,
		libraryID, trigger, startedBy, c.ts(), RunRunning)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishScanRun records a run's outcome and log, and drops the library's runs
// beyond the newest maxScanRunsPerLibrary.
func (c *Catalog) FinishScanRun(ctx context.Context, id int64, status string, n ScanCounts, log []RunEvent) error {
	if log == nil {
		log = []RunEvent{}
	}
	raw, err := json.Marshal(log)
	if err != nil {
		return err
	}
	return c.db.WithTx(ctx, "FinishScanRun", func(tx *sql.Tx) error {
		var libraryID int64
		if err := tx.QueryRowContext(ctx,
			`UPDATE scan_runs SET finished_at = ?, status = ?, books = ?, added = ?, updated = ?,
			        moved = ?, removed = ?, errors = ?, log = ?
			  WHERE id = ? RETURNING library_id`,
			c.ts(), status, n.Books, n.Added, n.Updated, n.Moved, n.Removed, n.Errors, string(raw), id,
		).Scan(&libraryID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil // its library was deleted while it ran
			}
			return err
		}
		_, err := tx.ExecContext(ctx,
			`DELETE FROM scan_runs WHERE library_id = ? AND id NOT IN
			   (SELECT id FROM scan_runs WHERE library_id = ? ORDER BY id DESC LIMIT ?)`,
			libraryID, libraryID, maxScanRunsPerLibrary)
		return err
	})
}

// InterruptScanRuns marks runs a previous server left running (it stopped
// mid-scan) as interrupted. Called once, before the job queue starts.
func (c *Catalog) InterruptScanRuns(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx,
		`UPDATE scan_runs SET status = ?, finished_at = started_at WHERE finished_at IS NULL`, RunInterrupted)
	return err
}

const scanRunCols = `r.id, r.library_id, l.name, r.trigger, r.started_by, COALESCE(u.username, ''),
	r.started_at, r.finished_at, r.status, r.books, r.added, r.updated, r.moved, r.removed, r.errors`

const scanRunFrom = ` FROM scan_runs r JOIN libraries l ON l.id = r.library_id
	LEFT JOIN users u ON u.id = r.started_by`

func scanRunDest(r *ScanRun) []any {
	return []any{&r.ID, &r.LibraryID, &r.LibraryName, &r.Trigger, &r.StartedBy, &r.StartedByName,
		&r.StartedAt, &r.FinishedAt, &r.Status, &r.Books, &r.Added, &r.Updated, &r.Moved, &r.Removed, &r.Errors}
}

// ListScanRuns returns recorded runs newest first, without their logs: of one
// library (or all, libraryID 0), older than the run `before` (0 = from the newest),
// at most limit (1-200, default 50).
func (c *Catalog) ListScanRuns(ctx context.Context, libraryID, before int64, limit int) ([]ScanRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return queryRows(ctx, c.db, func(rows *sql.Rows, r *ScanRun) error {
		return rows.Scan(scanRunDest(r)...)
	}, `SELECT `+scanRunCols+scanRunFrom+`
	     WHERE (?1 = 0 OR r.library_id = ?1) AND (?2 = 0 OR r.id < ?2)
	     ORDER BY r.id DESC LIMIT ?3`, libraryID, before, limit)
}

// GetScanRun returns one run with its log (ErrNotFound if there's none).
func (c *Catalog) GetScanRun(ctx context.Context, id int64) (*ScanRun, error) {
	var r ScanRun
	var raw string
	err := c.db.QueryRowContext(ctx, `SELECT `+scanRunCols+`, r.log`+scanRunFrom+` WHERE r.id = ?`, id).
		Scan(append(scanRunDest(&r), &raw)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.Log = []RunEvent{}
	if err := json.Unmarshal([]byte(raw), &r.Log); err != nil {
		return nil, err
	}
	return &r, nil
}

// LastScanStarts returns when each library's newest recorded scan started (what a
// schedule counts from), by library id. Libraries never scanned are absent.
func (c *Catalog) LastScanStarts(ctx context.Context) (map[int64]time.Time, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT library_id, MAX(started_at) FROM scan_runs GROUP BY library_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]time.Time{}
	for rows.Next() {
		var id int64
		var at string
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339, at); err == nil {
			out[id] = t
		}
	}
	return out, rows.Err()
}

// LastScanFinished returns when the newest finished scan of any library ended
// ("" when none has): the Health page's "checked N ago".
func (c *Catalog) LastScanFinished(ctx context.Context) (string, error) {
	var at sql.NullString
	err := c.db.QueryRowContext(ctx,
		`SELECT MAX(finished_at) FROM scan_runs WHERE status <> ?`, RunInterrupted).Scan(&at)
	return at.String, err
}
