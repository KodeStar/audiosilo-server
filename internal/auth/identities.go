package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/kodestar/audiosilo-server/internal/oidc"
	"github.com/kodestar/audiosilo-server/internal/store"
)

// Sign in with OpenID Connect's storage (server#235, migration 0077): an
// account's identities at the provider (user_identities), the sign-ins no
// account took (oidc_pending) and the one write a sign-in makes (OIDCSignIn),
// which reads the accounts, asks oidc.Decide and applies its answer in one
// transaction, so no change between the read and the write can link a sign-in
// to an account that has stopped being eligible.

// How an identity was linked (user_identities.link_method).
const (
	LinkAutoEmail = "auto_email"
	LinkAdmin     = "admin"
	LinkCreated   = "created"
)

// Errors from the identity routes.
var (
	// ErrIdentityLinked: the sign-in's (issuer, subject) is already linked to an
	// account (another admin linked it, or it signed in, meanwhile).
	ErrIdentityLinked = errors.New("that sign-in is already linked to an account")
	// ErrUserHasIdentity: the account already has an identity from this issuer.
	ErrUserHasIdentity = errors.New("the account is already linked to an account at this provider")
	// ErrNotLinkable: a household member or a demo account is never linked.
	ErrNotLinkable = errors.New("household members and demo accounts can't sign in with single sign-on")
	// ErrInvalidEmail: an email that isn't one.
	ErrInvalidEmail = errors.New("enter an email address like name@example.org, or leave it empty")
)

// Pending sign-in bounds: rows kept, days kept, groups kept per row.
const (
	maxPendingSignIns = 500
	pendingDays       = 30
	maxPendingGroups  = 50
)

// MaxEmail bounds an account's email, in bytes: a sign-in's bound.
const MaxEmail = oidc.MaxEmail

// NormalizeEmail is an email as stored: trimmed, "" for none, else something@
// something with no space or control character, at most MaxEmail bytes
// (ErrInvalidEmail otherwise). Case is kept; matching folds it.
func NormalizeEmail(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	at := strings.LastIndexByte(s, '@')
	if len(s) > MaxEmail || at <= 0 || at == len(s)-1 || strings.IndexFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return "", ErrInvalidEmail
	}
	return s, nil
}

// Identity is an account's identity at a provider, as the console shows it.
type Identity struct {
	ID           int64    `json:"id"`
	Issuer       string   `json:"issuer"`
	Subject      string   `json:"subject"`
	Email        string   `json:"email"`
	Username     string   `json:"username"`
	LinkMethod   string   `json:"link_method"`
	LinkedBy     *UserRef `json:"linked_by"`
	LinkedAt     string   `json:"linked_at"`
	LastSignInAt *string  `json:"last_sign_in_at"`
}

// PendingSignIn is a sign-in no account took, waiting for an admin.
type PendingSignIn struct {
	ID            int64    `json:"id"`
	Issuer        string   `json:"issuer"`
	Subject       string   `json:"subject"`
	Email         string   `json:"email"`
	EmailVerified bool     `json:"email_verified"`
	Username      string   `json:"username"`
	Name          string   `json:"name"`
	Groups        []string `json:"groups"`
	Reason        string   `json:"reason"`
	FirstSeenAt   string   `json:"first_seen_at"`
	LastSeenAt    string   `json:"last_seen_at"`
	Attempts      int      `json:"attempts"`
}

// RoleSync is what the admin_group rule did at a sign-in.
type RoleSync struct {
	From, To string
	// Refused: the demotion would have left no enabled admin (ErrLastAdmin), so
	// the role stays From.
	Refused bool
}

// Changed reports whether the role changed.
func (r RoleSync) Changed() bool { return r.From != r.To && !r.Refused }

