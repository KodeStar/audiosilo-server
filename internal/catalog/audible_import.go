package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/metadata"
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
	// Conflict is what a backup that failed exists found in the way, as a
	// plan's conflict names it ("file", "book", "audio"; "" for none:
	// importConflict).
	Conflict string `json:"conflict"`
}

// importConflict is what j, an audible.import that failed with code exists,
// found in the way: the result's "conflict" when the Bridge sends one of
// audiblePlanConflicts, else read from the Bridge's message (its
// audiblejob.vacantAt words each): a folder already the library's book, one
// already holding audio, else a file at the destination.
func importConflict(j bridge.Job, code string) string {
	if code != bridge.ImportExists {
		return ""
	}
	var r struct {
		Conflict string `json:"conflict"`
		Message  string `json:"message"`
	}
	_ = json.Unmarshal(j.Result, &r)
	if r.Conflict != "" && slices.Contains(audiblePlanConflicts, r.Conflict) {
		return r.Conflict
	}
	msg := cmp.Or(r.Message, j.Error)
	switch {
	case strings.Contains(msg, "is already the library's book"):
		return "book"
	case strings.Contains(msg, "already holds audio"):
		return "audio"
	}
	return "file"
}

// AudibleEstimate is what "Back up everything" would queue: how many titles and
// about how many bytes.
type AudibleEstimate struct {
	Count int   `json:"count"`
	Bytes int64 `json:"bytes"`
}

// openImportSQL is a title (aliased t) whose backup job is open.
const openImportSQL = `EXISTS(SELECT 1 FROM bridge_jobs j WHERE j.id = t.import_job_id AND j.state IN ` + bridge.OpenStatesSQL + `)`

// retryImportSQL is a title (aliased t) "Back up everything" may back up
// again: no backup yet, none open, and the latest not a done one whose book is
// still indexed (a refused fold, ErrImportNotOwn, leaves the title unmatched
// beside the book its file joined, so it isn't queued again every time; a
// console Back up still does). A failed or cancelled one is tried again, and a
// done one whose book has since left the library (deleted or moved) too, as
// is a done one whose stored result is not JSON (a damaged row, ParamSQL).
var retryImportSQL = `NOT EXISTS(SELECT 1 FROM bridge_jobs j WHERE j.id = t.import_job_id
	AND (j.state IN ` + bridge.OpenStatesSQL + ` OR j.state = '` + bridge.StateDone + `' AND EXISTS(SELECT 1 FROM books b
	     WHERE b.library_id = ` + bridge.ParamSQL("j.result", "$.library_id") + ` AND b.rel_path = ` + bridge.ParamSQL("j.result", "$.path") + `)))`

// contestedSQL is a title (aliased t) the Bridge left unmatched because
// another title claimed the same book (AudibleReasonContested).
const contestedSQL = `t.reason = '` + AudibleReasonContested + `'`

// backupAllSQL is a title (aliased t) "Back up everything" queues: available,
// not on this server, not marked "Not this book" or ignored, not contested
// (on this server under another title), and no backup open or refused
// (retryImportSQL).
var backupAllSQL = `t.state = 'available' AND t.match_path IS NULL AND t.rejected = 0 AND t.ignored = 0
	AND NOT ` + contestedSQL + ` AND ` + retryImportSQL

// backupAllQueuedSQL is a backupAllSQL title (aliased t) "Back up everything"
// queues rather than skips: its stored destination still validates
// (audible_dest_ok, audible.Queues.BackupAll's skip), so the accounts list's
// backup_estimate counts the backups the batch queues.
var backupAllQueuedSQL = backupAllSQL + ` AND audible_dest_ok(t.dest_path)`

// preorderWaitingSQL is a title (aliased t) that keeps its account checked
// hourly: a preorder released by the UTC date ?1 but not before ?2 (?1 less
// AudiblePreorderWait) that a backup may wait for (preorderOverdue's): not on
// this server, not marked or ignored, or a console request for it while it
// isn't here (a match marked "Not this book" isn't).
const preorderWaitingSQL = `t.state = 'preorder' AND t.release_date <> '' AND t.release_date <= ?1 AND t.release_date > ?2
	AND ((t.match_path IS NULL AND t.rejected = 0 AND t.ignored = 0)
	  OR (t.import_pending = 1 AND (t.match_path IS NULL OR t.rejected = 1)))`

