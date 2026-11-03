package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
)

// Bridge Phase 4b: backing up Audible titles (the audible.import job). The
// console asks for one title ("Back up"), or every title not here yet ("Back up
// everything"); after each poll the account's policy asks for the new
// purchases (all, or those in a series the person already has). The server
// builds each job's params (bridge.AudibleImportParams) from the account and
// the title, and the done job's result becomes the title's match (how
// "import"). A title's backup is its import_job_id (the bridge_jobs row), or
// import_pending for a request on a preorder, queued once a poll says it is
// available.

// AudibleMatchImport is the match a done backup sets (a poll reports asin or
// fuzzy; "import" is the server's).
const AudibleMatchImport = "import"

// AudibleImportPending is a title's import state while a console request waits
// for its preorder to become available.
const AudibleImportPending = "pending"

// AudiblePreorderWait is how long after its release date a preorder that is
// still not available keeps its account checked hourly; then it needs
// attention (AudibleTitle.NeedsAttention).
const AudiblePreorderWait = 14 * 24 * time.Hour

// audibleBytesPerSecond approximates a backup's size from its runtime: AAXC at
// 64 kbps (the console labels it approximate).
const audibleBytesPerSecond = 8000

var (
	// ErrUnknownTitle is an (account, ASIN) with no title: a 404.
	ErrUnknownTitle = errors.New("unknown audible title")
	// ErrAudibleMatched is a backup of a title already on this server (a match
	// the admin hasn't marked "Not this book"): a 409 already_matched.
	ErrAudibleMatched = errors.New("the title is already on this server")
	// ErrAudibleImportOpen is a backup of a title whose backup is still open: a
	// 409 import_open.
	ErrAudibleImportOpen = errors.New("a backup of the title is already open")
	// ErrAudibleNotAvailable is a backup of a title the account can't download
	// (unavailable, or gone from its library): a 409 not_available.
	ErrAudibleNotAvailable = errors.New("the title can't be downloaded")
	// ErrAudiblePaused is a backup for a paused account (its library was
	// deleted): a 409 account_paused.
	ErrAudiblePaused = errors.New("the account is paused")
)

// AudibleImport is a title's backup as the console shows it: its job and the
// job's state and code (bridge.AudibleImportCode), or a pending request.
type AudibleImport struct {
	// JobID is nil for a pending request.
	JobID *int64 `json:"job_id"`
	// State is the job's state, or AudibleImportPending.
	State string `json:"state"`
	// Code is a failed or parked job's code ("" for none).
	Code string `json:"code"`
}

// AudibleEstimate is what "Back up everything" would queue: how many titles and
// about how many bytes.
type AudibleEstimate struct {
	Count int   `json:"count"`
	Bytes int64 `json:"bytes"`
}

// openImportSQL is a title (aliased t) whose backup job is open.
const openImportSQL = `EXISTS(SELECT 1 FROM bridge_jobs j WHERE j.id = t.import_job_id AND j.state IN ` + bridge.OpenStatesSQL + `)`

// contestedSQL is a title (aliased t) the Bridge left unmatched because
// another title claimed the same book (AudibleReasonContested).
const contestedSQL = `t.reason = '` + AudibleReasonContested + `'`

// backupAllSQL is a title (aliased t) "Back up everything" queues: available,
// not on this server, not marked "Not this book" or ignored, not contested
// (on this server under another title), and no backup open (a failed one is
// tried again).
const backupAllSQL = `t.state = 'available' AND t.match_path IS NULL AND t.rejected = 0 AND t.ignored = 0
	AND NOT ` + contestedSQL + ` AND NOT ` + openImportSQL

// preorderWaitingSQL is a title (aliased t) that keeps its account checked
// hourly: a preorder released by the UTC date ?1 but not before ?2 (?1 less
// AudiblePreorderWait), not on this server, not marked or ignored.
const preorderWaitingSQL = `t.state = 'preorder' AND t.release_date <> '' AND t.release_date <= ?1 AND t.release_date > ?2
	AND t.match_path IS NULL AND t.rejected = 0 AND t.ignored = 0`

