package catalog

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
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
// (ignored), not gone from the account. See CONVENTIONS.md, Audible sync.

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

// syncTitleSQL is a title (aliased t) whose book takes part in its account's
// sync.
const syncTitleSQL = `t.match_library_id IS NOT NULL AND t.match_path IS NOT NULL AND t.rejected = 0 AND t.ignored = 0
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
// its sync is off, ErrAudibleSyncUser for a person an account may no longer be
// mapped to (bridgeUserSQL: disabled, a kid or a demo account).
func (c *Catalog) AudibleSyncFor(ctx context.Context, account string) (*AudibleSyncAccount, error) {
	a := AudibleSyncAccount{Account: account}
	var on, allowed bool
	var role string
	err := c.db.QueryRowContext(ctx,
		`SELECT a.label, a.marketplace, a.sync, a.user_id, u.role, `+bridgeUserSQL+`
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
// matches, AudibleSyncTitles), as progress keys.
func (c *Catalog) AudibleSyncBooks(ctx context.Context, account string) (map[Ref]bool, error) {
	titles, err := c.AudibleSyncTitles(ctx, account)
	if err != nil {
		return nil, err
	}
	out := make(map[Ref]bool, len(titles))
	for _, t := range titles {
		out[Ref{LibraryID: t.LibraryID, Path: t.Path}] = true
	}
	return out, nil
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

// MaxAudibleProgress is the most rows one GET /bridge/progress answers.
const MaxAudibleProgress = 500

// progressCursor is a GET /bridge/progress position: the last row's
// (updated_at, library_id, path), opaque on the wire (base64url JSON).
type progressCursor struct {
	At   string
	Lib  int64
	Path string
}

func (k progressCursor) encode() string {
	raw, _ := json.Marshal([]any{k.At, k.Lib, k.Path})
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
	var parts []json.RawMessage
	var k progressCursor
	if json.Unmarshal(raw, &parts) != nil || len(parts) != 3 || json.Unmarshal(parts[0], &k.At) != nil ||
		json.Unmarshal(parts[1], &k.Lib) != nil || json.Unmarshal(parts[2], &k.Path) != nil {
		return progressCursor{}, audibleInvalid("bad cursor")
	}
	return k, nil
}

// AudibleProgress is a page of the places of a's person that the Bridge may
// push to Audible: the progress rows on the books of the account's synced
// titles (syncTitleSQL) that the person can still reach (scopes, theirs), not
// written by the account itself (the place Audible gave is never echoed back),
// changed after the cursor since ("" from the start), oldest change first, at
// most limit (MaxAudibleProgress). next is where the next page starts (the
// last row read, or since when none was: a feed's cursor, kept for the next
// poll) and more whether a full page was read (ask again with next now). A
// row whose updated_at stayed (a diverged apply keeps the later time) is not
// read again. ErrAudibleInvalid for a cursor not made here.
func (c *Catalog) AudibleProgress(ctx context.Context, a *AudibleSyncAccount, scopes []Scope, since string, limit int) (
	rows []AudibleProgressRow, next string, more bool, err error) {
	from, err := decodeProgressCursor(since)
	if err != nil {
		return nil, "", false, err
	}
	if limit <= 0 || limit > MaxAudibleProgress {
		limit = MaxAudibleProgress
	}
	read, err := queryRows(ctx, c.db, func(r *sql.Rows, x *AudibleProgressRow) error {
		return r.Scan(&x.LibraryID, &x.Path, &x.ASIN, &x.Position, &x.Duration, &x.Finished, &x.UpdatedAt, &x.DeviceID,
			&x.Version)
	}, `SELECT p.library_id, p.rel_path, MIN(t.asin), p.position,
	           CASE WHEN p.duration > 0 THEN p.duration ELSE COALESCE(b.duration, 0) END,
	           p.finished, p.updated_at, p.device_id, p.version
	      FROM audible_titles t
	      JOIN progress p ON p.user_id = ? AND p.library_id = t.match_library_id AND p.rel_path = t.match_path
	      LEFT JOIN books b ON b.library_id = p.library_id AND b.rel_path = p.rel_path
	     WHERE t.account = ? AND `+syncTitleSQL+` AND p.device_id <> ?
	       AND (p.updated_at, p.library_id, p.rel_path) > (?, ?, ?)
	     GROUP BY p.library_id, p.rel_path
	     ORDER BY p.updated_at, p.library_id, p.rel_path
	     LIMIT ?`, a.UserID, a.Account, a.DeviceID(), from.At, from.Lib, from.Path, limit)
	if err != nil {
		return nil, "", false, err
	}
	rows = []AudibleProgressRow{}
	next = since
	for _, x := range read {
		next = progressCursor{At: x.UpdatedAt, Lib: x.LibraryID, Path: x.Path}.encode()
		if scope, ok := scopeOf(scopes, x.LibraryID); ok && scope.Allows(x.Path) {
			rows = append(rows, x)
		}
	}
	return rows, next, len(read) == limit, nil
}

// scopeOf is the scope for libraryID among scopes.
func scopeOf(scopes []Scope, libraryID int64) (Scope, bool) {
	for _, s := range scopes {
		if s.LibraryID == libraryID {
			return s, true
		}
	}
	return Scope{}, false
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
		  WHERE a.sync = 1 AND a.library_id IS NOT NULL AND a.refused_at IS NULL AND `+bridgeUserSQL+`
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

// FoldAudibleSync records an ended audible.sync on its account: a done one is
// the account's last sync (last_sync_at), one that failed because Audible
// refused the account (bridge.SyncAudibleRefused) marks it refused, so none is
// queued until the Bridge maps the account again. Any other ending changes
// nothing (the next sync is due as usual). A user-state write.
func (c *Catalog) FoldAudibleSync(ctx context.Context, job bridge.Job) error {
	account, err := bridge.AudibleSyncOf(job)
	if err != nil {
		return err
	}
	switch {
	case job.State == bridge.StateDone:
		_, err = c.db.ExecUserState(ctx, `UPDATE audible_accounts SET last_sync_at = ? WHERE account = ?`, c.ts(), account)
	case bridge.AudibleSyncCode(job) == bridge.SyncAudibleRefused:
		_, err = c.db.ExecUserState(ctx, `UPDATE audible_accounts SET refused_at = ? WHERE account = ?`, c.ts(), account)
	}
	return err
}