// OIDCResult is a sign-in's outcome.
type OIDCResult struct {
	Decision oidc.Decision
	// User is the account signing in (SignIn, Link, Create), else nil.
	User *User
	// Role is set when the admin_group rule applied (Decision.Role).
	Role *RoleSync
	// PendingID is the waiting row a Pending outcome recorded or refreshed
	// (its id stays the same across the person's attempts), else 0.
	PendingID int64
	// PendingEvicted is how many of the oldest waiting rows a Pending outcome
	// dropped to stay within maxPendingSignIns (the callback tells the admin).
	PendingEvicted int64
	// GroupsSeen: the linked identity had carried a groups claim before this
	// sign-in (user_identities.groups_seen), so a sign-in without one is the
	// provider leaving it out now (Decision.NoGroups says the role stayed).
	GroupsSeen bool
	// Token is the single-use token (KindOIDC, tokenTTL long) an outcome that
	// signs in minted in the sign-in's own transaction, so a sign-out
	// everywhere can't land between the decision and the token and miss it.
	// "" otherwise.
	Token string
}

// accountFacts reads an account's oidc.Account facts; ?1 is the issuer.
const accountFacts = `u.id, u.role = '` + RoleAdmin + `', u.disabled, u.is_demo, u.managed_by IS NOT NULL,
	EXISTS(SELECT 1 FROM user_identities li WHERE li.user_id = u.id AND li.issuer = ?1)`

// scanAccount scans accountFacts, then extra (columns the query adds after).
func scanAccount(row interface{ Scan(...any) error }, extra ...any) (oidc.Account, error) {
	var a oidc.Account
	err := row.Scan(append([]any{&a.ID, &a.Admin, &a.Disabled, &a.Demo, &a.Member, &a.Linked}, extra...)...)
	return a, err
}

// OIDCSignIn applies a verified sign-in under policy, in one durable
// transaction: the account linked to (issuer, subject), else the accounts with
// its email (any state: oidc.Decide sets the ineligible aside), oidc.Decide's
// answer, then its write. SignIn stamps the identity's last sign-in and what it
// says now, and marks it groups_seen when the sign-in carried the groups claim
// (even an empty list; read only for the callback's warning when a later
// sign-in carries none, OIDCResult.GroupsSeen); Link adds
// the identity (auto_email); Create makes an ordinary account (its username
// from the claims, numbered on a clash; the email only when verified) and its
// identity, both dropping the row the sign-in waited in; Pending records or
// refreshes the waiting row (capped, pruned; one refreshed by partial claims
// keeps the username, name and groups it lacks now). An outcome that signs in
// applies the role rule: a demotion that would leave no enabled admin is
// refused and reported, never an error. An outcome that signs in also mints the single-use token the
// browser finishes with (KindOIDC, tokenTTL long, the only token ConsumeOIDC
// takes), in the same transaction (OIDCResult.Token).
func (s *Service) OIDCSignIn(ctx context.Context, c oidc.Claims, p oidc.Policy, tokenTTL time.Duration) (OIDCResult, error) {
	if tokenTTL <= 0 { // issueToken's no-expiry: never for a token a browser carries
		return OIDCResult{}, errors.New("auth: OIDCSignIn needs a token life")
	}
	var res OIDCResult
	err := s.db.WithDurableTx(ctx, "OIDCSignIn", func(tx *sql.Tx) error {
		res = OIDCResult{}
		in := oidc.Input{Claims: c, Policy: p}
		a, err := scanAccount(tx.QueryRowContext(ctx, `SELECT `+accountFacts+`, i.groups_seen
			  FROM user_identities i JOIN users u ON u.id = i.user_id
			 WHERE i.issuer = ?1 AND i.subject = ?2`, c.Issuer, c.Subject), &res.GroupsSeen)
		switch {
		case err == nil:
			in.Linked = &a
		case !errors.Is(err, sql.ErrNoRows):
			return err
		case c.Email != "":
			if in.Matches, err = matchEmail(ctx, tx, c.Issuer, c.Email); err != nil {
				return err
			}
		}
		d := oidc.Decide(in)
		res.Decision = d
		now := s.ts()
		id := d.Account
		switch d.Outcome {
		case oidc.SignIn:
			// Partial claims (userinfo unreadable) keep the email and username
			// stored for a claim they lack: it may be the person's still.
			if _, err := tx.ExecContext(ctx, `UPDATE user_identities SET last_sign_in_at = ?1,
				email    = CASE WHEN ?2 = '' AND ?4 THEN email ELSE ?2 END,
				username = CASE WHEN ?3 = '' AND ?4 THEN username ELSE ?3 END,
				groups_seen = groups_seen OR ?5 WHERE issuer = ?6 AND subject = ?7`,
				now, c.Email, c.Username, c.Partial, c.Groups != nil, c.Issuer, c.Subject); err != nil {
				return err
			}
		case oidc.Link:
			if err := insertIdentity(ctx, tx, id, c, LinkAutoEmail, nil, now); err != nil {
				return err
			}
			if err := dropPending(ctx, tx, c); err != nil {
				return err
			}
		case oidc.Create:
			if id, err = createForIdentity(ctx, tx, c, "", now); err != nil {
				return err
			}
			if err := insertIdentity(ctx, tx, id, c, LinkCreated, nil, now); err != nil {
				return err
			}
			if err := dropPending(ctx, tx, c); err != nil {
				return err
			}
		case oidc.Pending:
			res.PendingID, res.PendingEvicted, err = upsertPending(ctx, tx, c, d.Reason, now, s.now())
			return err
		default:
			return nil
		}
		if d.Role != "" {
			rs, err := syncRole(ctx, tx, id, d.Role, now)
			if err != nil {
				return err
			}
			res.Role = &rs
		}
		if res.User, err = getUser(ctx, tx, id); err != nil {
			return err
		}
		res.Token, err = s.issueToken(ctx, tx, id, KindOIDC, "", tokenTTL)
		return err
	})
	return res, err
}