// preorderWindow is today's UTC date at now and the release date a preorder
// waits past, both YYYY-MM-DD.
func preorderWindow(now time.Time) (today, cutoff string) {
	now = now.UTC()
	return now.Format(time.DateOnly), now.Add(-AudiblePreorderWait).Format(time.DateOnly)
}

// preorderOverdue reports whether t (pending: a console request waits for it)
// is a preorder a backup may wait for, as preorderWaitingSQL's, still not
// available AudiblePreorderWait after it was released, at now.
func preorderOverdue(t *AudibleTitle, pending bool, now time.Time) bool {
	if t.State != AudiblePreorder || t.ReleaseDate == "" {
		return false
	}
	waited := (t.Match == nil && !t.Rejected && !t.Ignored) || (pending && (t.Match == nil || t.Rejected))
	_, cutoff := preorderWindow(now)
	return waited && t.ReleaseDate <= cutoff
}

// AudibleTitle returns one title, ErrUnknownTitle when there is none.
func (c *Catalog) AudibleTitle(ctx context.Context, account, asin string) (*AudibleTitle, error) {
	t, err := scanAudibleTitle(c.db.QueryRowContext(ctx, audibleTitleSelect+` WHERE t.account = ? AND t.asin = ?`,
		account, asin), c.now())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnknownTitle
	}
	if err != nil {
		return nil, err
	}
	one := []AudibleTitle{*t}
	if err := c.fillContests(ctx, one); err != nil {
		return nil, err
	}
	return &one[0], nil
}

// AudibleTitlePatch is the console's change to a title: "Not this book"
// (rejected), the backup's destination (dest_path, "" for the account's
// placement) and "Don't sync this book" (no_sync, Phase 4c: the sync only,
// never a backup). A nil field is left as it is.
type AudibleTitlePatch struct {
	Rejected *bool   `json:"rejected"`
	DestPath *string `json:"dest_path"`
	NoSync   *bool   `json:"no_sync"`
}