// preorderWindow is today's UTC date at now and the release date a preorder
// waits past, both YYYY-MM-DD.
func preorderWindow(now time.Time) (today, cutoff string) {
	now = now.UTC()
	return now.Format(time.DateOnly), now.Add(-AudiblePreorderWait).Format(time.DateOnly)
}

// preorderOverdue reports whether a title in state with release date release
// (YYYY-MM-DD) is a preorder still not available AudiblePreorderWait after it
// was released, at now.
func preorderOverdue(state, release string, now time.Time) bool {
	if state != AudiblePreorder || release == "" {
		return false
	}
	_, cutoff := preorderWindow(now)
	return release <= cutoff
}

// AudibleTitle returns one title, ErrUnknownTitle when there is none.
func (c *Catalog) AudibleTitle(ctx context.Context, account, asin string) (*AudibleTitle, error) {
	t, err := scanAudibleTitle(c.db.QueryRowContext(ctx, audibleTitleSelect+` WHERE t.account = ? AND t.asin = ?`,
		account, asin), c.now())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnknownTitle
	}
	return t, err
}

// AudibleTitlePatch is the console's change to a title: "Not this book"
// (rejected) and the backup's destination (dest_path, "" for the account's
// placement). A nil field is left as it is.
type AudibleTitlePatch struct {
	Rejected *bool   `json:"rejected"`
	DestPath *string `json:"dest_path"`
}

// PatchAudibleTitle applies the console's change to a title and returns it:
// ErrUnknownTitle for none, a bridge.ErrAudibleDestPath for a destination
// bridge.CheckAudibleDestPath refuses. "Not this book" drops a pending
// request: such a title is never backed up by a policy or "Back up
// everything", and its match is not this title's (a backup may still be asked
// for one by one). A user-state write.
func (c *Catalog) PatchAudibleTitle(ctx context.Context, account, asin string, p AudibleTitlePatch) (*AudibleTitle, error) {
	if p.DestPath != nil {
		if err := bridge.CheckAudibleDestPath(*p.DestPath); err != nil {
			return nil, err
		}
	}
	err := c.db.WithUserStateTx(ctx, "PatchAudibleTitle", func(tx *sql.Tx) error {
		var ok bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM audible_titles WHERE account = ? AND asin = ?)`,
			account, asin).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return ErrUnknownTitle
		}
		now := c.ts()
		if p.Rejected != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE audible_titles SET rejected = ?1,
				    import_pending = CASE WHEN ?1 THEN 0 ELSE import_pending END, updated_at = ?2
				  WHERE account = ?3 AND asin = ?4`, *p.Rejected, now, account, asin); err != nil {
				return err
			}
		}
		if p.DestPath != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE audible_titles SET dest_path = ?, updated_at = ? WHERE account = ? AND asin = ?`,
				*p.DestPath, now, account, asin); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c.AudibleTitle(ctx, account, asin)
}

// AudibleAccountPatch is the console's change to an account: its policy and
// its placement template. A nil field is left as it is.
type AudibleAccountPatch struct {
	Policy   *string `json:"policy"`
	Template *string `json:"template"`
}

// PatchAudibleAccount applies the console's change to account id and returns
// it: ErrUnknownAccount for none, ErrAudiblePolicy for an unknown policy, a
// bridge.ErrAudibleTemplate for a template bridge.CheckAudibleTemplate refuses.
// A policy changed to an automatic one (all, series) starts acting now
// (policy_since): only titles first seen after it are backed up by it.
func (c *Catalog) PatchAudibleAccount(ctx context.Context, id string, p AudibleAccountPatch) (*AudibleAccount, error) {
	if p.Policy != nil && !slices.Contains(AudiblePolicies, *p.Policy) {
		return nil, ErrAudiblePolicy
	}
	if p.Template != nil {
		if err := bridge.CheckAudibleTemplate(*p.Template); err != nil {
			return nil, err
		}
	}
	err := c.db.WithTx(ctx, "PatchAudibleAccount", func(tx *sql.Tx) error {
		var policy string
		err := tx.QueryRowContext(ctx, `SELECT policy FROM audible_accounts WHERE account = ?`, id).Scan(&policy)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknownAccount
		}
		if err != nil {
			return err
		}
		now := c.ts()
		if p.Policy != nil && *p.Policy != policy {
			since := sql.NullString{String: now, Valid: *p.Policy != "manual"}
			if _, err := tx.ExecContext(ctx, `UPDATE audible_accounts SET policy = ?, policy_since = COALESCE(?, policy_since),
				    updated_at = ? WHERE account = ?`, *p.Policy, since, now, id); err != nil {
				return err
			}
		}
		if p.Template != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE audible_accounts SET template = ?, updated_at = ? WHERE account = ?`,
				*p.Template, now, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c.AudibleAccount(ctx, id)
}