// ConsumeOIDC trades a token OIDCSignIn minted (KindOIDC) for its account and
// the device's session (named deviceName), spent and minted in one transaction
// (exchangeSession: two racing finishes cannot both win, and a Sign out
// everywhere or a password change committing alongside never leaves a session
// it missed). Any other token (a pairing token, a session) is ErrInvalidToken
// and left as it was, as is a spent or expired one, or one whose account is
// disabled or is now a household member or a demo account (which single
// sign-on never signs in; leaveOwners ends the open token of an account
// joining a household, so this is the second check, and the one for a demo
// account; the transaction asks it again of the account as it reads it).
func (s *Service) ConsumeOIDC(ctx context.Context, secret, deviceName string) (u *User, session string, err error) {
	hash := hashSecret(secret)
	u, _, err = s.lookupToken(ctx, hash, KindOIDC)
	if err != nil {
		return nil, "", err
	}
	if !u.SSOAllowed() {
		return nil, "", ErrInvalidToken
	}
	return s.exchangeSession(ctx, "ConsumeOIDC", u.ID, deviceName, func(tx *sql.Tx, now string) error {
		return spendToken(ctx, tx, hash, KindOIDC, now, true)
	}, (*User).SSOAllowed)
}

// matchEmail reads every account whose email is email (both trimmed; SQLite's
// NOCASE folds ASCII case only), as oidc.Account facts.
func matchEmail(ctx context.Context, tx *sql.Tx, issuer, email string) ([]oidc.Account, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+accountFacts+` FROM users u
		 WHERE u.email = ?2 COLLATE NOCASE AND u.email <> '' ORDER BY u.id`, issuer, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []oidc.Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// insertIdentity links (c.Issuer, c.Subject) to account id, linked by admin by
// (nil: by the sign-in itself, which then counts as the link's first sign-in
// and marks groups_seen when it carried the groups claim; a waiting row's
// stored list can't tell an absent claim from an empty one, so an admin's
// link marks it only for a row that holds a group, and leaves an empty one
// for the identity's first sign-in).
func insertIdentity(ctx context.Context, tx *sql.Tx, id int64, c oidc.Claims, method string, by *int64, now string) error {
	var last any
	if by == nil {
		last = now
	}
	seen := c.Groups != nil && (by == nil || len(c.Groups) > 0)
	_, err := tx.ExecContext(ctx, `INSERT INTO user_identities(user_id, issuer, subject, email, username, link_method, linked_by, linked_at,
		    last_sign_in_at, groups_seen)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, id, c.Issuer, c.Subject, c.Email, c.Username, method, by, now, last, seen)
	if store.IsUniqueViolation(err) {
		var n int
		if qerr := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_identities WHERE issuer = ? AND subject = ?`,
			c.Issuer, c.Subject).Scan(&n); qerr == nil && n > 0 {
			return ErrIdentityLinked
		}
		return ErrUserHasIdentity
	}
	return err
}