// PatchAudibleTitle applies the console's change to a title and returns it:
// ErrUnknownTitle for none, a bridge.ErrAudibleDestPath for a destination
// bridge.CheckAudibleDestPath refuses or one into a disc folder of an indexed
// book (checkDestOwnFolder). "Not this book" drops a pending
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
		var library int64
		var template string
		err := tx.QueryRowContext(ctx, `SELECT COALESCE(a.library_id, 0), a.template FROM audible_titles t
			JOIN audible_accounts a ON a.account = t.account WHERE t.account = ? AND t.asin = ?`, account, asin).Scan(&library, &template)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknownTitle
		}
		if err != nil {
			return err
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
			if err := checkDestOwnFolder(ctx, tx, library, *p.DestPath, template); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE audible_titles SET dest_path = ?, updated_at = ? WHERE account = ? AND asin = ?`,
				*p.DestPath, now, account, asin); err != nil {
				return err
			}
		}
		if p.NoSync != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE audible_titles SET no_sync = ?, updated_at = ? WHERE account = ? AND asin = ?`,
				*p.NoSync, now, account, asin); err != nil {
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

// The parent of the disc destination is already indexed as a book, or the
// admin explicitly set it to join its discs. The former alone does not prove
// the next scan would join the backup.
var (
	ErrAudibleIndexedBook  = fmt.Errorf("%w: the disc folder is inside an indexed book", bridge.ErrAudibleDestPath)
	ErrAudibleBookOverride = fmt.Errorf("%w: the Audible disc destination conflicts with the one-book folder setting", bridge.ErrAudibleDestPath)
)

// audibleDiscParent is the folder that could absorb the backup's disc, or ""
// when there is none. The Bridge renders templates. We can check a literal
// disc folder even if the file name uses a title variable. For a folder that
// depends on a variable, importOwnBook checks the actual placement.
func audibleDiscParent(dest, template string) string {
	folder := strings.TrimSpace(dest)
	fromTemplate := folder == ""
	var templateFile bool
	if fromTemplate {
		folder = strings.TrimSpace(template)
		name := path.Base(folder)
		templateFile = name == "{OriginalFilename}" || (name != "{Ext}" && strings.HasSuffix(name, "{Ext}"))
	}
	if templateFile || metadata.IsAudio(folder) {
		folder = path.Dir(folder)
	}
	if !metadata.IsDiscFolder(path.Base(folder)) {
		return ""
	}
	parent := path.Dir(folder)
	if parent == "." || (fromTemplate && strings.ContainsAny(parent, "{}\\")) {
		return ""
	}
	return parent
}

func checkDestOwnFolder(ctx context.Context, tx *sql.Tx, library int64, dest, template string) error {
	book := audibleDiscParent(dest, template)
	if book == "" || library == 0 {
		return nil
	}
	var override, indexed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM folder_overrides WHERE library_id = ?1 AND path = ?2 AND mode = ?3),
		EXISTS(SELECT 1 FROM books WHERE library_id = ?1 AND rel_path = ?2)`, library, book, OverrideBook).Scan(&override, &indexed)
	switch {
	case err != nil:
		return err
	case override:
		return discConflict(ErrAudibleBookOverride, book)
	case indexed:
		return discConflict(ErrAudibleIndexedBook, book)
	}
	return nil
}

// discConflict is the refusal naming the folder that would absorb the disc.
func discConflict(sentinel error, book string) error {
	return fmt.Errorf("%w (%s). Choose a separate folder for the backup", sentinel, book)
}

// checkAudibleRemap checks the saved destinations in the library the Bridge
// proposes for this account, including when a paused account resumes.
func checkAudibleRemap(ctx context.Context, tx *sql.Tx, account string, library int64, template string) error {
	if err := checkDestOwnFolder(ctx, tx, library, "", template); err != nil {
		return err
	}
	if library == 0 {
		return nil
	}
	// checkDestOwnFolder's rule over every saved destination in one statement.
	var book string
	var override bool
	err := tx.QueryRowContext(ctx, `SELECT p, EXISTS(SELECT 1 FROM folder_overrides WHERE library_id = ?2 AND path = p AND mode = ?3) AS o
		FROM (SELECT DISTINCT audible_disc_parent(dest_path, '') AS p FROM audible_titles WHERE account = ?1 AND dest_path <> '')
		WHERE p <> '' AND (o OR EXISTS(SELECT 1 FROM books WHERE library_id = ?2 AND rel_path = p))
		ORDER BY o DESC LIMIT 1`, account, library, OverrideBook).Scan(&book, &override)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	case override:
		return discConflict(ErrAudibleBookOverride, book)
	}
	return discConflict(ErrAudibleIndexedBook, book)
}

// checkAudibleOverride checks the other order of edits. Open backups keep
// their original placement if the account is remapped or the title is edited.
func checkAudibleOverride(ctx context.Context, tx *sql.Tx, library int64, book string) error {
	lo, hi := bridge.AudibleImportKeyRange(library)
	var conflict bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM audible_accounts WHERE library_id = ?1 AND template <> '' AND audible_disc_parent('', template) = ?2
		UNION ALL SELECT 1 FROM audible_accounts a JOIN audible_titles t ON t.account = a.account
			WHERE a.library_id = ?1 AND t.dest_path <> '' AND audible_disc_parent(t.dest_path, '') = ?2
		UNION ALL SELECT 1 FROM bridge_jobs j INDEXED BY idx_bridge_jobs_open WHERE j.dedupe_key >= ?3 AND j.dedupe_key < ?4
			AND j.state IN `+bridge.OpenStatesSQL+`
			AND audible_disc_parent(`+bridge.ParamSQL("j.params", "$.dest_path")+`, `+bridge.ParamSQL("j.params", "$.template")+`) = ?2)`,
		library, book, lo, hi).Scan(&conflict)
	if err == nil && conflict {
		return discConflict(ErrAudibleBookOverride, book)
	}
	return err
}