// importParams are the params of a backup of asin (with the console's
// destination dest) for acct.
func importParams(acct *AudibleAccount, asin, dest string) bridge.AudibleImportParams {
	return bridge.AudibleImportParams{Account: acct.ID, ASIN: asin, LibraryID: *acct.LibraryID, DestPath: dest,
		Template: acct.Template}
}

// RequestAudibleBackup is the console's "Back up" of one title: the params of
// the job to queue, or, for a preorder, the request stored (pending true, no
// params) to be queued once a poll says it is available. ErrUnknownAccount,
// ErrUnknownTitle; ErrAudiblePaused for a paused account; ErrAudibleMatched
// for a title on this server (unless marked "Not this book");
// ErrAudibleImportOpen while its backup is open (or a request is pending);
// ErrAudibleNotAvailable for an unavailable or gone title.
func (c *Catalog) RequestAudibleBackup(ctx context.Context, account, asin string) (*bridge.AudibleImportParams, bool, error) {
	acct, err := c.AudibleAccount(ctx, account)
	if err != nil {
		return nil, false, err
	}
	t, err := c.AudibleTitle(ctx, account, asin)
	switch {
	case err != nil:
		return nil, false, err
	case acct.Paused:
		return nil, false, ErrAudiblePaused
	case t.Match != nil && !t.Rejected:
		return nil, false, ErrAudibleMatched
	case t.Import != nil && (t.Import.State == AudibleImportPending || bridge.IsOpenState(t.Import.State)):
		return nil, false, ErrAudibleImportOpen
	case t.State == AudiblePreorder:
		_, err := c.db.ExecUserState(ctx, `UPDATE audible_titles SET import_pending = 1, updated_at = ? WHERE account = ? AND asin = ?`,
			c.ts(), account, asin)
		return nil, err == nil, err
	case t.State != AudibleAvailable:
		return nil, false, ErrAudibleNotAvailable
	}
	p := importParams(acct, asin, t.DestPath)
	return &p, false, nil
}

// AudibleBackupAll are the params of the backups "Back up everything" queues
// for account (backupAllSQL), by title. ErrUnknownAccount; ErrAudiblePaused.
func (c *Catalog) AudibleBackupAll(ctx context.Context, account string) ([]bridge.AudibleImportParams, error) {
	acct, err := c.AudibleAccount(ctx, account)
	if err != nil {
		return nil, err
	}
	if acct.Paused {
		return nil, ErrAudiblePaused
	}
	return queryRows(ctx, c.db, func(rows *sql.Rows, p *bridge.AudibleImportParams) error {
		var asin, dest string
		if err := rows.Scan(&asin, &dest); err != nil {
			return err
		}
		*p = importParams(acct, asin, dest)
		return nil
	}, `SELECT t.asin, t.dest_path FROM audible_titles t WHERE t.account = ? AND `+backupAllSQL+`
		ORDER BY t.title COLLATE NOCASE, t.asin`, account)
}

// autoImport is what decides whether a poll backs a title up by itself.
type autoImport struct {
	Policy      string
	PolicySince time.Time
	FirstSeen   time.Time
	State       string
	Matched     bool // on this server
	Rejected    bool // "Not this book"
	Contested   bool // left unmatched: another title claimed the same book
	Ignored     bool
	Pending     bool // a console request waiting for the preorder
	Attempted   bool // a backup was queued before (whatever became of it)
	Open        bool // that backup is still open
	SeriesOwned bool // a book of one of its series is on the server for the person
}