// dropPending removes c's waiting row, if it has one: a sign-in that waited
// (no account matched then) and is now linked or given an account by a sign-in
// itself is no longer the console's to link, create or dismiss.
func dropPending(ctx context.Context, tx *sql.Tx, c oidc.Claims) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM oidc_pending WHERE issuer = ? AND subject = ?`, c.Issuer, c.Subject)
	return err
}

// createForIdentity makes an ordinary, password-less account for c: username
// as given (an admin's choice: ErrUsernameTaken on a clash), else derived from
// the claims (the username claim, else the email's local part, else the name,
// folded by usernameSlug, so never demo_; numbered on a clash; "user" when
// nothing is left). A verified email is copied onto it.
func createForIdentity(ctx context.Context, tx *sql.Tx, c oidc.Claims, username, now string) (int64, error) {
	var err error
	if username == "" {
		if username, err = freeUsername(ctx, tx, identityUsernameBase(c), ""); err != nil {
			return 0, err
		}
	}
	email := ""
	if c.EmailVerified {
		email, _ = NormalizeEmail(c.Email) // an address that isn't one is left off
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO users(username, password_hash, role, email, created_at, updated_at)
		VALUES(?, '', ?, ?, ?, ?)`, username, RoleUser, email, now, now)
	if store.IsUniqueViolation(err) {
		return 0, ErrUsernameTaken
	}
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// identityUsernameBase is the base of a new account's username for c.
func identityUsernameBase(c oidc.Claims) string {
	local, _, _ := strings.Cut(c.Email, "@")
	for _, s := range []string{c.Username, local, c.Name} {
		if base := clipBase(usernameSlug(s)); base != "" {
			return base
		}
	}
	return "user"
}