// AudibleAccountPatch is the console's change to an account: its policy, its
// placement template and whether its places sync with Audible (Phase 4c). A
// nil field is left as it is.
type AudibleAccountPatch struct {
	Policy   *string `json:"policy"`
	Template *string `json:"template"`
	Sync     *bool   `json:"sync"`
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
		var library int64
		err := tx.QueryRowContext(ctx, `SELECT policy, COALESCE(library_id, 0) FROM audible_accounts WHERE account = ?`, id).Scan(&policy, &library)
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
			if err := checkDestOwnFolder(ctx, tx, library, "", *p.Template); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE audible_accounts SET template = ?, updated_at = ? WHERE account = ?`,
				*p.Template, now, id); err != nil {
				return err
			}
		}
		// Turned on, the account's first sync is due at the scheduler's next
		// pass; turned off, its open sync is cancelled (a running one posts
		// nothing more: the listening route answers sync_off).
		if p.Sync != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE audible_accounts SET sync = ?1, sync_queued_at = NULL, updated_at = ?2
				  WHERE account = ?3 AND sync <> ?1`, *p.Sync, now, id); err != nil {
				return err
			}
			if !*p.Sync {
				if err := bridge.CancelAudibleSyncs(ctx, tx, id, bridge.CancelAudibleSyncOff, c.now()); err != nil {
					return err
				}
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
// ErrAudibleNotAvailable for an unavailable or gone title; a
// bridge.ErrAudibleDestPath or ErrAudibleTemplate for a stored destination or
// template that no longer validates (saved before a rule tightened), a
// preorder's too: its request would never be queued.
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
	}
	if err := t.backupRefusal(); err != nil {
		return nil, false, err
	}
	if err := bridge.CheckAudibleTemplate(acct.Template); err != nil {
		return nil, false, err
	}
	if t.State == AudiblePreorder {
		_, err := c.db.ExecUserState(ctx, `UPDATE audible_titles SET import_pending = 1, updated_at = ? WHERE account = ? AND asin = ?`,
			c.ts(), account, asin)
		return nil, err == nil, err
	}
	p := importParams(acct, asin, t.DestPath)
	return &p, false, nil
}

// backupRefusal is why the console's Back up refuses t whatever its account:
// ErrAudibleMatched for a title on this server (unless marked "Not this
// book"), ErrAudibleImportOpen while its backup is open (or a request is
// pending), ErrAudibleNotAvailable unless it is available or a preorder,
// bridge.ErrAudibleDestPath for a stored destination that no longer validates
// (AudibleTitle.DestInvalid); nil when it may be backed up
// (AudibleTitle.CanBackup).
func (t *AudibleTitle) backupRefusal() error {
	switch {
	case t.Match != nil && !t.Rejected:
		return ErrAudibleMatched
	case t.Import != nil && (t.Import.State == AudibleImportPending || bridge.IsOpenState(t.Import.State)):
		return ErrAudibleImportOpen
	case t.State != AudibleAvailable && t.State != AudiblePreorder:
		return ErrAudibleNotAvailable
	}
	return bridge.CheckAudibleDestPath(t.DestPath)
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
	Matched     bool // on this server: a match not marked "Not this book"
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

// AudiblePolicyImport is a backup a poll queues: its params, and whether a
// console request waited for it (Requested: a preorder's Back up, now
// available) rather than the account's policy asking.
type AudiblePolicyImport struct {
	bridge.AudibleImportParams
	Requested bool
}

// AudiblePolicyImports are the backups a poll of account queues once its final
// titles call landed (autoImport.wants), by title: the pending console
// requests whose preorder is now available, and the new purchases its policy
// covers. None for a paused account, or one mapped to a person it may no
// longer be (disabled, made a kid). A pending request for a title now on this
// server (a match not marked "Not this book") is dropped; a title whose stored
// destination no longer validates is left out (its request kept).
func (c *Catalog) AudiblePolicyImports(ctx context.Context, account string) ([]AudiblePolicyImport, error) {
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
		  WHERE account = ? AND import_pending = 1 AND match_path IS NOT NULL AND rejected = 0`, account); err != nil {
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
	}, `SELECT t.asin, t.dest_path, t.state, t.match_path IS NOT NULL AND t.rejected = 0, t.first_seen, t.series, t.rejected, `+contestedSQL+`,
		    t.ignored, t.import_pending, t.import_job_id IS NOT NULL, `+openImportSQL+`
		  FROM audible_titles t WHERE t.account = ?
		 ORDER BY t.title COLLATE NOCASE, t.asin`, account)
	if err != nil {
		return nil, err
	}
	var owned *seriesOwner
	out := []AudiblePolicyImport{}
	for _, x := range cands {
		// A destination stored before a rule tightened (AudibleTitle.DestInvalid,
		// as Back up everything skips it): the queue would refuse it at every
		// poll, so it waits, a pending request with it, until the console sets
		// it again.
		if bridge.CheckAudibleDestPath(x.dest) != nil {
			continue
		}
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
		out = append(out, AudiblePolicyImport{AudibleImportParams: importParams(acct, x.asin, x.dest), Requested: x.Pending})
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

// SetAudibleImportTx records job as the backup of account's title asin (a
// pending request is now queued), in tx: the transaction that queued the job
// (bridge.EnqueueAll), so a job and its title's record commit together. An
// earlier backup's refusal (fold_refusal_reason, 0093) goes: the title's
// status is this backup's now, so a stored refusal is always its import
// job's.
func (c *Catalog) SetAudibleImportTx(ctx context.Context, tx *sql.Tx, account, asin string, job int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE audible_titles SET import_job_id = ?, import_pending = 0,
		    fold_refusal_reason = '', fold_refused_at = NULL, updated_at = ?
		  WHERE account = ? AND asin = ?`, job, c.ts(), account, asin)
	return err
}

