package catalog

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
)

// Bridge Phase 4a: Audible accounts (migration 0063). The Bridge signs in to an
// Audible account (the credentials stay in its vault), maps it to a server user
// and a books library, polls the account's library and reports its titles,
// each matched (or not) to a book on this server. The server keeps the mapping
// and the titles for the console and queues the polls; it never talks to
// Audible and never writes a library.

// AudibleMarketplaces are the Audible stores an account may belong to: the
// community metadata's region vocabulary (config.Regions; a test holds the two
// together).
var AudibleMarketplaces = []string{"us", "uk", "ca", "au", "de", "fr", "es", "it", "jp", "in", "br"}

// AudiblePolicies are what an account backs up after each poll (Phase 4b):
// every new title, titles in a series the user already has, or only what an
// admin approves.
var AudiblePolicies = []string{"all", "series", "manual"}

// Audible title states. A poll reports the first three; gone is the server's,
// for a title absent from the account's last complete poll.
const (
	AudibleAvailable   = "available"
	AudiblePreorder    = "preorder"
	AudibleUnavailable = "unavailable"
	AudibleGone        = "gone"
)

// AudibleReasonContested is the reason a Bridge stores a title unmatched with
// when another title of the account claimed the same book (a different work
// tied for it). The book is on this server under the other title's match, so
// neither "Back up everything" nor a policy backs it up; a console Back up of
// the one title still may (the admin decides).
const AudibleReasonContested = "contested"

// AudibleStates are the states a title may be in, as the console filters them.
var AudibleStates = []string{AudibleAvailable, AudiblePreorder, AudibleUnavailable, AudibleGone}

const (
	// MaxAudibleTitles bounds one titles call; a poll sends more in several.
	MaxAudibleTitles = 500
	// maxBridgeBooks is a page of BridgeBooks, the default and the bound.
	maxBridgeBooks = 500
	// MaxAudibleTitlesPage and DefaultAudibleTitlesPage bound the console's
	// titles list.
	MaxAudibleTitlesPage     = 200
	DefaultAudibleTitlesPage = 50

	maxAudibleLabel     = 100
	maxAudibleTitle     = 500
	maxAudibleName      = 200
	maxAudibleNames     = 20
	maxAudibleSeries    = 10
	maxAudiblePosition  = 32
	maxAudibleReason    = 256
	maxAudibleRuntime   = 1_000_000 // seconds, over eleven days
	maxAudibleQueryText = 200
	maxAudibleSiblings  = 100_000
	maxAudibleContest   = 10 // a contest's other titles listed
)

var (
	// ErrAudibleInvalid is a malformed account or titles request (a 400).
	ErrAudibleInvalid = errors.New("invalid audible request")
	// ErrAudiblePolicy and ErrAudibleMarketplace are an account mapping the
	// server refuses (400 invalid_policy, invalid_marketplace; a podcast
	// library is ErrNotBookLibrary, 400 not_books).
	ErrAudiblePolicy      = errors.New("policy must be all, series or manual")
	ErrAudibleMarketplace = errors.New("unknown marketplace")
	// ErrAudibleUnknownUser is a user id that is not an enabled adult or admin (a
	// kid, a demo or a disabled account counts as unknown): a 404.
	ErrAudibleUnknownUser = errors.New("unknown user")
	// ErrUnknownLibrary is a library id with no library: a 404.
	ErrUnknownLibrary = errors.New("unknown library")
	// ErrUnknownAccount is an account id the Bridge never mapped: a 404.
	ErrUnknownAccount = errors.New("unknown audible account")
	// ErrASINSet is an ASIN backfill for a book that already has an ASIN, or
	// whose ASIN an admin edited (cleared included): a 409.
	ErrASINSet = errors.New("the book already has an asin")
)

func audibleInvalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrAudibleInvalid, fmt.Sprintf(format, args...))
}

var audiblePollPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidASIN reports whether s is an ASIN as Audible spells one (ten upper-case
// letters and digits).
func ValidASIN(s string) bool { return asinRE.MatchString(s) }

// bridgeUserSQL is who an Audible account may be mapped to: an enabled adult
// or admin, never a kid's account, a demo account or a disabled one.
const bridgeUserSQL = `disabled = 0 AND is_demo = 0 AND (member_kind IS NULL OR member_kind <> 'kid')`

// BridgeUser is one person the Bridge may map an Audible account to (GET
// /bridge/users): no email or anything else about them.
type BridgeUser struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Admin       bool   `json:"admin"`
}

// displayNameSQL is a person's name to show: the one an owner typed, else the
// username (users aliased u).
const displayNameSQL = `COALESCE(NULLIF(u.display_name, ''), u.username)`

// BridgeUsers lists the enabled adults and admins, by username.
func (c *Catalog) BridgeUsers(ctx context.Context) ([]BridgeUser, error) {
	return queryRows(ctx, c.db, func(rows *sql.Rows, u *BridgeUser) error {
		return rows.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Admin)
	}, `SELECT u.id, u.username, `+displayNameSQL+`, u.role = 'admin' FROM users u WHERE `+bridgeUserSQL+`
		ORDER BY u.username COLLATE NOCASE, u.id`)
}

// AudibleUser is the person an account is mapped to.
type AudibleUser struct {
	ID          int64  `json:"id"`
	DisplayName string `json:"display_name"`
}

// AudibleLibrary is the library an account is mapped to.
type AudibleLibrary struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// AudibleCounts are an account's titles by state, and how many are matched to
// a book on this server (a match marked "Not this book" isn't).
type AudibleCounts struct {
	Total       int `json:"total"`
	Available   int `json:"available"`
	Preorder    int `json:"preorder"`
	Unavailable int `json:"unavailable"`
	Gone        int `json:"gone"`
	Matched     int `json:"matched"`
}

