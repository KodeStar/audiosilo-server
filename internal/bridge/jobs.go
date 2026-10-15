package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Job states (migration 0047).
const (
	StateQueued         = "queued"
	StateClaimed        = "claimed"
	StateRunning        = "running"
	StateDone           = "done"
	StateFailed         = "failed"
	StateNeedsAttention = "needs_attention"
	StateCancelled      = "cancelled"
)

// openStates are the states a job is still the Bridge's business in; a kind,
// library and path have at most one job in them (dedupe_key).
const openStatesSQL = `('queued', 'claimed', 'running', 'needs_attention')`

// IsOpen reports whether state is one a job can still move on from.
func IsOpen(state string) bool {
	switch state {
	case StateQueued, StateClaimed, StateRunning, StateNeedsAttention:
		return true
	}
	return false
}

const (
	// Lease is how long a claim holds a job without a progress update. The
	// Bridge reports at least every 30 s while it works.
	Lease = 2 * time.Minute
	// MaxAttempts is how many times a job may be claimed; a lease that runs out
	// on the last attempt fails the job instead of queueing it again.
	MaxAttempts = 3
	// MaxWait bounds a claim's long poll, under the server's 30 s request
	// timeout.
	MaxWait = 25 * time.Second

	maxParamsBytes   = 16 << 10
	maxProgressBytes = 16 << 10
	maxResultBytes   = 64 << 10
	maxErrorBytes    = 2 << 10
)

// KindVerify is Phase 0's proof job: the Bridge stats a book (or a library's
// root) through its mapping and reports what it found.
const KindVerify = "bridge.verify"

// kindSpec is what the server knows about a job kind: whether a job of it is
// about a library and a book, and how to check its params.
type kindSpec struct {
	needsLibrary bool
	// bookOptional: a path may be given (a book) or not (the library itself).
	bookOptional bool
	params       func(json.RawMessage) (json.RawMessage, error)
}

// kinds are the job kinds the server can queue (and check). A kind the Bridge
// advertises that isn't here is never queued, and neither is one here that the
// Bridge doesn't advertise.
var kinds = map[string]kindSpec{
	KindVerify: {needsLibrary: true, bookOptional: true, params: verifyParams},
}

// verifyParams checks bridge.verify's params: an optional hold_seconds (0-600)
// the Bridge waits before finishing, so a test can cancel it or kill the Bridge
// mid-job.
func verifyParams(raw json.RawMessage) (json.RawMessage, error) {
	var p struct {
		HoldSeconds int `json:"hold_seconds"`
	}
	if err := decodeStrict(raw, &p); err != nil {
		return nil, invalid("params: %v", err)
	}
	if p.HoldSeconds < 0 || p.HoldSeconds > 600 {
		return nil, invalid("hold_seconds must be 0-600")
	}
	return json.Marshal(p)
}

// decodeStrict decodes one JSON object into v, refusing unknown fields and
// trailing data. Empty input is an empty object.
func decodeStrict(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

// Known reports whether the server knows kind.
func Known(kind string) bool {
	_, ok := kinds[kind]
	return ok
}

// Job is one bridge_jobs row on the wire (admin and Bridge routes alike).
type Job struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	LibraryID *int64          `json:"library_id"`
	Path      *string         `json:"path"`
	Params    json.RawMessage `json:"params"`
	State     string          `json:"state"`
	Progress  json.RawMessage `json:"progress"`
	Result    json.RawMessage `json:"result"`
	Error     string          `json:"error"`
	// RequestedBy is the admin who asked, nil once they are deleted.
	RequestedBy     *Requester `json:"requested_by"`
	Priority        int        `json:"priority"`
	Attempts        int        `json:"attempts"`
	LeaseUntil      *string    `json:"lease_until"`
	CancelRequested bool       `json:"cancel_requested"`
	CreatedAt       string     `json:"created_at"`
	UpdatedAt       string     `json:"updated_at"`
	ClaimedAt       *string    `json:"claimed_at"`
	FinishedAt      *string    `json:"finished_at"`
}

// Requester names the admin who queued a job.
type Requester struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

const jobColumns = `j.id, j.kind, j.library_id, j.rel_path, j.params, j.state, j.progress, j.result, j.error,
	j.requested_by, u.username, j.priority, j.attempts, j.lease_until, j.cancel_requested,
	j.created_at, j.updated_at, j.claimed_at, j.finished_at`

const jobFrom = ` FROM bridge_jobs j LEFT JOIN users u ON u.id = j.requested_by`

type rowScanner interface{ Scan(...any) error }