var (
	// ErrImportNotIndexed is a done backup whose result names no book indexed
	// in a books library: its title's match is left as it was.
	ErrImportNotIndexed = errors.New("the backup's book is not indexed")
	// ErrImportNotOwn is a done backup whose book isn't its title's own: the
	// placed file joined a book the library already had (other files, not the
	// title's ASIN), or the book is the one the admin marked "Not this book"
	// for the title. Its title's match, and every other title's, are left as
	// they were.
	ErrImportNotOwn = errors.New("the backup's book is not the title's own")
)

// The reasons a done backup could not become its title's match, stored on the
// title (fold_refusal_reason, 0093) and worded by the console.
const (
	// AudibleFoldInvalidResult: the result is unreadable, names another
	// library or no usable book path.
	AudibleFoldInvalidResult = "invalid_result"
	// AudibleFoldNotIndexed: no book is indexed at the result's path.
	AudibleFoldNotIndexed = "not_indexed"
	// AudibleFoldNotOwnBook: the book there isn't the title's own.
	AudibleFoldNotOwnBook = "not_own_book"
	// AudibleFoldRejectedMatch: the book there is the one marked Not this book.
	AudibleFoldRejectedMatch = "rejected_match"
)

// foldRefusal is a done backup the fold refused: the reason it stores on the
// title, and the error (ErrImport*, or the result's read error) it returns.
type foldRefusal struct {
	reason string
	err    error
}

func (e *foldRefusal) Error() string { return e.err.Error() }
func (e *foldRefusal) Unwrap() error { return e.err }

// AudibleFoldRefusal is the reason (one of AudibleFold*) of a
// FoldAudibleImport error that is a refusal, "" for one that is not (the fold
// could not run).
func AudibleFoldRefusal(err error) string {
	if r, ok := errors.AsType[*foldRefusal](err); ok {
		return r.reason
	}
	return ""
}