// syncRole gives account id role (the admin_group rule). A demotion that would
// leave no enabled admin is refused (lastAdminGuard) and reported in the
// answer, not as an error: the sign-in goes on with the role it had.
func syncRole(ctx context.Context, tx *sql.Tx, id int64, role, now string) (RoleSync, error) {
	var cur string
	if err := tx.QueryRowContext(ctx, `SELECT role FROM users WHERE id = ?`, id).Scan(&cur); err != nil {
		return RoleSync{}, err
	}
	rs := RoleSync{From: cur, To: role}
	if cur == role {
		return rs, nil
	}
	if cur == RoleAdmin {
		if err := lastAdminGuard(ctx, tx, id); errors.Is(err, ErrLastAdmin) {
			rs.Refused = true
			return rs, nil
		} else if err != nil {
			return RoleSync{}, err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE users SET role = ?, updated_at = ? WHERE id = ?`, role, now, id)
	return rs, err
}

// upsertPending records or refreshes c's waiting row with reason, returning its
// id and how many rows the cap evicted. Rows unseen for pendingDays go first,
// c's own among them: ListPending no longer shows such a row, so the attempt
// records a new one (a new id, which the callback tells the admins about)
// rather than bringing back one they were told about long ago. A row refreshed
// by partial claims (userinfo unreadable) keeps the username, the name and the
// groups those lack: they may be the person's still, as on a linked identity.
// Then the oldest past maxPendingSignIns go, never the row just written (it is
// the newest, unless the clock stepped back past rows stamped later: excluded
// by id all the same, so the id returned always names a row).
func upsertPending(ctx context.Context, tx *sql.Tx, c oidc.Claims, reason, now string, at time.Time) (int64, int64, error) {
	groups := c.Groups
	if len(groups) > maxPendingGroups {
		groups = groups[:maxPendingGroups]
	}
	if groups == nil {
		groups = []string{}
	}
	g, err := json.Marshal(groups)
	if err != nil {
		return 0, 0, err
	}
	cutoff := at.AddDate(0, 0, -pendingDays).UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `DELETE FROM oidc_pending WHERE last_seen_at < ?`, cutoff); err != nil {
		return 0, 0, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO oidc_pending(issuer, subject, email, email_verified, username, name, groups,
		    reason, first_seen_at, last_seen_at, attempts)
		VALUES(?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?9, 1)
		ON CONFLICT(issuer, subject) DO UPDATE SET email = excluded.email, email_verified = excluded.email_verified,
		    username = CASE WHEN excluded.username = '' AND ?10 THEN username ELSE excluded.username END,
		    name     = CASE WHEN excluded.name = '' AND ?10 THEN name ELSE excluded.name END,
		    groups   = CASE WHEN ?11 THEN groups ELSE excluded.groups END, reason = excluded.reason,
		    last_seen_at = excluded.last_seen_at, attempts = attempts + 1
		RETURNING id`,
		c.Issuer, c.Subject, c.Email, c.EmailVerified, c.Username, c.Name, string(g), reason, now,
		c.Partial, c.Partial && c.Groups == nil).Scan(&id); err != nil {
		return 0, 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM oidc_pending WHERE id <> ? AND id IN
		(SELECT id FROM oidc_pending ORDER BY last_seen_at DESC, id DESC LIMIT -1 OFFSET ?)`, id, maxPendingSignIns)
	if err != nil {
		return 0, 0, err
	}
	evicted, err := res.RowsAffected()
	return id, evicted, err
}

const pendingColumns = `id, issuer, subject, email, email_verified, username, name, groups, reason, first_seen_at, last_seen_at, attempts`

func scanPending(row interface{ Scan(...any) error }) (PendingSignIn, error) {
	var (
		p      PendingSignIn
		groups string
	)
	err := row.Scan(&p.ID, &p.Issuer, &p.Subject, &p.Email, &p.EmailVerified, &p.Username, &p.Name, &groups, &p.Reason,
		&p.FirstSeenAt, &p.LastSeenAt, &p.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	if json.Unmarshal([]byte(groups), &p.Groups) != nil || p.Groups == nil {
		p.Groups = []string{}
	}
	return p, nil
}

// claims are the pending row's facts as a sign-in's.
func (p PendingSignIn) claims() oidc.Claims {
	return oidc.Claims{Issuer: p.Issuer, Subject: p.Subject, Email: p.Email, EmailVerified: p.EmailVerified,
		Username: p.Username, Name: p.Name, Groups: p.Groups}
}

// ListPending lists the waiting sign-ins seen in the last pendingDays, the
// newest first.
func (s *Service) ListPending(ctx context.Context) ([]PendingSignIn, error) {
	cutoff := s.now().AddDate(0, 0, -pendingDays).UTC().Format(time.RFC3339)
	rows, err := s.db.QueryContext(ctx, `SELECT `+pendingColumns+` FROM oidc_pending WHERE last_seen_at >= ?
		ORDER BY last_seen_at DESC, id DESC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PendingSignIn{}
	for rows.Next() {
		p, err := scanPending(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LinkedAccounts maps each issuer a waiting sign-in came from to the accounts
// that already have an identity from it (an account holds at most one per
// issuer, so the console's Link leaves them out of its choices).
func (s *Service) LinkedAccounts(ctx context.Context) (map[string][]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT issuer, user_id FROM user_identities
		 WHERE issuer IN (SELECT issuer FROM oidc_pending) ORDER BY issuer, user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]int64{}
	for rows.Next() {
		var (
			iss string
			id  int64
		)
		if err := rows.Scan(&iss, &id); err != nil {
			return nil, err
		}
		out[iss] = append(out[iss], id)
	}
	return out, rows.Err()
}

// takePending removes waiting row id inside tx and returns it (ErrNotFound
// when gone); a refusal later in tx rolls the removal back.
func takePending(ctx context.Context, tx *sql.Tx, id int64) (PendingSignIn, error) {
	return scanPending(tx.QueryRowContext(ctx, `DELETE FROM oidc_pending WHERE id = ? RETURNING `+pendingColumns, id))
}

// LinkPending is the console's Link: waiting row id's identity linked to
// account userID by admin by (link_method admin), the row removed. Refused for
// a gone row or account (ErrNotFound), a household member or demo account
// (ErrNotLinkable), an account with an identity from this issuer
// (ErrUserHasIdentity) and an identity linked meanwhile (ErrIdentityLinked).
// An admin's account may be linked here: the admin decides (decision 4 is about
// linking by email alone).
func (s *Service) LinkPending(ctx context.Context, id, userID, by int64) (PendingSignIn, *User, error) {
	var (
		p PendingSignIn
		u *User
	)
	err := s.db.WithDurableTx(ctx, "LinkPending", func(tx *sql.Tx) error {
		var err error
		if p, err = takePending(ctx, tx, id); err != nil {
			return err
		}
		if u, err = getUser(ctx, tx, userID); err != nil {
			return err
		}
		if !u.SSOAllowed() {
			return ErrNotLinkable
		}
		return insertIdentity(ctx, tx, userID, p.claims(), LinkAdmin, &by, s.ts())
	})
	return p, u, err
}

// CreateFromPending is the console's Create: an ordinary account for waiting
// row id (username as given, else derived as a sign-in's), its identity
// (link_method created, linked by by), the row removed. ErrNotFound for a gone
// row, ErrUsernameTaken for a given name an account has, ErrIdentityLinked for
// an identity linked meanwhile.
func (s *Service) CreateFromPending(ctx context.Context, id int64, username string, by int64) (PendingSignIn, *User, error) {
	var (
		p PendingSignIn
		u *User
	)
	username = strings.TrimSpace(username)
	err := s.db.WithDurableTx(ctx, "CreateFromPending", func(tx *sql.Tx) error {
		var err error
		if p, err = takePending(ctx, tx, id); err != nil {
			return err
		}
		now := s.ts()
		uid, err := createForIdentity(ctx, tx, p.claims(), username, now)
		if err != nil {
			return err
		}
		if err := insertIdentity(ctx, tx, uid, p.claims(), LinkCreated, &by, now); err != nil {
			return err
		}
		u, err = getUser(ctx, tx, uid)
		return err
	})
	return p, u, err
}

// DismissPending removes waiting row id, returning it (ErrNotFound when gone).
// The person's next sign-in records it again.
func (s *Service) DismissPending(ctx context.Context, id int64) (PendingSignIn, error) {
	var p PendingSignIn
	err := s.db.WithTx(ctx, "DismissPending", func(tx *sql.Tx) error {
		var err error
		p, err = takePending(ctx, tx, id)
		return err
	})
	return p, err
}

const identityColumns = `i.id, i.issuer, i.subject, i.email, i.username, i.link_method, i.linked_by, b.username,
	i.linked_at, i.last_sign_in_at`

func scanIdentity(row interface{ Scan(...any) error }) (Identity, error) {
	var (
		i      Identity
		byID   sql.NullInt64
		byName sql.NullString
		last   sql.NullString
	)
	if err := row.Scan(&i.ID, &i.Issuer, &i.Subject, &i.Email, &i.Username, &i.LinkMethod, &byID, &byName,
		&i.LinkedAt, &last); err != nil {
		return i, err
	}
	if byID.Valid {
		i.LinkedBy = &UserRef{ID: byID.Int64, Username: byName.String}
	}
	if last.Valid {
		i.LastSignInAt = &last.String
	}
	return i, nil
}

// ListIdentities lists account userID's identities.
func (s *Service) ListIdentities(ctx context.Context, userID int64) ([]Identity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+identityColumns+`
		  FROM user_identities i LEFT JOIN users b ON b.id = i.linked_by
		 WHERE i.user_id = ? ORDER BY i.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Identity{}
	for rows.Next() {
		i, err := scanIdentity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// UnlinkIdentity removes identity identityID from account userID, returning
// it: ErrNotFound when the account has no such identity, ErrAdminNeedsPassword
// when it is a password-less admin's way in (decision 5): an identity from the
// single sign-on that can run now (activeIssuer; an account has at most one
// per issuer). One from another provider, or while it is off, is no way in.
// The account's sign-ins not yet finished (KindOIDC tokens) end with it.
func (s *Service) UnlinkIdentity(ctx context.Context, userID, identityID int64) (Identity, error) {
	var out Identity
	err := s.db.WithDurableTx(ctx, "UnlinkIdentity", func(tx *sql.Tx) error {
		var err error
		out, err = scanIdentity(tx.QueryRowContext(ctx, `SELECT `+identityColumns+`
			  FROM user_identities i LEFT JOIN users b ON b.id = i.linked_by
			 WHERE i.id = ? AND i.user_id = ?`, identityID, userID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if iss := s.activeIssuer(); iss != "" && out.Issuer == iss {
			var stranded bool
			if err := tx.QueryRowContext(ctx, `SELECT role = ? AND password_hash = '' FROM users WHERE id = ?`,
				RoleAdmin, userID).Scan(&stranded); err != nil {
				return err
			}
			if stranded {
				return ErrAdminNeedsPassword
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_identities WHERE id = ?`, identityID); err != nil {
			return err
		}
		// A sign-in through it the callback decided and the browser hasn't
		// finished yet ends with it, as a sign-out everywhere ends one.
		_, err = tx.ExecContext(ctx, `UPDATE tokens SET revoked = 1 WHERE user_id = ? AND kind = ? AND revoked = 0`,
			userID, KindOIDC)
		return err
	})
	return out, err
}

// hasSignInIdentity reports whether account id has an identity from the
// single sign-on that can run now (activeIssuer): a way into the console
// besides a password (decision 5). None counts while it is off or
// misconfigured, nor one from a provider no longer configured.
func (s *Service) hasSignInIdentity(ctx context.Context, q rowQuerier, id int64) (bool, error) {
	iss := s.activeIssuer()
	if iss == "" {
		return false, nil
	}
	var ok bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_identities WHERE user_id = ? AND issuer = ?)`, id, iss).Scan(&ok)
	return ok, err
}

// SSOAccounts are the accounts holding an identity from the single sign-on
// that can run now (activeIssuer), the console's "signs in with" (server#408);
// nil while none can. Whether one may use it (ConsumeOIDC refuses a household
// member or a demo account) is the caller's to say: the account is at hand.
func (s *Service) SSOAccounts(ctx context.Context) (map[int64]bool, error) {
	iss := s.activeIssuer()
	if iss == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT user_id FROM user_identities WHERE issuer = ?`, iss)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// HasSSO reports whether account id holds an identity from the single sign-on
// that can run now (hasSignInIdentity): SSOAccounts for one account.
func (s *Service) HasSSO(ctx context.Context, id int64) (bool, error) {
	return s.hasSignInIdentity(ctx, s.db, id)
}