func scanJob(row rowScanner) (*Job, error) {
	var (
		j                        Job
		lib, by                  sql.NullInt64
		path, name               sql.NullString
		lease, claimed, finished sql.NullString
		params, progress, result string
	)
	if err := row.Scan(&j.ID, &j.Kind, &lib, &path, &params, &j.State, &progress, &result, &j.Error,
		&by, &name, &j.Priority, &j.Attempts, &lease, &j.CancelRequested,
		&j.CreatedAt, &j.UpdatedAt, &claimed, &finished); err != nil {
		return nil, err
	}
	if lib.Valid {
		j.LibraryID = &lib.Int64
	}
	if path.Valid {
		j.Path = &path.String
	}
	if by.Valid && name.Valid {
		j.RequestedBy = &Requester{ID: by.Int64, Username: name.String}
	}
	j.Params, j.Progress, j.Result = json.RawMessage(params), json.RawMessage(progress), json.RawMessage(result)
	j.LeaseUntil, j.ClaimedAt, j.FinishedAt = nullable(lease), nullable(claimed), nullable(finished)
	return &j, nil
}

func nullable(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

// ErrNotFound is a job id with no job.
var ErrNotFound = errors.New("job not found")

// DedupeKey is a job's identity among the open jobs: its kind, library and path.
// catalog.moveBookState rebuilds it in SQL on a move (kind || '|' || library_id
// || '|' || path); the two must agree.
func DedupeKey(kind string, libraryID *int64, path *string) string {
	var b strings.Builder
	b.WriteString(kind)
	b.WriteByte('|')
	if libraryID != nil {
		b.WriteString(strconv.FormatInt(*libraryID, 10))
	}
	b.WriteByte('|')
	if path != nil {
		b.WriteString(*path)
	}
	return b.String()
}

// NewJob is an admin's request for a job, after the transport resolved its book.
type NewJob struct {
	Kind      string
	LibraryID *int64
	// Path is the book's indexed path (nil: the job is about the library itself).
	Path        *string
	Params      json.RawMessage
	RequestedBy int64
	Priority    int
}

// CheckNew validates a request against the kind: known, its params well formed
// (normalised in place), a library when the kind needs one and a path only when
// it allows one. It doesn't check that the Bridge offers the kind (the caller
// holds the heartbeat) or that the book exists (the transport resolves it).
func CheckNew(j *NewJob) error {
	spec, ok := kinds[j.Kind]
	if !ok {
		return invalid("unknown kind %q", j.Kind)
	}
	if spec.needsLibrary && j.LibraryID == nil {
		return invalid("library_id is required")
	}
	if !spec.needsLibrary && (j.LibraryID != nil || j.Path != nil) {
		return invalid("this kind takes no library or path")
	}
	if j.Path != nil && !spec.bookOptional {
		return invalid("this kind takes no path")
	}
	if len(j.Params) > maxParamsBytes {
		return invalid("params too large")
	}
	params, err := spec.params(j.Params)
	if err != nil {
		return err
	}
	j.Params = params
	return nil
}

// Enqueue queues j (checked by CheckNew) and returns it, or the open job with
// the same kind, library and path when there is one (created false).
func (s *Service) Enqueue(ctx context.Context, j NewJob) (job *Job, created bool, err error) {
	key := DedupeKey(j.Kind, j.LibraryID, j.Path)
	var id int64
	err = s.db.WithTx(ctx, "bridge.Enqueue", func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM bridge_jobs WHERE dedupe_key = ? AND state IN `+openStatesSQL, key).Scan(&id)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		now := s.ts()
		res, err := tx.ExecContext(ctx,
			`INSERT INTO bridge_jobs(kind, library_id, rel_path, params, requested_by, priority, dedupe_key, created_at, updated_at)
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			j.Kind, j.LibraryID, j.Path, string(j.Params), j.RequestedBy, j.Priority, key, now, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		created = true
		return err
	})
	if err != nil {
		return nil, false, err
	}
	if created {
		s.wake.fire()
	}
	job, err = s.Get(ctx, id)
	return job, created, err
}