// AudibleAccount is one Audible account the Bridge signed in to, as the server
// keeps it: its mapping, never a credential.
type AudibleAccount struct {
	ID          string      `json:"id"`
	Label       string      `json:"label"`
	Marketplace string      `json:"marketplace"`
	UserID      int64       `json:"user_id"`
	User        AudibleUser `json:"user"`
	// LibraryID is nil for a paused account: its library was deleted.
	LibraryID    *int64          `json:"library_id"`
	Library      *AudibleLibrary `json:"library"`
	Paused       bool            `json:"paused"`
	Policy       string          `json:"policy"`
	Sync         bool            `json:"sync"`
	PollHours    int             `json:"poll_hours"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
	LastPollAt   *string         `json:"last_poll_at"`
	LastQueuedAt *string         `json:"last_queued_at"`
	// Template is the account's placement template for its backups ("" = the
	// Bridge's automatic placement), PolicySince when its automatic policy
	// began to act (Phase 4b: only titles first seen after it are backed up by
	// the policy).
	Template    string `json:"template"`
	PolicySince string `json:"policy_since"`
	// LastSyncAt is when the account's last audible.sync finished, RefusedAt
	// when one failed because Audible refused the account (Phase 4c: no sync
	// is queued until the Bridge maps it again); nil for none. Sync (above) is
	// the console's toggle, off for a new account.
	LastSyncAt *string `json:"last_sync_at"`
	RefusedAt  *string `json:"refused_at"`
	// Titles are the account's titles counted, BackupEstimate what "Back up
	// everything" would queue (the console's list only).
	Titles         *AudibleCounts   `json:"titles,omitempty"`
	BackupEstimate *AudibleEstimate `json:"backup_estimate,omitempty"`
}

// AudibleAccountInput is the Bridge's mapping of an account (PUT
// /bridge/audible/accounts/{account}).
type AudibleAccountInput struct {
	Label       string `json:"label"`
	Marketplace string `json:"marketplace"`
	UserID      int64  `json:"user_id"`
	LibraryID   int64  `json:"library_id"`
	Policy      string `json:"policy"`
	// Sync is read and ignored (Phase 4c): the console owns the account's sync
	// toggle, off for a new account.
	Sync bool `json:"sync"`
}

const audibleAccountSelect = `SELECT a.account, a.label, a.marketplace, a.user_id, ` + displayNameSQL + `,
	a.library_id, l.name, a.policy, a.sync, a.poll_hours, a.created_at, a.updated_at, a.last_poll_at, a.last_queued_at,
	a.template, a.policy_since, a.last_sync_at, a.refused_at
	FROM audible_accounts a JOIN users u ON u.id = a.user_id LEFT JOIN libraries l ON l.id = a.library_id`

func scanAudibleAccount(row rowScanner) (*AudibleAccount, error) {
	var (
		a                    AudibleAccount
		lib                  sql.NullInt64
		libName, poll, queue sql.NullString
		synced, refused      sql.NullString
	)
	if err := row.Scan(&a.ID, &a.Label, &a.Marketplace, &a.UserID, &a.User.DisplayName, &lib, &libName,
		&a.Policy, &a.Sync, &a.PollHours, &a.CreatedAt, &a.UpdatedAt, &poll, &queue, &a.Template, &a.PolicySince,
		&synced, &refused); err != nil {
		return nil, err
	}
	if synced.Valid {
		a.LastSyncAt = &synced.String
	}
	if refused.Valid {
		a.RefusedAt = &refused.String
	}
	a.User.ID = a.UserID
	if lib.Valid {
		a.LibraryID = &lib.Int64
		a.Library = &AudibleLibrary{ID: lib.Int64, Name: libName.String}
	}
	a.Paused = a.LibraryID == nil
	if poll.Valid {
		a.LastPollAt = &poll.String
	}
	if queue.Valid {
		a.LastQueuedAt = &queue.String
	}
	return &a, nil
}

type rowScanner interface{ Scan(...any) error }

// checkAudibleAccount checks and normalises an account mapping's own fields.
func checkAudibleAccount(in *AudibleAccountInput) error {
	in.Label = bridge.CleanText(in.Label, maxAudibleLabel)
	in.Marketplace = strings.ToLower(strings.TrimSpace(in.Marketplace))
	switch {
	case !slices.Contains(AudiblePolicies, in.Policy):
		return ErrAudiblePolicy
	case !slices.Contains(AudibleMarketplaces, in.Marketplace):
		return ErrAudibleMarketplace
	case in.UserID <= 0:
		return ErrAudibleUnknownUser
	case in.LibraryID <= 0:
		return ErrUnknownLibrary
	}
	return nil
}

// PutAudibleAccount records the Bridge's mapping of account id (Bridge-made,
// bridge.AudibleAccountPattern): the user must be an enabled adult or admin
// (ErrAudibleUnknownUser) and the library a books library (ErrUnknownLibrary,
// ErrNotBookLibrary). A known account is mapped again (a paused one resumes); its
// titles and poll times stay, and so do its policy and template (Phase 4b: the
// Bridge's policy is a new account's; after that the console sets it,
// PatchAudibleAccount). A new account's policy_since is now. Its sync is the
// console's, off for a new account whatever the Bridge sends (Phase 4c), never
// turned on by a PUT; mapped to another person it is turned off (the console
// turned it on for the person it showed then), its last sync forgotten and its
// open sync cancelled. A mapping again ends a refusal (refused_at: a fresh
// sign-in maps it again).
func (c *Catalog) PutAudibleAccount(ctx context.Context, id string, in AudibleAccountInput) (*AudibleAccount, error) {
	if !bridge.AudibleAccountPattern.MatchString(id) {
		return nil, audibleInvalid("account must match %s", bridge.AudibleAccountPattern)
	}
	if err := checkAudibleAccount(&in); err != nil {
		return nil, err
	}
	var out *AudibleAccount
	err := c.db.WithTx(ctx, "PutAudibleAccount", func(tx *sql.Tx) error {
		var ok bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id = ? AND `+bridgeUserSQL+`)`,
			in.UserID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return ErrAudibleUnknownUser
		}
		var kind string
		err := tx.QueryRowContext(ctx, `SELECT kind FROM libraries WHERE id = ?`, in.LibraryID).Scan(&kind)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknownLibrary
		}
		if err != nil {
			return err
		}
		if kind != KindBooks {
			return ErrNotBookLibrary
		}
		var was int64
		err = tx.QueryRowContext(ctx, `SELECT user_id FROM audible_accounts WHERE account = ?`, id).Scan(&was)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		remapped := err == nil && was != in.UserID
		now := c.ts()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO audible_accounts(account, label, marketplace, user_id, library_id, policy, sync, created_at, updated_at,
			     policy_since)
			 VALUES(?, ?, ?, ?, ?, ?, 0, ?, ?, ?)
			 ON CONFLICT(account) DO UPDATE SET label = excluded.label, marketplace = excluded.marketplace,
			     user_id = excluded.user_id, library_id = excluded.library_id,
			     refused_at = NULL, updated_at = excluded.updated_at`,
			id, in.Label, in.Marketplace, in.UserID, in.LibraryID, in.Policy, now, now, now); err != nil {
			return err
		}
		if remapped {
			if _, err := tx.ExecContext(ctx, `UPDATE audible_accounts SET sync = 0, sync_queued_at = NULL, last_sync_at = NULL
				  WHERE account = ?`, id); err != nil {
				return err
			}
			if err := bridge.CancelAudibleSyncs(ctx, tx, id, bridge.CancelAudibleSyncOff, c.now()); err != nil {
				return err
			}
		}
		out, err = scanAudibleAccount(tx.QueryRowContext(ctx, audibleAccountSelect+` WHERE a.account = ?`, id))
		return err
	})
	return out, err
}

// DeleteAudibleAccount forgets account id and its titles (the Bridge, or the
// console, removed it), cancelling its open poll and backups. Forgetting an unknown
// account is not an error: the Bridge forgets locally first and may call
// again. removed says whether there was one.
func (c *Catalog) DeleteAudibleAccount(ctx context.Context, id string) (removed bool, err error) {
	err = c.db.WithTx(ctx, "DeleteAudibleAccount", func(tx *sql.Tx) error {
		if err := bridge.CancelAudiblePolls(ctx, tx, id, bridge.CancelAudibleRemoved, c.now()); err != nil {
			return err
		}
		if err := bridge.CancelAudibleImports(ctx, tx, id, bridge.CancelAudibleRemoved, c.now()); err != nil {
			return err
		}
		if err := bridge.CancelAudibleSyncs(ctx, tx, id, bridge.CancelAudibleRemoved, c.now()); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM audible_accounts WHERE account = ?`, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		removed = n > 0
		return err
	})
	return removed, err
}