// FoldAudibleImport takes a done backup's place as its title's match (how
// "import"), and clears its "Not this book" (the match is now this title's
// own), what the last poll said of it unmatched (its planned destination
// and contested book) and an earlier backup's refusal. The result must name the job's own library and a book indexed there
// (the Bridge rescans before it reports done), else ErrImportNotIndexed, and
// the title's own book (importOwnBook), else ErrImportNotOwn. A refusal is
// recorded on the title (fold_refusal_reason, fold_refused_at) in the same
// transaction, then returned. Unreadable params name no title and change
// nothing. A job that isn't a done audible.import, or whose title has another
// backup since, changes nothing. A user-state write.
func (c *Catalog) FoldAudibleImport(ctx context.Context, j bridge.Job) error {
	if j.Kind != bridge.KindAudibleImport || j.State != bridge.StateDone || j.LibraryID == nil {
		return nil
	}
	var (
		p       bridge.AudibleImportParams
		r       bridge.AudibleImportResult
		refused *foldRefusal
	)
	if err := json.Unmarshal(j.Params, &p); err != nil {
		return fmt.Errorf("read backup %d: %w", j.ID, err)
	}
	if len(j.Result) > 0 {
		if err := json.Unmarshal(j.Result, &r); err != nil {
			refused = &foldRefusal{AudibleFoldInvalidResult, fmt.Errorf("read backup %d: %w", j.ID, err)}
		}
	}
	if refused == nil && (r.LibraryID != *j.LibraryID || r.LibraryID != p.LibraryID || !bridgeBookPath(r.Path)) {
		refused = &foldRefusal{AudibleFoldInvalidResult, ErrImportNotIndexed}
	}
	err := c.db.WithUserStateTx(ctx, "FoldAudibleImport", func(tx *sql.Tx) error {
		if refused == nil {
			if err := importOwnBook(ctx, tx, p, r); !errors.As(err, &refused) && err != nil {
				return err
			}
		}
		if refused != nil {
			_, err := tx.ExecContext(ctx, `UPDATE audible_titles SET fold_refusal_reason = ?1, fold_refused_at = ?2,
				    updated_at = ?2, import_job_id = ?3
				  WHERE account = ?4 AND asin = ?5 AND (import_job_id IS NULL OR import_job_id = ?3)`,
				refused.reason, c.ts(), j.ID, p.Account, p.ASIN)
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE audible_titles SET match_library_id = ?, match_path = ?, match_how = ?,
			    rejected = 0, import_pending = 0, import_job_id = ?, planned = '',
			    contest_library_id = NULL, contest_path = '', fold_refusal_reason = '', fold_refused_at = NULL, updated_at = ?
			  WHERE account = ? AND asin = ? AND (import_job_id IS NULL OR import_job_id = ?)`,
			r.LibraryID, r.Path, AudibleMatchImport, j.ID, c.ts(), p.Account, p.ASIN, j.ID)
		return err
	})
	if err != nil || refused == nil {
		return err
	}
	return refused
}

// importOwnBook checks, through tx, that the book a done backup's result names
// is indexed in a books library (else ErrImportNotIndexed) and is the title's
// own (else ErrImportNotOwn): it carries the title's ASIN (the Bridge's
// enrichment set it, or the book had it), or it holds only the files the
// backup placed. A joined disc set must pass the file check even with the ASIN. A file the Bridge placed into a
// folder book already there (the scan makes a folder one book) joins that
// book, which stays its own title's. Never the book the admin marked "Not
// this book" for the title. A refusal is a *foldRefusal.
func importOwnBook(ctx context.Context, tx *sql.Tx, p bridge.AudibleImportParams, r bridge.AudibleImportResult) error {
	var (
		id       int64
		folder   bool
		asin     string
		rejected bool
	)
	err := tx.QueryRowContext(ctx, `SELECT b.id, b.is_folder, b.asin,
		    EXISTS(SELECT 1 FROM audible_titles t WHERE t.account = ? AND t.asin = ? AND t.rejected = 1
		       AND t.match_library_id = b.library_id AND t.match_path = b.rel_path)
		  FROM books b JOIN libraries l ON l.id = b.library_id
		 WHERE b.library_id = ? AND b.rel_path = ? AND l.kind = ?`,
		p.Account, p.ASIN, r.LibraryID, r.Path, KindBooks).Scan(&id, &folder, &asin, &rejected)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return &foldRefusal{AudibleFoldNotIndexed, ErrImportNotIndexed}
	case err != nil:
		return err
	case rejected:
		return &foldRefusal{AudibleFoldRejectedMatch, ErrImportNotOwn}
	}
	// A newly joined disc set can carry the ASIN the Bridge just wrote.
	// In that case require the indexed files to belong to this backup.
	joinedDisc := folder && audibleDiscParent(p.DestPath, p.Template) == r.Path
	placed := make(map[string]bool, len(r.Files))
	for _, f := range r.Files {
		placed[f.Path] = true
		if folder && audibleDiscParent(f.Path, "") == r.Path {
			joinedDisc = true
		}
	}
	if strings.EqualFold(asin, p.ASIN) && !joinedDisc {
		return nil
	}
	files := []string{r.Path} // a single-file book is its file
	if folder {
		if files, err = queryRows(ctx, tx, func(rows *sql.Rows, path *string) error { return rows.Scan(path) },
			`SELECT rel_path FROM book_files WHERE book_id = ?`, id); err != nil {
			return err
		}
	}
	if len(files) == 0 {
		return &foldRefusal{AudibleFoldNotOwnBook, ErrImportNotOwn}
	}
	for _, f := range files {
		if !placed[f] {
			return &foldRefusal{AudibleFoldNotOwnBook, ErrImportNotOwn}
		}
	}
	return nil
}