// wants reports whether a poll queues the title's backup: an available title
// not on this server, with no backup open, that a console request waits for,
// or that the account's automatic policy covers: a new purchase (first seen
// after policy_since) never marked "Not this book", ignored, contested or
// tried, under "all", or under "series" when the person already has a book of
// its series.
func (a autoImport) wants() bool {
	if a.State != AudibleAvailable || a.Matched || a.Open || a.Ignored {
		return false
	}
	if a.Pending {
		return true
	}
	if a.Rejected || a.Contested || a.Attempted || !a.FirstSeen.After(a.PolicySince) {
		return false
	}
	switch a.Policy {
	case "all":
		return true
	case "series":
		return a.SeriesOwned
	}
	return false
}

// AudiblePolicyImports are the backups a poll of account queues once its final
// titles call landed (autoImport.wants), by title: the pending console
// requests whose preorder is now available, and the new purchases its policy
// covers. None for a paused account, or one mapped to a person it may no
// longer be (disabled, made a kid). A pending request for a title now on this
// server is dropped.
func (c *Catalog) AudiblePolicyImports(ctx context.Context, account string) ([]bridge.AudibleImportParams, error) {
	acct, err := c.AudibleAccount(ctx, account)
	if err != nil {
		return nil, err
	}
	var (
		eligible bool
		admin    bool
	)
	if err := c.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id = ?1 AND `+bridgeUserSQL+`),
		    EXISTS(SELECT 1 FROM users WHERE id = ?1 AND role = 'admin')`, acct.UserID).Scan(&eligible, &admin); err != nil {
		return nil, err
	}
	if acct.Paused || !eligible {
		return nil, nil
	}
	if _, err := c.db.ExecUserState(ctx, `UPDATE audible_titles SET import_pending = 0
		  WHERE account = ? AND import_pending = 1 AND match_path IS NOT NULL`, account); err != nil {
		return nil, err
	}
	since, err := time.Parse(time.RFC3339Nano, acct.PolicySince)
	if err != nil {
		since = time.Now().Add(time.Hour) // unreadable: no new purchase counts
	}
	type cand struct {
		asin, dest string
		series     []AudibleSeries
		autoImport
	}
	// Every title of the account: autoImport.wants is the one statement of the
	// rule (a poll reads a few hundred rows once).
	cands, err := queryRows(ctx, c.db, func(rows *sql.Rows, x *cand) error {
		var first, series string
		if err := rows.Scan(&x.asin, &x.dest, &x.State, &x.Matched, &first, &series, &x.Rejected, &x.Contested,
			&x.Ignored, &x.Pending, &x.Attempted, &x.Open); err != nil {
			return err
		}
		x.FirstSeen, _ = time.Parse(time.RFC3339Nano, first)
		x.series = decodeSeries(series)
		x.Policy, x.PolicySince = acct.Policy, since
		return nil
	}, `SELECT t.asin, t.dest_path, t.state, t.match_path IS NOT NULL, t.first_seen, t.series, t.rejected, `+contestedSQL+`,
		    t.ignored, t.import_pending, t.import_job_id IS NOT NULL, `+openImportSQL+`
		  FROM audible_titles t WHERE t.account = ?
		 ORDER BY t.title COLLATE NOCASE, t.asin`, account)
	if err != nil {
		return nil, err
	}
	var owned *seriesOwner
	out := []bridge.AudibleImportParams{}
	for _, x := range cands {
		// Ask wants first as if the person had the series, so the series are
		// looked up only for a title that turns on them.
		x.SeriesOwned = true
		if !x.wants() {
			continue
		}
		if acct.Policy == "series" && !x.Pending {
			if owned == nil {
				if owned, err = c.newSeriesOwner(ctx, acct.UserID, admin); err != nil {
					return nil, err
				}
			}
			if x.SeriesOwned, err = owned.any(ctx, x.series); err != nil {
				return nil, err
			}
			if !x.wants() {
				continue
			}
		}
		out = append(out, importParams(acct, x.asin, x.dest))
	}
	return out, nil
}

// decodeSeries reads a title's stored series list ([] when unreadable).
func decodeSeries(raw string) []AudibleSeries {
	var s []AudibleSeries
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil
	}
	return s
}

// seriesOwner answers whether a person has a book of a series on the server:
// in a books library they can see (their scopes), as a book's main series or
// one of its others, by the series' key (any spelling).
type seriesOwner struct {
	c      *Catalog
	scopes []Scope
	seen   map[string]bool
}

func (c *Catalog) newSeriesOwner(ctx context.Context, userID int64, admin bool) (*seriesOwner, error) {
	scopes, err := c.UserScopes(ctx, userID, admin)
	if err != nil {
		return nil, err
	}
	return &seriesOwner{c: c, scopes: scopes, seen: map[string]bool{}}, nil
}

// any reports whether the person has a book of one of series.
func (o *seriesOwner) any(ctx context.Context, series []AudibleSeries) (bool, error) {
	for _, s := range series {
		key := seriesKey(s.Name)
		if key == "" {
			continue
		}
		has, ok := o.seen[key]
		if !ok {
			books, err := o.c.SeriesBooks(ctx, o.scopes, []string{s.Name})
			if err != nil {
				return false, err
			}
			has = len(books) > 0
			o.seen[key] = has
		}
		if has {
			return true, nil
		}
	}
	return false, nil
}

// SetAudibleImport records job as the backup of account's title asin (a
// pending request is now queued). A user-state write.
func (c *Catalog) SetAudibleImport(ctx context.Context, account, asin string, job int64) error {
	_, err := c.db.ExecUserState(ctx, `UPDATE audible_titles SET import_job_id = ?, import_pending = 0, updated_at = ?
		  WHERE account = ? AND asin = ?`, job, c.ts(), account, asin)
	return err
}

// ErrImportNotIndexed is a done backup whose result names no book indexed in
// a books library: its title's match is left as it was.
var ErrImportNotIndexed = errors.New("the backup's book is not indexed")

// FoldAudibleImport takes a done backup's place as its title's match (how
// "import"), and clears its "Not this book" (the match is now this title's
// own). The result must name the job's own library and a book indexed there
// (the Bridge rescans before it reports done), else ErrImportNotIndexed. A job
// that isn't a done audible.import, or whose title has another backup since,
// changes nothing. A user-state write.
func (c *Catalog) FoldAudibleImport(ctx context.Context, j bridge.Job) error {
	if j.Kind != bridge.KindAudibleImport || j.State != bridge.StateDone || j.LibraryID == nil {
		return nil
	}
	p, r, err := bridge.AudibleImportOf(j)
	if err != nil {
		return fmt.Errorf("read backup %d: %w", j.ID, err)
	}
	if r.LibraryID != *j.LibraryID || r.LibraryID != p.LibraryID || !bridgeBookPath(r.Path) {
		return ErrImportNotIndexed
	}
	return c.db.WithUserStateTx(ctx, "FoldAudibleImport", func(tx *sql.Tx) error {
		var indexed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM books b JOIN libraries l ON l.id = b.library_id
			  WHERE b.library_id = ? AND b.rel_path = ? AND l.kind = ?)`, r.LibraryID, r.Path, KindBooks).Scan(&indexed); err != nil {
			return err
		}
		if !indexed {
			return ErrImportNotIndexed
		}
		_, err := tx.ExecContext(ctx, `UPDATE audible_titles SET match_library_id = ?, match_path = ?, match_how = ?,
			    rejected = 0, import_pending = 0, import_job_id = ?, updated_at = ?
			  WHERE account = ? AND asin = ? AND (import_job_id IS NULL OR import_job_id = ?)`,
			r.LibraryID, r.Path, AudibleMatchImport, j.ID, c.ts(), p.Account, p.ASIN, j.ID)
		return err
	})
}