// AudibleAccount returns account id, ErrUnknownAccount when there is none.
func (c *Catalog) AudibleAccount(ctx context.Context, id string) (*AudibleAccount, error) {
	a, err := scanAudibleAccount(c.db.QueryRowContext(ctx, audibleAccountSelect+` WHERE a.account = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnknownAccount
	}
	return a, err
}

// AudibleAccounts lists every account, by label then id, each with its titles
// counted.
func (c *Catalog) AudibleAccounts(ctx context.Context) ([]AudibleAccount, error) {
	rows, err := c.db.QueryContext(ctx, audibleAccountSelect+` ORDER BY a.label COLLATE NOCASE, a.account`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []AudibleAccount{}
	byID := map[string]*AudibleAccount{} // its Titles and BackupEstimate are shared with out's copy
	for rows.Next() {
		a, err := scanAudibleAccount(rows)
		if err != nil {
			return nil, err
		}
		a.Titles, a.BackupEstimate = &AudibleCounts{}, &AudibleEstimate{}
		byID[a.ID] = a
		out = append(out, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = rows.Close()
	counts, err := c.db.QueryContext(ctx, `SELECT t.account, COUNT(*),
		    SUM(t.state = 'available'), SUM(t.state = 'preorder'), SUM(t.state = 'unavailable'), SUM(t.state = 'gone'),
		    SUM(t.match_path IS NOT NULL AND t.rejected = 0),
		    SUM(CASE WHEN `+backupAllSQL+` THEN 1 ELSE 0 END), SUM(CASE WHEN `+backupAllSQL+` THEN COALESCE(t.runtime_s, 0) ELSE 0 END)
		  FROM audible_titles t GROUP BY t.account`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = counts.Close() }()
	for counts.Next() {
		var (
			id      string
			n       AudibleCounts
			e       AudibleEstimate
			seconds int64
		)
		if err := counts.Scan(&id, &n.Total, &n.Available, &n.Preorder, &n.Unavailable, &n.Gone, &n.Matched,
			&e.Count, &seconds); err != nil {
			return nil, err
		}
		e.Bytes = seconds * audibleBytesPerSecond
		if a := byID[id]; a != nil {
			*a.Titles, *a.BackupEstimate = n, e
		}
	}
	if err := counts.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// AudibleSeries is a title's place in one series, as Audible names it.
type AudibleSeries struct {
	Name     string `json:"name"`
	Position string `json:"position"`
}

// AudibleMatch is the book on this server the Bridge matched a title to: by
// its ASIN, or by a fuzzy match of its title and authors.
type AudibleMatch struct {
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"`
	How       string `json:"how"`
}

// AudibleTitleInput is one title of a poll, as the Bridge reports it.
type AudibleTitleInput struct {
	ASIN        string          `json:"asin"`
	Title       string          `json:"title"`
	Authors     []string        `json:"authors"`
	Series      []AudibleSeries `json:"series"`
	ReleaseDate string          `json:"release_date"`
	RuntimeS    int64           `json:"runtime_s"`
	State       string          `json:"state"`
	Reason      string          `json:"reason"`
	Match       *AudibleMatch   `json:"match"`
	// Planned is where a backup of the unmatched title would go, as the
	// Bridge planned it during this poll (nil for none; dropped with a match).
	Planned *AudiblePlan `json:"planned"`
	// ContestedPath is the book in the account's library another title
	// claimed as well, for a title left unmatched as contested ("" for none;
	// dropped with a match). Input only: the console reads Contested.
	ContestedPath string `json:"contested_path,omitempty"`
}

// The kinds of place a planned backup goes to: beside the books of its series
// already in the library, in a new series folder, or as a book of its own.
const (
	AudiblePlanSeriesExisting = "series_existing"
	AudiblePlanSeriesNew      = "series_new"
	AudiblePlanStandalone     = "standalone"
)

// The states of a planned destination: a new place, one that joins an author
// or series folder already there, one where a book or file already is (the
// backup would be refused), or a plan that failed (Conflict says why).
const (
	AudiblePlanNew    = "new"
	AudiblePlanJoins  = "joins"
	AudiblePlanExists = "exists"
	AudiblePlanError  = "error"
)

var (
	audiblePlanKinds  = []string{AudiblePlanSeriesExisting, AudiblePlanSeriesNew, AudiblePlanStandalone}
	audiblePlanStates = []string{AudiblePlanNew, AudiblePlanJoins, AudiblePlanExists, AudiblePlanError}
	// audiblePlanConflicts are what an existing place holds (state exists).
	audiblePlanConflicts = []string{"", "file", "book", "audio"}
)

// AudiblePlan is where the Bridge would place a backup of a title, as it
// planned it at its last poll: the file's library-relative path, its name,
// the kind of place, how many books of the series are there already, the
// state, what is in the way (or, for state error, why the plan failed) and
// the template the plan used ("" for the automatic placement).
type AudiblePlan struct {
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"`
	File      string `json:"file"`
	Kind      string `json:"kind"`
	Siblings  int    `json:"siblings"`
	State     string `json:"state"`
	Conflict  string `json:"conflict"`
	Template  string `json:"template"`
}

// AudibleContest is a contested title's book and the account's other titles
// that matched it, or claimed it too.
type AudibleContest struct {
	LibraryID int64              `json:"library_id"`
	Path      string             `json:"path"`
	With      []AudibleContender `json:"with"`
}

// AudibleContender is another title of the account on a contested book.
type AudibleContender struct {
	ASIN  string `json:"asin"`
	Title string `json:"title"`
}

// AudibleTitle is one title an account owns, as the server keeps it: what the
// poll reported (its fields inline on the wire) and the console's and an
// import's columns.
type AudibleTitle struct {
	Account string `json:"account"`
	AudibleTitleInput
	Rejected bool `json:"rejected"`
	Ignored  bool `json:"ignored"`
	// NoSync is the console's "Don't sync this book" (Phase 4c): neither
	// direction of the account's sync touches its book. Backups don't read it.
	NoSync      bool   `json:"no_sync"`
	DestPath    string `json:"dest_path"`
	FirstSeen   string `json:"first_seen"`
	UpdatedAt   string `json:"updated_at"`
	ImportJobID *int64 `json:"import_job_id"`
	// Import is the title's backup (Phase 4b): its job's state and code, or a
	// console request waiting for a preorder ("pending"); nil for none.
	Import *AudibleImport `json:"import"`
	// NeedsAttention: a preorder still not available AudiblePreorderWait after
	// its release date; the account is no longer checked hourly for it.
	NeedsAttention bool `json:"needs_attention"`
	// Contested is the book a contested title tied for and the account's
	// other titles on it (nil for none).
	Contested *AudibleContest `json:"contested"`
}

// checkAudibleTitle checks and normalises one reported title: its identity,
// state, dates and match strictly (a Bridge's mistake), its free text cleaned
// and bounded (what Audible said, never refused: an anthology's long author
// list keeps its first maxAudibleNames).
func checkAudibleTitle(t *AudibleTitleInput) error {
	t.ASIN = strings.ToUpper(strings.TrimSpace(t.ASIN))
	if !ValidASIN(t.ASIN) {
		return audibleInvalid("asin %q is not an ASIN", t.ASIN)
	}
	if t.State == AudibleGone || !slices.Contains(AudibleStates, t.State) {
		return audibleInvalid("state must be available, preorder or unavailable")
	}
	if t.ReleaseDate != "" {
		if _, err := time.Parse(time.DateOnly, t.ReleaseDate); err != nil {
			return audibleInvalid("release_date must be YYYY-MM-DD")
		}
	}
	if t.RuntimeS < 0 || t.RuntimeS > maxAudibleRuntime {
		return audibleInvalid("runtime_s out of range")
	}
	if m := t.Match; m != nil {
		if m.LibraryID <= 0 || !bridgeBookPath(m.Path) || (m.How != "asin" && m.How != "fuzzy") {
			return audibleInvalid("match must name a library, a clean book path and how (asin or fuzzy)")
		}
	}
	if t.ContestedPath != "" && !bridgeBookPath(t.ContestedPath) {
		return audibleInvalid("contested_path must be a clean book path")
	}
	if err := checkAudiblePlan(t.Planned); err != nil {
		return err
	}
	t.Title = bridge.CleanText(t.Title, maxAudibleTitle)
	t.Reason = bridge.CleanText(t.Reason, maxAudibleReason)
	authors := make([]string, 0, min(len(t.Authors), maxAudibleNames))
	for _, a := range t.Authors {
		if len(authors) == maxAudibleNames {
			break
		}
		if a = bridge.CleanText(a, maxAudibleName); a != "" {
			authors = append(authors, a)
		}
	}
	t.Authors = authors
	series := make([]AudibleSeries, 0, min(len(t.Series), maxAudibleSeries))
	for _, s := range t.Series {
		if len(series) == maxAudibleSeries {
			break
		}
		s.Name, s.Position = bridge.CleanText(s.Name, maxAudibleName), bridge.CleanText(s.Position, maxAudiblePosition)
		if s.Name != "" {
			series = append(series, s)
		}
	}
	t.Series = series
	return nil
}

// checkAudiblePlan checks a title's planned destination strictly (a Bridge's
// mistake): a library, a clean file path and name (both "" allowed only for a
// failed plan), a known kind and state, a sibling count in range and a
// template within MaxAudibleTemplate. Its conflict is a known one, or for a
// failed plan the reason, cleaned and bounded.
func checkAudiblePlan(p *AudiblePlan) error {
	if p == nil {
		return nil
	}
	// A path past what the server keeps is Audible's long text through the
	// account's template (the Bridge bounds no length), not a Bridge's
	// mistake: the plan reads as one that failed, never a refused poll.
	if len(p.Path) > MaxPathBytes && slices.Contains(audiblePlanStates, p.State) {
		p.State, p.Path, p.File = AudiblePlanError, "", ""
		p.Conflict = fmt.Sprintf("the path is longer than the server allows (%d bytes)", MaxPathBytes)
	}
	failed := p.State == AudiblePlanError
	switch {
	case p.LibraryID <= 0:
		return audibleInvalid("planned.library_id is required")
	case !slices.Contains(audiblePlanStates, p.State):
		return audibleInvalid("planned.state must be one of %s", strings.Join(audiblePlanStates, ", "))
	case !slices.Contains(audiblePlanKinds, p.Kind) && !(failed && p.Kind == ""):
		return audibleInvalid("planned.kind must be one of %s", strings.Join(audiblePlanKinds, ", "))
	case !cleanPlanPath(p.Path, failed):
		return audibleInvalid("planned.path must be a clean library-relative file path")
	case !cleanPlanPath(p.File, failed) || strings.Contains(p.File, "/"):
		return audibleInvalid("planned.file must be a file name")
	case p.Siblings < 0 || p.Siblings > maxAudibleSiblings:
		return audibleInvalid("planned.siblings out of range")
	case !bridge.AudibleTemplateBounded(p.Template):
		return audibleInvalid("planned.template must be at most %d bytes", bridge.MaxAudibleTemplate)
	}
	if failed {
		p.Conflict = bridge.CleanText(p.Conflict, maxAudibleReason)
	} else if !slices.Contains(audiblePlanConflicts, p.Conflict) {
		return audibleInvalid("planned.conflict must be file, book, audio or empty")
	}
	return nil
}

// cleanPlanPath reports whether p is a clean library-relative path ("" too
// when empty is allowed).
func cleanPlanPath(p string, empty bool) bool {
	return (empty && p == "") || cleanLibPath(p)
}

// cleanLibPath reports whether p is a non-empty clean library-relative path
// within MaxPathBytes.
func cleanLibPath(p string) bool {
	return p != "" && len(p) <= MaxPathBytes && CleanRelPath(p) == p
}

// bridgeBookPath reports whether p is a book path as BridgeBooks lists one: a
// clean library-relative path, or "." for a `book` override on the library
// root (the index keeps that book's path as "."; CleanRelPath would make it "").
func bridgeBookPath(p string) bool {
	return p == "." || cleanLibPath(p)
}

// PutAudibleTitles stores titles (at most MaxAudibleTitles) of one poll of
// account (pollID, the Bridge's), each replacing what an earlier poll said of
// it (its planned destination and contested book too, both dropped when a
// match is stored) but keeping the console's and an import's columns
// (rejected, ignored, no_sync, dest_path, import_job_id, first_seen). A match to a library that is gone or
// not a books library is stored as none. A backup's match (how "import",
// FoldAudibleImport) stays while its book is indexed and the poll names no
// other: the next poll may have listed the library before the backup landed.
// On the poll's final call every title of the account the poll didn't report
// becomes gone, and the account's last_poll_at is now, its sync's refusal
// ended (refused_at: Audible answered the account's whole library); on its
// first complete poll its policy_since moves to now too, so the library the account already
// had is never backed up by a policy (new purchases only). A user-state write:
// no content cache reads these tables.
func (c *Catalog) PutAudibleTitles(ctx context.Context, account, pollID string, final bool, titles []AudibleTitleInput) error {
	if !audiblePollPattern.MatchString(pollID) {
		return audibleInvalid("poll_id must be 1-64 letters, digits, dots, dashes or underscores")
	}
	if len(titles) > MaxAudibleTitles {
		return audibleInvalid("at most %d titles per call", MaxAudibleTitles)
	}
	for i := range titles {
		if err := checkAudibleTitle(&titles[i]); err != nil {
			return err
		}
	}
	return c.db.WithUserStateTx(ctx, "PutAudibleTitles", func(tx *sql.Tx) error {
		// The account's library (NULL while paused) is a contest's.
		var acctLib sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT library_id FROM audible_accounts WHERE account = ?`, account).Scan(&acctLib)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknownAccount
		}
		if err != nil {
			return err
		}
		books, err := booksLibraries(ctx, tx)
		if err != nil {
			return err
		}
		now := c.ts()
		imported, err := importMatches(ctx, tx, account)
		if err != nil {
			return err
		}
		upsert, err := tx.PrepareContext(ctx,
			`INSERT INTO audible_titles(account, asin, title, authors, series, release_date, runtime_s, state, reason,
			     match_library_id, match_path, match_how, first_seen, updated_at, poll_id, planned,
			     contest_library_id, contest_path)
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(account, asin) DO UPDATE SET title = excluded.title, authors = excluded.authors,
			     series = excluded.series, release_date = excluded.release_date, runtime_s = excluded.runtime_s,
			     state = excluded.state, reason = excluded.reason, match_library_id = excluded.match_library_id,
			     match_path = excluded.match_path, match_how = excluded.match_how,
			     updated_at = excluded.updated_at, poll_id = excluded.poll_id,
			     planned = excluded.planned, contest_library_id = excluded.contest_library_id,
			     contest_path = excluded.contest_path`)
		if err != nil {
			return err
		}
		defer func() { _ = upsert.Close() }()
		for _, t := range titles {
			authors, _ := json.Marshal(t.Authors)
			series, _ := json.Marshal(t.Series)
			var (
				matchLib  any
				matchPath any
				how       string
			)
			if t.Match != nil && books[t.Match.LibraryID] {
				matchLib, matchPath, how = t.Match.LibraryID, t.Match.Path, t.Match.How
			}
			if im, ok := imported[t.ASIN]; ok && books[im.LibraryID] &&
				(matchPath == nil || (t.Match.LibraryID == im.LibraryID && t.Match.Path == im.Path)) {
				matchLib, matchPath, how = im.LibraryID, im.Path, AudibleMatchImport
			}
			// The plan and the contest are an unmatched title's: a stored match
			// drops both.
			var (
				planned, contest string
				contestLib       any
			)
			if matchPath == nil {
				// The contest's path is in the library the poll listed: the
				// account's, unless the poll planned in another (the account
				// was mapped elsewhere while it ran), whose path this isn't.
				if t.ContestedPath != "" && acctLib.Valid && (t.Planned == nil || t.Planned.LibraryID == acctLib.Int64) {
					contestLib, contest = acctLib.Int64, t.ContestedPath
				}
				if t.Planned != nil {
					raw, _ := json.Marshal(t.Planned)
					planned = string(raw)
				}
			}
			if _, err := upsert.ExecContext(ctx,
				account, t.ASIN, t.Title, string(authors), string(series), t.ReleaseDate, t.RuntimeS, t.State, t.Reason,
				matchLib, matchPath, how, now, now, pollID, planned, contestLib, contest); err != nil {
				return err
			}
		}
		if !final {
			return nil
		}
		// A title gone from the account is backed up nowhere: its plan goes.
		if _, err := tx.ExecContext(ctx,
			`UPDATE audible_titles SET state = 'gone', planned = '', updated_at = ?
			  WHERE account = ? AND poll_id <> ? AND state <> 'gone'`,
			now, account, pollID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE audible_accounts SET last_poll_at = ?1, refused_at = NULL,
			    policy_since = CASE WHEN last_poll_at IS NULL THEN ?1 ELSE policy_since END WHERE account = ?2`, now, account)
		return err
	})
}

// importMatches are account's titles matched by a backup whose book is still
// indexed, by ASIN, read through tx.
func importMatches(ctx context.Context, tx *sql.Tx, account string) (map[string]AudibleMatch, error) {
	rows, err := queryRows(ctx, tx, func(rows *sql.Rows, m *struct {
		asin string
		AudibleMatch
	}) error {
		return rows.Scan(&m.asin, &m.LibraryID, &m.Path)
	}, `SELECT t.asin, t.match_library_id, t.match_path FROM audible_titles t
		  WHERE t.account = ? AND t.match_how = ? AND t.match_path IS NOT NULL
		    AND EXISTS(SELECT 1 FROM books b WHERE b.library_id = t.match_library_id AND b.rel_path = t.match_path)`,
		account, AudibleMatchImport)
	out := make(map[string]AudibleMatch, len(rows))
	for _, r := range rows {
		out[r.asin] = r.AudibleMatch
	}
	return out, err
}

// booksLibraries are the ids of the books libraries, read through q.
func booksLibraries(ctx context.Context, q rowQuerier) (map[int64]bool, error) {
	ids, err := queryRows(ctx, q, func(rows *sql.Rows, id *int64) error { return rows.Scan(id) },
		`SELECT id FROM libraries WHERE kind = ?`, KindBooks)
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, err
}

// AudibleTitleFilter narrows the console's titles list.
type AudibleTitleFilter struct {
	Account string // "" for every account
	State   string // one of AudibleStates, "" for any
	// Matched: nil any, true only titles matched to a book here (not marked
	// "Not this book"), false only the rest.
	Matched *bool
	Query   string // in the title, the authors or the ASIN
	After   string // the cursor from the previous page
	Limit   int
}

// AudibleTitlePage is a page of titles and the cursor for the next ("" at the end).
type AudibleTitlePage struct {
	Titles []AudibleTitle `json:"titles"`
	Next   string         `json:"next"`
}

// audibleCursor is where a titles page ended: the last row's sort key.
type audibleCursor struct {
	Title   string `json:"t"`
	Account string `json:"a"`
	ASIN    string `json:"s"`
}

// AudibleTitles lists titles by title (any case), then account and ASIN,
// keyset-paged, each with its backup (AudibleImport) and whether it needs
// attention.
func (c *Catalog) AudibleTitles(ctx context.Context, f AudibleTitleFilter) (AudibleTitlePage, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultAudibleTitlesPage
	}
	limit = min(limit, MaxAudibleTitlesPage)
	var (
		where []string
		args  []any
	)
	if f.Account != "" {
		where, args = append(where, `t.account = ?`), append(args, f.Account)
	}
	if f.State != "" {
		if !slices.Contains(AudibleStates, f.State) {
			return AudibleTitlePage{}, audibleInvalid("state must be one of %s", strings.Join(AudibleStates, ", "))
		}
		where, args = append(where, `t.state = ?`), append(args, f.State)
	}
	if f.Matched != nil {
		if *f.Matched {
			where = append(where, `t.match_path IS NOT NULL AND t.rejected = 0`)
		} else {
			where = append(where, `(t.match_path IS NULL OR t.rejected = 1)`)
		}
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		if len(q) > maxAudibleQueryText {
			return AudibleTitlePage{}, audibleInvalid("query too long")
		}
		// Each author as written (json_each decodes the stored list), so a
		// search never matches the list's JSON: its quotes, brackets, escapes.
		like := "%" + escapeLike(q) + "%"
		where = append(where, `(t.title LIKE ? ESCAPE '\' OR t.asin = ?
		    OR EXISTS(SELECT 1 FROM json_each(t.authors) WHERE value LIKE ? ESCAPE '\'))`)
		args = append(args, like, strings.ToUpper(q), like)
	}
	if f.After != "" {
		var cur audibleCursor
		raw, err := base64.RawURLEncoding.DecodeString(f.After)
		if err != nil || json.Unmarshal(raw, &cur) != nil {
			return AudibleTitlePage{}, audibleInvalid("bad cursor")
		}
		// The leading range term lets the title index seek to the cursor; the
		// row value keeps the exact order past it.
		where = append(where, `t.title >= ? COLLATE NOCASE`,
			`(t.title COLLATE NOCASE, t.account, t.asin) > (? COLLATE NOCASE, ?, ?)`)
		args = append(args, cur.Title, cur.Title, cur.Account, cur.ASIN)
	}
	query := audibleTitleSelect
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY t.title COLLATE NOCASE, t.account, t.asin LIMIT ?`
	args = append(args, limit+1)
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return AudibleTitlePage{}, err
	}
	defer func() { _ = rows.Close() }()
	page := AudibleTitlePage{Titles: []AudibleTitle{}}
	now := c.now()
	for rows.Next() {
		t, err := scanAudibleTitle(rows, now)
		if err != nil {
			return AudibleTitlePage{}, err
		}
		page.Titles = append(page.Titles, *t)
	}
	if err := rows.Err(); err != nil {
		return AudibleTitlePage{}, err
	}
	if len(page.Titles) > limit {
		page.Titles = page.Titles[:limit]
		last := page.Titles[limit-1]
		raw, _ := json.Marshal(audibleCursor{Title: last.Title, Account: last.Account, ASIN: last.ASIN})
		page.Next = base64.RawURLEncoding.EncodeToString(raw)
	}
	_ = rows.Close()
	return page, c.fillContests(ctx, page.Titles)
}

// fillContests lists, for each contested title of titles, the other titles of
// its account on the same book: matched to it, or left contested over it too
// (never a gone one: the poll that contested it didn't have it). By title, at
// most maxAudibleContest each. One query for the page.
func (c *Catalog) fillContests(ctx context.Context, titles []AudibleTitle) error {
	type key struct {
		account string
		ref     Ref
	}
	var (
		terms []string
		args  []any
		seen  = map[key]bool{}
	)
	for _, t := range titles {
		if t.Contested == nil {
			continue
		}
		k := key{t.Account, Ref{LibraryID: t.Contested.LibraryID, Path: t.Contested.Path}}
		if seen[k] {
			continue
		}
		seen[k] = true
		terms = append(terms, `(o.account = ? AND ((o.contest_library_id = ? AND o.contest_path = ?)
			OR (o.match_library_id = ? AND o.match_path = ?)))`)
		args = append(args, k.account, k.ref.LibraryID, k.ref.Path, k.ref.LibraryID, k.ref.Path)
	}
	if len(terms) == 0 {
		return nil
	}
	rows, err := queryRows(ctx, c.db, func(rows *sql.Rows, r *struct {
		account        string
		contest, match Ref
		AudibleContender
	}) error {
		return rows.Scan(&r.account, &r.contest.LibraryID, &r.contest.Path, &r.match.LibraryID, &r.match.Path,
			&r.ASIN, &r.Title)
	}, `SELECT o.account, COALESCE(o.contest_library_id, 0), o.contest_path,
		    COALESCE(o.match_library_id, 0), COALESCE(o.match_path, ''), o.asin, o.title
		  FROM audible_titles o
		 WHERE o.state <> 'gone' AND (`+strings.Join(terms, ` OR `)+`)
		 ORDER BY o.title COLLATE NOCASE, o.asin`, args...)
	if err != nil {
		return err
	}
	for i := range titles {
		t := &titles[i]
		if t.Contested == nil {
			continue
		}
		book := Ref{LibraryID: t.Contested.LibraryID, Path: t.Contested.Path}
		for _, r := range rows {
			if len(t.Contested.With) == maxAudibleContest {
				break
			}
			if r.account == t.Account && r.ASIN != t.ASIN && (r.contest == book || r.match == book) {
				t.Contested.With = append(t.Contested.With, r.AudibleContender)
			}
		}
	}
	return nil
}

// audibleTitleSelect reads a title (aliased t) with its import job (j) and its
// account's library (acct), for scanAudibleTitle.
const audibleTitleSelect = `SELECT t.account, t.asin, t.title, t.authors, t.series, t.release_date, t.runtime_s, t.state,
	t.reason, t.match_library_id, t.match_path, t.match_how, t.rejected, t.ignored, t.no_sync, t.dest_path, t.first_seen,
	t.updated_at, t.import_job_id, t.import_pending, j.state, j.error, j.result, j.progress,
	t.planned, t.contest_library_id, t.contest_path, acct.library_id
	FROM audible_titles t LEFT JOIN bridge_jobs j ON j.id = t.import_job_id
	LEFT JOIN audible_accounts acct ON acct.account = t.account`

// scanAudibleTitle reads one row of audibleTitleSelect, its needs_attention
// as of now.
func scanAudibleTitle(row rowScanner, now time.Time) (*AudibleTitle, error) {
	var (
		t                                   AudibleTitle
		authors, series                     string
		matchLib, jobID                     sql.NullInt64
		matchPath                           sql.NullString
		how                                 string
		pending                             bool
		jobState, jobErr, jobRes, jobProgrs sql.NullString
		planned, contest                    string
		contestLib, acctLib                 sql.NullInt64
	)
	if err := row.Scan(&t.Account, &t.ASIN, &t.Title, &authors, &series, &t.ReleaseDate, &t.RuntimeS, &t.State,
		&t.Reason, &matchLib, &matchPath, &how, &t.Rejected, &t.Ignored, &t.NoSync, &t.DestPath, &t.FirstSeen, &t.UpdatedAt,
		&jobID, &pending, &jobState, &jobErr, &jobRes, &jobProgrs, &planned, &contestLib, &contest, &acctLib); err != nil {
		return nil, err
	}
	if json.Unmarshal([]byte(authors), &t.Authors) != nil || t.Authors == nil {
		t.Authors = []string{}
	}
	if json.Unmarshal([]byte(series), &t.Series) != nil || t.Series == nil {
		t.Series = []AudibleSeries{}
	}
	if matchLib.Valid && matchPath.Valid {
		t.Match = &AudibleMatch{LibraryID: matchLib.Int64, Path: matchPath.String, How: how}
	}
	// The plan and the contest are in the account's library: a paused account
	// (its library deleted) or one mapped to another library since shows
	// neither until its next poll. (A stored match clears both.)
	if planned != "" && acctLib.Valid {
		var p AudiblePlan
		if json.Unmarshal([]byte(planned), &p) == nil && p.LibraryID == acctLib.Int64 {
			t.Planned = &p
		}
	}
	if contest != "" && contestLib.Valid && contestLib == acctLib {
		t.Contested = &AudibleContest{LibraryID: contestLib.Int64, Path: contest, With: []AudibleContender{}}
	}
	if jobID.Valid {
		t.ImportJobID = &jobID.Int64
	}
	switch {
	case pending && (t.Match == nil || t.Rejected): // a match marked "Not this book" isn't this title
		t.Import = &AudibleImport{State: AudibleImportPending}
	case jobID.Valid && jobState.Valid:
		j := bridge.Job{ID: jobID.Int64, State: jobState.String, Error: jobErr.String,
			Result: json.RawMessage(jobRes.String), Progress: json.RawMessage(jobProgrs.String)}
		t.Import = &AudibleImport{JobID: &j.ID, State: j.State, Code: bridge.AudibleImportCode(j)}
	}
	t.NeedsAttention = preorderOverdue(&t, pending, now)
	return &t, nil
}

// DueAudiblePolls are the accounts whose scheduled poll is due at now: mapped
// to a library (a paused one waits) and to a person an account may still be
// mapped to (bridgeUserSQL: one disabled or made a kid since waits too), and
// never queued, or last queued at least poll_hours ago (an hour ago while one
// of its preorders is waiting: released by the UTC date, not yet available,
// for at most AudiblePreorderWait: preorderWaitingSQL). At most limit, the
// longest waiting first, each with its placement template (the poll plans by
// it).
func (c *Catalog) DueAudiblePolls(ctx context.Context, now time.Time, limit int) ([]DueAudiblePoll, error) {
	today, cutoff := preorderWindow(now)
	rows, err := c.db.QueryContext(ctx,
		`SELECT a.account, a.template, a.poll_hours, a.last_queued_at,
		     EXISTS(SELECT 1 FROM audible_titles t WHERE t.account = a.account AND `+preorderWaitingSQL+`)
		   FROM audible_accounts a JOIN users u ON u.id = a.user_id
		  WHERE a.library_id IS NOT NULL AND `+bridgeUserSQL+`
		  ORDER BY a.last_queued_at IS NOT NULL, a.last_queued_at, a.account`, today, cutoff)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DueAudiblePoll
	for rows.Next() && len(out) < limit {
		var (
			due     DueAudiblePoll
			hours   int
			queued  sql.NullString
			waiting bool
		)
		if err := rows.Scan(&due.Account, &due.Template, &hours, &queued, &waiting); err != nil {
			return nil, err
		}
		if waiting {
			hours = 1
		}
		if queued.Valid {
			at, err := time.Parse(time.RFC3339Nano, queued.String)
			if err == nil && now.Sub(at) < time.Duration(max(hours, 1))*time.Hour {
				continue
			}
		}
		out = append(out, due)
	}
	return out, rows.Err()
}

// DueAudiblePoll is an account whose poll is due and its placement template.
type DueAudiblePoll struct {
	Account  string
	Template string
}

// MarkAudibleQueued records that account's poll was queued now (the scheduler
// counts poll_hours from it). A user-state write.
func (c *Catalog) MarkAudibleQueued(ctx context.Context, account string) error {
	_, err := c.db.ExecUserState(ctx, `UPDATE audible_accounts SET last_queued_at = ? WHERE account = ?`, c.ts(), account)
	return err
}

// BridgeBooks is a page of a books library's indexed books by path, for the
// Bridge's matching: the books after path after (""), at most limit, and the
// cursor of the next page ("" at the end). ErrNotFound for no library,
// ErrNotBookLibrary for a podcast library.
func (c *Catalog) BridgeBooks(ctx context.Context, libraryID int64, after string, limit int) ([]Book, string, error) {
	lib, err := c.GetLibrary(ctx, libraryID)
	if err != nil {
		return nil, "", err
	}
	if lib.Kind != KindBooks {
		return nil, "", ErrNotBookLibrary
	}
	if limit <= 0 || limit > maxBridgeBooks {
		limit = maxBridgeBooks
	}
	from := ""
	if after != "" {
		raw, err := base64.RawURLEncoding.DecodeString(after)
		if err != nil {
			return nil, "", audibleInvalid("bad cursor")
		}
		from = string(raw)
	}
	rows, err := c.db.QueryContext(ctx, `SELECT `+bookCols+` FROM books WHERE library_id = ? AND rel_path > ?
		ORDER BY rel_path LIMIT ?`, libraryID, from, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = rows.Close() }()
	out := []Book{}
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, *b)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = base64.RawURLEncoding.EncodeToString([]byte(out[limit-1].RelPath))
	}
	return out, next, nil
}

// BackfillASIN attaches asin to an indexed book that has none (the Bridge's
// unambiguous Audible match), durably (book_enrichment) and on the index row
// at once. ErrNotFound when no book of a books library is indexed at path;
// ErrASINSet when the book has another ASIN, or an admin's edit of its ASIN
// (an override, a cleared one too: a backfill never goes under one). The
// same ASIN again is a no-op. Only a write moves the content generation: the
// Bridge asks again for such a book after every poll.
func (c *Catalog) BackfillASIN(ctx context.Context, libraryID int64, path, asin string) error {
	if path != "." { // the library root's book keeps "." (bridgeBookPath)
		path = CleanRelPath(path)
	}
	if write, err := backfillWanted(ctx, c.db, libraryID, path, asin); err != nil || !write {
		return err
	}
	written := false
	err := c.db.WithTx(ctx, "BackfillASIN", func(tx *sql.Tx) error {
		// Again in the writer: an edit or a scan may have landed since.
		write, err := backfillWanted(ctx, tx, libraryID, path, asin)
		if err != nil || !write {
			return err
		}
		written = true
		return setEnrichmentTx(ctx, tx, libraryID, path, asin, "", c.ts())
	})
	if err == nil && written {
		c.changed()
	}
	return err
}

// backfillWanted reports whether BackfillASIN writes asin onto the book at
// path, read through q: false for a book that has it already, ErrNotFound for
// no book of a books library (a podcast's episode is none), ErrASINSet for a
// book with another ASIN or an ASIN override.
func backfillWanted(ctx context.Context, q querier, libraryID int64, path, asin string) (bool, error) {
	var (
		have, kind string
		edited     bool
	)
	err := q.QueryRowContext(ctx, `SELECT b.asin, l.kind,
		    EXISTS(SELECT 1 FROM book_overrides o WHERE o.library_id = b.library_id AND o.path = b.rel_path AND o.field = ?)
		  FROM books b JOIN libraries l ON l.id = b.library_id WHERE b.library_id = ? AND b.rel_path = ?`,
		FieldASIN, libraryID, path).Scan(&have, &kind, &edited)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, ErrNotFound
	case err != nil:
		return false, err
	case kind != KindBooks:
		return false, ErrNotFound
	case strings.EqualFold(have, asin):
		return false, nil
	case have != "" || edited:
		return false, ErrASINSet
	}
	return true, nil
}
