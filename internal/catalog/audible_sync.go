package catalog

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
)

// Bridge Phase 4c: keeping an Audible account's places in step with this
// server, both ways. The Bridge reads the account's places on Audible (the
// audible.sync job, params built here: AudibleSyncTitles) and posts them for
// the account's person as a furthest-only batch (SyncBatch.FurthestOnly), and
// pushes the person's places here back to Audible from GET /bridge/progress
// (AudibleProgress). Only the titles the console lets sync take part: matched
// to a book here, not "Not this book" (rejected), not "Don't sync this book"
// (no_sync), not ignored (the manager registry's Ignored), not gone from the account. See CONVENTIONS.md, Audible sync.

var (
	// ErrAudibleSyncOff: the account's sync is off (403 sync_off).
	ErrAudibleSyncOff = errors.New("sync with Audible is off for the account")
	// ErrAudibleSyncUser: the account's person may no longer be synced for: a
	// kid, demo or disabled account since it was mapped (403 user_unavailable).
	// A deleted person takes the account with them (ErrUnknownAccount).
	ErrAudibleSyncUser = errors.New("the account's person can't be synced for")
)

// AudibleDevicePrefix starts the device id of an account's places
// ("audible:<account>").
const AudibleDevicePrefix = "audible:"

// IsAudibleDevice reports whether a device id names an Audible account.
func IsAudibleDevice(id string) bool { return strings.HasPrefix(id, AudibleDevicePrefix) }

// syncTitleSQL is a title (aliased t) whose book takes part in its account's
// sync.
const syncTitleSQL = `t.match_library_id IS NOT NULL AND t.match_path IS NOT NULL AND t.rejected = 0 AND t.ignored = 0 AND t.no_sync = 0
	AND t.state <> '` + AudibleGone + `'`

// AudibleSyncAccount is an account a sync call acts for: its person (whose
// scopes and progress rows the call reads and writes) and its names.
type AudibleSyncAccount struct {
	Account     string
	Label       string
	Marketplace string
	UserID      int64
	Admin       bool
}

// DeviceID is the device id the account's places are written under.
func (a AudibleSyncAccount) DeviceID() string { return AudibleDevicePrefix + a.Account }

// DeviceName is the name the account's places carry ("Audible (Chris US)"),
// the store's when the account has no label.
func (a AudibleSyncAccount) DeviceName() string {
	name := a.Label
	if name == "" {
		name = strings.ToUpper(a.Marketplace)
	}
	return "Audible (" + name + ")"
}

// AudibleSyncFor is account as a sync call acting for it needs it:
// ErrUnknownAccount for none (its person deleted, too), ErrAudibleSyncOff while
// its sync is off or the account is paused (its library deleted: no sync is
// queued for it either, DueAudibleSyncs), ErrAudibleSyncUser for a person an
// account may no longer be mapped to (bridgeUserSQL: disabled, a kid or a demo
// account).
func (c *Catalog) AudibleSyncFor(ctx context.Context, account string) (*AudibleSyncAccount, error) {
	a := AudibleSyncAccount{Account: account}
	var on, allowed bool
	var role string
	err := c.db.QueryRowContext(ctx,
		`SELECT a.label, a.marketplace, a.sync = 1 AND a.library_id IS NOT NULL, a.user_id, u.role, `+bridgeUserSQL+`
		   FROM audible_accounts a JOIN users u ON u.id = a.user_id WHERE a.account = ?`, account).
		Scan(&a.Label, &a.Marketplace, &on, &a.UserID, &role, &allowed)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrUnknownAccount
	case err != nil:
		return nil, err
	case !on:
		return nil, ErrAudibleSyncOff
	case !allowed:
		return nil, ErrAudibleSyncUser
	}
	a.Admin = role == "admin"
	return &a, nil
}

// AudibleSyncTitles are account's titles that take part in its sync
// (syncTitleSQL), by ASIN: audible.sync's params.
func (c *Catalog) AudibleSyncTitles(ctx context.Context, account string) ([]bridge.AudibleSyncTitle, error) {
	return queryRows(ctx, c.db, func(rows *sql.Rows, t *bridge.AudibleSyncTitle) error {
		return rows.Scan(&t.ASIN, &t.LibraryID, &t.Path, &t.RuntimeS)
	}, `SELECT t.asin, t.match_library_id, t.match_path, t.runtime_s FROM audible_titles t
		 WHERE t.account = ? AND `+syncTitleSQL+` ORDER BY t.asin`, account)
}