// Get returns one job.
func (s *Service) Get(ctx context.Context, id int64) (*Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobColumns+jobFrom+` WHERE j.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// getTx is Get inside a write transaction (the writer's view, not the reader
// pool's).
func getTx(ctx context.Context, tx *sql.Tx, id int64) (*Job, error) {
	j, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+jobFrom+` WHERE j.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// Filter narrows List.
type Filter struct {
	LibraryID *int64
	Path      *string
	State     string
	// Before is the previous page's last id (0: the first page).
	Before int64
	Limit  int
}

// List returns jobs newest first, after expiring the leases that ran out (so
// the console shows what the Bridge is really doing).
func (s *Service) List(ctx context.Context, f Filter) ([]Job, error) {
	if err := s.ExpireLeases(ctx); err != nil {
		return nil, err
	}
	var (
		where []string
		args  []any
	)
	if f.LibraryID != nil {
		where, args = append(where, "j.library_id = ?"), append(args, *f.LibraryID)
	}
	if f.Path != nil {
		where, args = append(where, "j.rel_path = ?"), append(args, *f.Path)
	}
	if f.State != "" {
		where, args = append(where, "j.state = ?"), append(args, f.State)
	}
	if f.Before > 0 {
		where, args = append(where, "j.id < ?"), append(args, f.Before)
	}
	q := `SELECT ` + jobColumns + jobFrom
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q += ` ORDER BY j.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// expireSQL ends every lease that ran out (?1 is now): a job whose cancel was
// asked for is cancelled, one on its last attempt failed, the rest queued again.
const expireSQL = `UPDATE bridge_jobs SET
	    state = CASE WHEN cancel_requested = 1 THEN 'cancelled'
	                 WHEN attempts >= ?2 THEN 'failed'
	                 ELSE 'queued' END,
	    error = CASE WHEN cancel_requested = 1 THEN error
	                 WHEN attempts >= ?2 THEN 'the Bridge stopped reporting on every attempt'
	                 ELSE error END,
	    finished_at = CASE WHEN cancel_requested = 1 OR attempts >= ?2 THEN ?1 ELSE NULL END,
	    lease_until = NULL, updated_at = ?1
	  WHERE state IN ('claimed', 'running') AND lease_until < ?1`

// ExpireLeases ends the leases that ran out (expireSQL). Claims and lists run it
// first, so a Bridge that died mid-job leaves the job queued again (or failed)
// by the time anyone looks.
func (s *Service) ExpireLeases(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, expireSQL, s.ts(), MaxAttempts)
	return err
}

// Claim hands the Bridge the next queued job of one of kinds (highest priority,
// then oldest), leased for Lease, or nil when there is none.
func (s *Service) Claim(ctx context.Context, kinds []string) (*Job, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	var job *Job
	err := s.db.WithTx(ctx, "bridge.Claim", func(tx *sql.Tx) error {
		now := s.ts()
		if _, err := tx.ExecContext(ctx, expireSQL, now, MaxAttempts); err != nil {
			return err
		}
		args := []any{}
		for _, k := range kinds {
			args = append(args, k)
		}
		var id int64
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM bridge_jobs WHERE state = 'queued' AND kind IN (`+placeholders(len(kinds))+`)
			  ORDER BY priority DESC, id LIMIT 1`, args...).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE bridge_jobs SET state = 'claimed', attempts = attempts + 1, lease_until = ?, claimed_at = ?,
			        updated_at = ? WHERE id = ?`, at(s.now().Add(Lease)), now, now, id); err != nil {
			return err
		}
		job, err = getTx(ctx, tx, id)
		return err
	})
	return job, err
}

// WaitClaim is Claim that waits up to wait (at most MaxWait) for a job to be
// queued when there is none, returning nil when none came or ctx ended.
func (s *Service) WaitClaim(ctx context.Context, kinds []string, wait time.Duration) (*Job, error) {
	wait = min(max(wait, 0), MaxWait)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		woken := s.wake.wait()
		job, err := s.Claim(ctx, kinds)
		if job != nil || err != nil {
			return job, err
		}
		select {
		case <-woken:
		case <-timer.C:
			return nil, nil
		case <-ctx.Done():
			return nil, nil
		}
	}
}

// Update is what the Bridge reports about a job it holds (PATCH
// /bridge/jobs/{id}). Attempt is the attempts value its claim returned, so a
// Bridge reporting on a claim that expired (and was claimed again) is refused.
// Fields left nil keep their value.
type Update struct {
	Attempt  int             `json:"attempt"`
	State    *string         `json:"state"`
	Progress json.RawMessage `json:"progress"`
	Result   json.RawMessage `json:"result"`
	Error    *string         `json:"error"`
}

// ErrNotHeld is an update for a job the caller no longer holds: it ended, was
// cancelled, or its lease ran out and a later claim took it (a 409).
var ErrNotHeld = errors.New("the job is not held by this claim")

// fromHeld are the states the Bridge may report from (it holds the job) and
// where each may go.
var fromHeld = map[string][]string{
	StateClaimed:        {StateRunning, StateDone, StateFailed, StateNeedsAttention, StateCancelled},
	StateRunning:        {StateRunning, StateDone, StateFailed, StateNeedsAttention, StateCancelled},
	StateNeedsAttention: {StateRunning, StateDone, StateFailed, StateNeedsAttention, StateCancelled},
}

// checkUpdate bounds what the Bridge sends: progress and result are JSON
// objects, the error bounded text.
func checkUpdate(u *Update) error {
	for name, raw := range map[string]struct {
		v   json.RawMessage
		max int
	}{"progress": {u.Progress, maxProgressBytes}, "result": {u.Result, maxResultBytes}} {
		if raw.v == nil {
			continue
		}
		if len(raw.v) > raw.max {
			return invalid("%s too large", name)
		}
		var obj map[string]any
		if err := json.Unmarshal(raw.v, &obj); err != nil || obj == nil {
			return invalid("%s must be a JSON object", name)
		}
	}
	if u.Error != nil && (len(*u.Error) > maxErrorBytes || checkText("error", strings.ReplaceAll(*u.Error, "\n", " "), maxErrorBytes) != nil) {
		return invalid("error")
	}
	return nil
}

// UpdateHeld applies the Bridge's update to job id. The job must be held by
// this claim (u.Attempt) in a state the Bridge reports from, and the new state
// one it may go to; a claimed or running job's lease is renewed, a parked one's
// dropped, a finished one's ended. It returns the job as stored, whose
// CancelRequested tells the Bridge an admin asked it to stop.
func (s *Service) UpdateHeld(ctx context.Context, id int64, u Update) (*Job, error) {
	if err := checkUpdate(&u); err != nil {
		return nil, err
	}
	var job *Job
	err := s.db.WithTx(ctx, "bridge.UpdateHeld", func(tx *sql.Tx) error {
		now := s.ts()
		if _, err := tx.ExecContext(ctx, expireSQL, now, MaxAttempts); err != nil {
			return err
		}
		cur, err := getTx(ctx, tx, id)
		if err != nil {
			return err
		}
		next, ok := fromHeld[cur.State]
		if !ok || cur.Attempts != u.Attempt {
			return ErrNotHeld
		}
		state := cur.State
		if u.State != nil {
			if !slices.Contains(next, *u.State) {
				return invalid("state %q from %q", *u.State, cur.State)
			}
			state = *u.State
		}
		var lease, finished any
		switch state {
		case StateClaimed, StateRunning:
			lease = at(s.now().Add(Lease))
		case StateDone, StateFailed, StateCancelled:
			finished = now
		}
		// A field the update leaves out (nil) keeps its value.
		var progress, result, errText any
		if u.Progress != nil {
			progress = string(u.Progress)
		}
		if u.Result != nil {
			result = string(u.Result)
		}
		if u.Error != nil {
			errText = *u.Error
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE bridge_jobs SET state = ?, lease_until = ?, finished_at = ?, updated_at = ?,
			        progress = COALESCE(?, progress), result = COALESCE(?, result), error = COALESCE(?, error)
			  WHERE id = ?`, state, lease, finished, now, progress, result, errText, id); err != nil {
			return err
		}
		job, err = getTx(ctx, tx, id)
		return err
	})
	return job, err
}

