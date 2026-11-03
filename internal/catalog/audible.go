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
// a book on this server.
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
	// Titles are the account's titles counted (the console's list only).
	Titles *AudibleCounts `json:"titles,omitempty"`
}

// AudibleAccountInput is the Bridge's mapping of an account (PUT
// /bridge/audible/accounts/{account}).
type AudibleAccountInput struct {
	Label       string `json:"label"`
	Marketplace string `json:"marketplace"`
	UserID      int64  `json:"user_id"`
	LibraryID   int64  `json:"library_id"`
	Policy      string `json:"policy"`
	Sync        bool   `json:"sync"`
}

const audibleAccountSelect = `SELECT a.account, a.label, a.marketplace, a.user_id, ` + displayNameSQL + `,
	a.library_id, l.name, a.policy, a.sync, a.poll_hours, a.created_at, a.updated_at, a.last_poll_at, a.last_queued_at
	FROM audible_accounts a JOIN users u ON u.id = a.user_id LEFT JOIN libraries l ON l.id = a.library_id`

func scanAudibleAccount(row rowScanner) (*AudibleAccount, error) {
	var (
		a                    AudibleAccount
		lib                  sql.NullInt64
		libName, poll, queue sql.NullString
	)
	if err := row.Scan(&a.ID, &a.Label, &a.Marketplace, &a.UserID, &a.User.DisplayName, &lib, &libName,
		&a.Policy, &a.Sync, &a.PollHours, &a.CreatedAt, &a.UpdatedAt, &poll, &queue); err != nil {
		return nil, err
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
// titles and poll times stay.
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
		now := c.ts()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO audible_accounts(account, label, marketplace, user_id, library_id, policy, sync, created_at, updated_at)
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(account) DO UPDATE SET label = excluded.label, marketplace = excluded.marketplace,
			     user_id = excluded.user_id, library_id = excluded.library_id, policy = excluded.policy,
			     sync = excluded.sync, updated_at = excluded.updated_at`,
			id, in.Label, in.Marketplace, in.UserID, in.LibraryID, in.Policy, in.Sync, now, now); err != nil {
			return err
		}
		out, err = scanAudibleAccount(tx.QueryRowContext(ctx, audibleAccountSelect+` WHERE a.account = ?`, id))
		return err
	})
	return out, err
}

// DeleteAudibleAccount forgets account id and its titles (the Bridge removed
// it), cancelling its open poll. Forgetting an unknown account is not an
// error: the Bridge forgets locally first and may call again.
func (c *Catalog) DeleteAudibleAccount(ctx context.Context, id string) error {
	return c.db.WithTx(ctx, "DeleteAudibleAccount", func(tx *sql.Tx) error {
		if err := bridge.CancelAudiblePolls(ctx, tx, id, bridge.CancelAudibleRemoved, c.now()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM audible_accounts WHERE account = ?`, id)
		return err
	})
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
	byID := map[string]*AudibleCounts{}
	for rows.Next() {
		a, err := scanAudibleAccount(rows)
		if err != nil {
			return nil, err
		}
		a.Titles = &AudibleCounts{}
		byID[a.ID] = a.Titles
		out = append(out, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = rows.Close()
	counts, err := c.db.QueryContext(ctx, `SELECT account, COUNT(*),
		    SUM(state = 'available'), SUM(state = 'preorder'), SUM(state = 'unavailable'), SUM(state = 'gone'),
		    SUM(match_path IS NOT NULL)
		  FROM audible_titles GROUP BY account`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = counts.Close() }()
	for counts.Next() {
		var (
			id string
			n  AudibleCounts
		)
		if err := counts.Scan(&id, &n.Total, &n.Available, &n.Preorder, &n.Unavailable, &n.Gone, &n.Matched); err != nil {
			return nil, err
		}
		if c := byID[id]; c != nil {
			*c = n
		}
	}
	return out, counts.Err()
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
}

// AudibleTitle is one title an account owns, as the server keeps it: what the
// poll reported (its fields inline on the wire) and the console's and an
// import's columns.
type AudibleTitle struct {
	Account string `json:"account"`
	AudibleTitleInput
	Rejected    bool   `json:"rejected"`
	Ignored     bool   `json:"ignored"`
	DestPath    string `json:"dest_path"`
	FirstSeen   string `json:"first_seen"`
	UpdatedAt   string `json:"updated_at"`
	ImportJobID *int64 `json:"import_job_id"`
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

// bridgeBookPath reports whether p is a book path as BridgeBooks lists one: a
// clean library-relative path, or "." for a `book` override on the library
// root (the index keeps that book's path as "."; CleanRelPath would make it "").
func bridgeBookPath(p string) bool {
	return p == "." || (p != "" && len(p) <= MaxPathBytes && CleanRelPath(p) == p)
}

// PutAudibleTitles stores titles (at most MaxAudibleTitles) of one poll of
// account (pollID, the Bridge's), each replacing what an earlier poll said of
// it but keeping the console's and an import's columns (rejected, ignored,
// dest_path, import_job_id, first_seen). A match to a library that is gone or
// not a books library is stored as none. On the poll's final call every title
// of the account the poll didn't report becomes gone, and the account's
// last_poll_at is now. A user-state write: no content cache reads these
// tables.
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
		var ok bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM audible_accounts WHERE account = ?)`, account).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return ErrUnknownAccount
		}
		books, err := booksLibraries(ctx, tx)
		if err != nil {
			return err
		}
		now := c.ts()
		upsert, err := tx.PrepareContext(ctx,
			`INSERT INTO audible_titles(account, asin, title, authors, series, release_date, runtime_s, state, reason,
			     match_library_id, match_path, match_how, first_seen, updated_at, poll_id)
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(account, asin) DO UPDATE SET title = excluded.title, authors = excluded.authors,
			     series = excluded.series, release_date = excluded.release_date, runtime_s = excluded.runtime_s,
			     state = excluded.state, reason = excluded.reason, match_library_id = excluded.match_library_id,
			     match_path = excluded.match_path, match_how = excluded.match_how,
			     updated_at = excluded.updated_at, poll_id = excluded.poll_id`)
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
			if _, err := upsert.ExecContext(ctx,
				account, t.ASIN, t.Title, string(authors), string(series), t.ReleaseDate, t.RuntimeS, t.State, t.Reason,
				matchLib, matchPath, how, now, now, pollID); err != nil {
				return err
			}
		}
		if !final {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE audible_titles SET state = 'gone', updated_at = ? WHERE account = ? AND poll_id <> ? AND state <> 'gone'`,
			now, account, pollID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE audible_accounts SET last_poll_at = ? WHERE account = ?`, now, account)
		return err
	})
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
	// Matched: nil any, true only titles matched to a book here, false only the rest.
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
// keyset-paged.
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
		where, args = append(where, `account = ?`), append(args, f.Account)
	}
	if f.State != "" {
		if !slices.Contains(AudibleStates, f.State) {
			return AudibleTitlePage{}, audibleInvalid("state must be one of %s", strings.Join(AudibleStates, ", "))
		}
		where, args = append(where, `state = ?`), append(args, f.State)
	}
	if f.Matched != nil {
		if *f.Matched {
			where = append(where, `match_path IS NOT NULL`)
		} else {
			where = append(where, `match_path IS NULL`)
		}
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		if len(q) > maxAudibleQueryText {
			return AudibleTitlePage{}, audibleInvalid("query too long")
		}
		// Each author as written (json_each decodes the stored list), so a
		// search never matches the list's JSON: its quotes, brackets, escapes.
		like := "%" + escapeLike(q) + "%"
		where = append(where, `(title LIKE ? ESCAPE '\' OR asin = ?
		    OR EXISTS(SELECT 1 FROM json_each(authors) WHERE value LIKE ? ESCAPE '\'))`)
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
		where = append(where, `title >= ? COLLATE NOCASE`,
			`(title COLLATE NOCASE, account, asin) > (? COLLATE NOCASE, ?, ?)`)
		args = append(args, cur.Title, cur.Title, cur.Account, cur.ASIN)
	}
	query := `SELECT account, asin, title, authors, series, release_date, runtime_s, state, reason,
		match_library_id, match_path, match_how, rejected, ignored, dest_path, first_seen, updated_at, import_job_id
		FROM audible_titles`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY title COLLATE NOCASE, account, asin LIMIT ?`
	args = append(args, limit+1)
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return AudibleTitlePage{}, err
	}
	defer func() { _ = rows.Close() }()
	page := AudibleTitlePage{Titles: []AudibleTitle{}}
	for rows.Next() {
		var (
			t               AudibleTitle
			authors, series string
			matchLib, jobID sql.NullInt64
			matchPath       sql.NullString
			how             string
		)
		if err := rows.Scan(&t.Account, &t.ASIN, &t.Title, &authors, &series, &t.ReleaseDate, &t.RuntimeS, &t.State,
			&t.Reason, &matchLib, &matchPath, &how, &t.Rejected, &t.Ignored, &t.DestPath, &t.FirstSeen, &t.UpdatedAt,
			&jobID); err != nil {
			return AudibleTitlePage{}, err
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
		if jobID.Valid {
			t.ImportJobID = &jobID.Int64
		}
		page.Titles = append(page.Titles, t)
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
	return page, nil
}

// DueAudiblePolls are the accounts whose scheduled poll is due at now: mapped
// to a library (a paused one waits) and to a person an account may still be
// mapped to (bridgeUserSQL: one disabled or made a kid since waits too), and
// never queued, or last queued at least poll_hours ago. At most limit, the
// longest waiting first.
func (c *Catalog) DueAudiblePolls(ctx context.Context, now time.Time, limit int) ([]string, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT a.account, a.poll_hours, a.last_queued_at FROM audible_accounts a JOIN users u ON u.id = a.user_id
		  WHERE a.library_id IS NOT NULL AND `+bridgeUserSQL+`
		  ORDER BY a.last_queued_at IS NOT NULL, a.last_queued_at, a.account`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() && len(out) < limit {
		var (
			id     string
			hours  int
			queued sql.NullString
		)
		if err := rows.Scan(&id, &hours, &queued); err != nil {
			return nil, err
		}
		if queued.Valid {
			at, err := time.Parse(time.RFC3339Nano, queued.String)
			if err == nil && now.Sub(at) < time.Duration(max(hours, 1))*time.Hour {
				continue
			}
		}
		out = append(out, id)
	}
	return out, rows.Err()
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