// AudibleSyncBooks are the books account's sync may write (its titles'
// matches, syncTitleSQL), as progress keys, each with "" or the reason the
// sync's events on it are rejected: duration_mismatch when no title on the
// book has a runtime that agrees with the indexed book's duration
// (audibleRuntimeAgrees: the title is matched to another edition or volume, so
// neither its places nor its finish are this book's).
func (c *Catalog) AudibleSyncBooks(ctx context.Context, account string) (map[Ref]string, error) {
	type row struct {
		ref      Ref
		runtimeS int64
		duration float64
	}
	titles, err := queryRows(ctx, c.db, func(rows *sql.Rows, x *row) error {
		return rows.Scan(&x.ref.LibraryID, &x.ref.Path, &x.runtimeS, &x.duration)
	}, `SELECT t.match_library_id, t.match_path, t.runtime_s, COALESCE(b.duration, 0) FROM audible_titles t
		  LEFT JOIN books b ON b.library_id = t.match_library_id AND b.rel_path = t.match_path
		 WHERE t.account = ? AND `+syncTitleSQL, account)
	if err != nil {
		return nil, err
	}
	out := make(map[Ref]string, len(titles))
	for _, t := range titles {
		if audibleRuntimeAgrees(t.runtimeS, t.duration) {
			out[t.ref] = ""
		} else if _, ok := out[t.ref]; !ok {
			out[t.ref] = reasonDurationMismatch
		}
	}
	return out, nil
}

// audibleRuntimeAgrees reports whether an Audible title's runtime (seconds)
// and the indexed book's duration are one recording's: within 5% of the
// longer plus 2 minutes (furthestPastEndRatio, the manager's wrong-match
// guard), or either unknown.
func audibleRuntimeAgrees(runtimeS int64, duration float64) bool {
	if runtimeS <= 0 || duration <= 0 {
		return true
	}
	rt := float64(runtimeS)
	return math.Abs(rt-duration) <= max(rt, duration)*(furthestPastEndRatio-1)+furthestPastEndSlack
}

// AudibleProgressRow is one of the person's places the Bridge may push to the
// account on Audible (GET /bridge/progress): the book, the title's ASIN and
// the row as stored (its duration, else the indexed book's).
type AudibleProgressRow struct {
	LibraryID int64   `json:"library_id"`
	Path      string  `json:"path"`
	ASIN      string  `json:"asin"`
	Position  float64 `json:"position"`
	Duration  float64 `json:"duration"`
	Finished  bool    `json:"finished"`
	UpdatedAt string  `json:"updated_at"`
	DeviceID  string  `json:"device_id"`
	Version   int64   `json:"version"`
}

// MaxAudibleProgress is the most rows one GET /bridge/progress answers, and
// defaultAudibleProgress its page when the call names no limit.
const (
	MaxAudibleProgress     = 500
	defaultAudibleProgress = 200
)

// progressCursor is a GET /bridge/progress position: the last row's
// (change_seq, asin), opaque on the wire (base64url JSON).
type progressCursor struct {
	Seq  int64  `json:"q"`
	ASIN string `json:"s"`
}