// ErrFinished is a cancel for a job that already ended (a 409).
var ErrFinished = errors.New("the job has already finished")

// Cancel cancels job id: at once when it is queued or parked (no Bridge is
// working on it), else by asking the Bridge (cancel_requested), which stops at
// its next update; a lease that runs out on such a job cancels it then.
func (s *Service) Cancel(ctx context.Context, id int64) (*Job, error) {
	var job *Job
	err := s.db.WithTx(ctx, "bridge.Cancel", func(tx *sql.Tx) error {
		now := s.ts()
		if _, err := tx.ExecContext(ctx, expireSQL, now, MaxAttempts); err != nil {
			return err
		}
		cur, err := getTx(ctx, tx, id)
		if err != nil {
			return err
		}
		switch cur.State {
		case StateQueued, StateNeedsAttention:
			_, err = tx.ExecContext(ctx,
				`UPDATE bridge_jobs SET state = 'cancelled', cancel_requested = 1, lease_until = NULL,
				        finished_at = ?1, updated_at = ?1 WHERE id = ?2`, now, id)
		case StateClaimed, StateRunning:
			_, err = tx.ExecContext(ctx,
				`UPDATE bridge_jobs SET cancel_requested = 1, updated_at = ? WHERE id = ?`, now, id)
		default:
			return ErrFinished
		}
		if err != nil {
			return err
		}
		job, err = getTx(ctx, tx, id)
		return err
	})
	return job, err
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return "?" + strings.Repeat(",?", n-1)
}

// String is a job's short name in logs.
func (j *Job) String() string { return fmt.Sprintf("%s#%d", j.Kind, j.ID) }