func (k progressCursor) encode() string {
	raw, _ := json.Marshal(k)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeProgressCursor(s string) (progressCursor, error) {
	if s == "" {
		return progressCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return progressCursor{}, audibleInvalid("bad cursor")
	}
	var k progressCursor
	if json.Unmarshal(raw, &k) != nil {
		return progressCursor{}, audibleInvalid("bad cursor")
	}
	return k, nil
}

// AudibleProgress is a page of the places of a's person that the Bridge may
// push to Audible: the progress rows on the books of the account's synced
// titles (syncTitleSQL) that the person can still reach (scopes, theirs), one
// per title (two titles on one book are two rows, each with its ASIN), not
// written by the account itself (the place Audible gave is never echoed back),
// written after the cursor since ("" from the start), in the order this server
// wrote them (progress.change_seq, migration 0066: a row written late with an
// earlier updated_at, such as a device's offline backlog or a diverged apply
// keeping the later time, still comes after the cursor), at most limit
// (defaultAudibleProgress for none, never past MaxAudibleProgress). next is
// where the next page starts (the last row read, or since when none was: a
// feed's cursor, kept for the next poll) and more whether a full page was read
// (ask again with next now). ErrAudibleInvalid for a cursor not made here.
func (c *Catalog) AudibleProgress(ctx context.Context, a *AudibleSyncAccount, scopes []Scope, since string, limit int) (
	rows []AudibleProgressRow, next string, more bool, err error) {
	from, err := decodeProgressCursor(since)
	if err != nil {
		return nil, "", false, err
	}
	if limit <= 0 {
		limit = defaultAudibleProgress
	}
	limit = min(limit, MaxAudibleProgress)
	type read struct {
		AudibleProgressRow
		seq int64
	}
	scoped, scopeArgs := scopesFilterSQL("p.library_id", "p.rel_path", scopes)
	args := append([]any{a.Account, a.UserID, a.DeviceID(), from.Seq, from.Seq, from.ASIN}, scopeArgs...)
	got, err := queryRows(ctx, c.db, func(r *sql.Rows, x *read) error {
		return r.Scan(&x.LibraryID, &x.Path, &x.ASIN, &x.Position, &x.Duration, &x.Finished, &x.UpdatedAt, &x.DeviceID,
			&x.Version, &x.seq)
	}, `SELECT p.library_id, p.rel_path, t.asin, p.position,
	           CASE WHEN p.duration > 0 THEN p.duration ELSE COALESCE(b.duration, 0) END,
	           p.finished, p.updated_at, p.device_id, p.version, p.change_seq
	      FROM progress p
	      JOIN audible_titles t ON t.account = ? AND t.match_library_id = p.library_id AND t.match_path = p.rel_path
	      LEFT JOIN books b ON b.library_id = p.library_id AND b.rel_path = p.rel_path
	     WHERE p.user_id = ? AND `+syncTitleSQL+` AND p.device_id <> ?
	       AND p.change_seq >= ? AND (p.change_seq, t.asin) > (?, ?) AND `+scoped+`
	     ORDER BY p.change_seq, t.asin
	     LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, "", false, err
	}
	rows = make([]AudibleProgressRow, len(got))
	next = since
	for i, x := range got {
		rows[i] = x.AudibleProgressRow
		next = progressCursor{Seq: x.seq, ASIN: x.ASIN}.encode()
	}
	return rows, next, len(rows) == limit, nil
}

// syncQueueableSQL is an account (aliased a, its person u) whose sync may be
// queued: sync on, not paused, not refused, mapped to a person an account may
// still be mapped to (bridgeUserSQL).
const syncQueueableSQL = `a.sync = 1 AND a.library_id IS NOT NULL AND a.refused_at IS NULL AND ` + bridgeUserSQL

// AudibleSyncQueueable reports whether account's sync may be queued now
// (syncQueueableSQL), whenever it was last queued: audible.SyncNow's check.
func (c *Catalog) AudibleSyncQueueable(ctx context.Context, account string) (bool, error) {
	var ok bool
	err := c.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM audible_accounts a JOIN users u ON u.id = a.user_id
		  WHERE a.account = ? AND `+syncQueueableSQL+`)`, account).Scan(&ok)
	return ok, err
}

// DueAudibleSyncs are the accounts whose audible.sync is due at now, at most
// limit, the longest waiting first: sync on, not paused, not refused, mapped to
// a person an account may still be mapped to (bridgeUserSQL), and not queued
// within bridge.AudibleSyncEvery.
func (c *Catalog) DueAudibleSyncs(ctx context.Context, now time.Time, limit int) ([]string, error) {
	type row struct {
		account string
		queued  sql.NullString
	}
	all, err := queryRows(ctx, c.db, func(r *sql.Rows, x *row) error { return r.Scan(&x.account, &x.queued) },
		`SELECT a.account, a.sync_queued_at FROM audible_accounts a JOIN users u ON u.id = a.user_id
		  WHERE `+syncQueueableSQL+`
		  ORDER BY a.sync_queued_at IS NOT NULL, a.sync_queued_at, a.account`)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, x := range all {
		if len(out) == limit {
			break
		}
		if x.queued.Valid {
			if at, err := time.Parse(time.RFC3339Nano, x.queued.String); err == nil && now.Sub(at) < bridge.AudibleSyncEvery {
				continue
			}
		}
		out = append(out, x.account)
	}
	return out, nil
}

// MarkAudibleSyncQueued records that account's sync was queued now (the
// scheduler counts bridge.AudibleSyncEvery from it). A user-state write.
func (c *Catalog) MarkAudibleSyncQueued(ctx context.Context, account string) error {
	_, err := c.db.ExecUserState(ctx, `UPDATE audible_accounts SET sync_queued_at = ? WHERE account = ?`, c.ts(), account)
	return err
}

// FoldAudibleSync records an ended audible.sync on its account: one that read
// Audible's places (bridge.AudibleSynced: done, neither waiting on Audible nor
// finding the sync off) is the account's last sync (last_sync_at), one that
// failed because Audible refused the account (bridge.SyncAudibleRefused) marks
// it refused, so none is queued until the Bridge maps the account again or
// polls it whole (PutAudibleTitles: Audible answered the account). Any other
// ending changes nothing (the next sync is due as usual). A user-state write.
func (c *Catalog) FoldAudibleSync(ctx context.Context, job bridge.Job) error {
	account, err := bridge.AudibleSyncOf(job)
	if err != nil {
		return err
	}
	switch {
	case bridge.AudibleSynced(job):
		_, err = c.db.ExecUserState(ctx, `UPDATE audible_accounts SET last_sync_at = ? WHERE account = ?`, c.ts(), account)
	case bridge.AudibleSyncCode(job) == bridge.SyncAudibleRefused:
		_, err = c.db.ExecUserState(ctx, `UPDATE audible_accounts SET refused_at = ? WHERE account = ?`, c.ts(), account)
	}
	return err
}
