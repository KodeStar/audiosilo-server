// Package auth manages users, opaque session tokens, redeemable auth codes and
// the QR pairing payload used by the "enter your auth code" connect flow.
package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/store"
)

// Roles.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// Token kinds.
const (
	KindSession = "session"
	KindPairing = "pairing"
	// KindAPI is a user-minted, non-expiring personal access token ("API key")
	// for headless integrations (dashboards, cron). It authenticates exactly like
	// a session token - anywhere requireAuth accepts a session it accepts an api
	// key, acting as its owner - but it is never valid for pairing exchange, and
	// its lifecycle is mint/list/revoke rather than sign-in/sign-out.
	KindAPI = "api"
)

// Auth code kinds. An invite is an admin-minted onboarding secret (bounded uses
// and lifetime); a recovery code is a durable, reusable credential the user holds
// to re-authenticate themselves after signing out or losing a device - so
// recovery never needs an admin to mint a fresh invite. Both redeem through the
// same path; only their ownership and lifetime differ.
const (
	CodeInvite   = "invite"
	CodeRecovery = "recovery"
)

// DemoUsernamePrefix marks throwaway accounts created by public demo mode. The
// demo session endpoint creates `demo_<random>` users and the background reaper
// deletes idle ones by this prefix.
const DemoUsernamePrefix = "demo_"

// Errors returned by the service.
var (
	ErrNotFound     = errors.New("not found")
	ErrInvalidCreds = errors.New("invalid credentials")
	ErrInvalidToken = errors.New("invalid or expired token")
	ErrInvalidCode  = errors.New("invalid or expired auth code")
	// ErrCodeExhausted is returned by ConsumePairingToken when the parent invite
	// has no uses left, so the transport can tell "the invite is spent" apart
	// from a bogus token.
	ErrCodeExhausted = errors.New("invite has no uses left")
	// ErrCodeExpired is returned by ConsumePairingToken when the parent invite
	// expired between redeem and exchange.
	ErrCodeExpired = errors.New("invite has expired")
	// ErrLastAdmin is returned when an operation would leave no enabled admin.
	ErrLastAdmin = errors.New("cannot remove the last admin")
	// ErrAdminNeedsPassword is returned when an account would become (or remain)
	// an admin without a password to sign in to the console.
	ErrAdminNeedsPassword = errors.New("admin accounts require a password")
	// ErrPasswordTooShort is returned when a non-empty password is below the
	// minimum length.
	ErrPasswordTooShort = errors.New("password must be at least 8 characters")
	// ErrUsernameTaken is returned when creating a user whose username already
	// exists, so the transport layer can map it to 409 instead of echoing the raw
	// SQLite unique-constraint string.
	ErrUsernameTaken = errors.New("username already taken")
)

// MinPasswordLen is the minimum length for a non-empty account password.
const MinPasswordLen = 8

// validatePassword enforces the minimum length for a non-empty password. An
// empty password is permitted here (it means "password-less" for non-admins, or
// "clear"); the admin-needs-a-password rule is enforced separately by callers.
func validatePassword(password string) error {
	if password != "" && len(password) < MinPasswordLen {
		return ErrPasswordTooShort
	}
	return nil
}

// User is an account record (without the password hash for callers).
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	Disabled bool   `json:"disabled"`
	// HasPassword reports whether the account can sign in with a password. It is
	// false for password-less accounts (non-admins onboarded purely via auth-code
	// pairing); such accounts never satisfy Authenticate.
	HasPassword bool `json:"has_password"`
	// HasRecovery reports whether the user holds a durable recovery code they can
	// use to re-authenticate without an admin. Drives the "you have no way back
	// in" warning shown at sign-out.
	HasRecovery bool `json:"has_recovery"`
	// IsDemo marks a throwaway demo account. Self-service password/recovery are
	// refused for demo accounts so a public demo can't be turned into a durable
	// login that outlives the idle reaper.
	IsDemo bool `json:"is_demo"`
	// LastSeenAt is the RFC3339 time of the user's most recent authenticated API
	// activity, derived from the newest tokens.last_seen across their tokens
	// (empty if they have never made an authenticated request).
	LastSeenAt string `json:"last_seen_at,omitempty"`
}

// Service provides authentication and account operations backed by the store.
type Service struct {
	db  *store.DB
	now func() time.Time
}

// New returns a Service. now may be nil to use time.Now.
func New(db *store.DB, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{db: db, now: now}
}

func (s *Service) ts() string { return s.now().UTC().Format(time.RFC3339) }

// CreateUser creates an account and returns it. A password is required for
// admins (who sign in to the console); non-admins may be created password-less
// (password == ""), in which case they onboard purely via auth-code pairing and
// can never password-login. An empty password is stored as an empty hash, never
// a hash of "".
func (s *Service) CreateUser(ctx context.Context, username, password, role string) (*User, error) {
	return s.createUser(ctx, username, password, role, false)
}

// CreateDemoUser creates a password-less, non-admin throwaway account flagged
// is_demo so the background reaper can sweep idle ones by flag (never by username
// prefix, which would catch real accounts an admin named "demo_*").
func (s *Service) CreateDemoUser(ctx context.Context, username string) (*User, error) {
	return s.createUser(ctx, username, "", RoleUser, true)
}

func (s *Service) createUser(ctx context.Context, username, password, role string, isDemo bool) (*User, error) {
	if username == "" {
		return nil, errors.New("username is required")
	}
	if role != RoleAdmin {
		role = RoleUser
	}
	if password == "" && role == RoleAdmin {
		return nil, ErrAdminNeedsPassword
	}
	if err := validatePassword(password); err != nil {
		return nil, err
	}
	hash := ""
	if password != "" {
		var err error
		if hash, err = HashPassword(password); err != nil {
			return nil, err
		}
	}
	now := s.ts()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users(username, password_hash, role, is_demo, created_at, updated_at)
		 VALUES(?,?,?,?,?,?)`, username, hash, role, isDemo, now, now)
	if err != nil {
		if store.IsUniqueViolation(err) {
			return nil, ErrUsernameTaken
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &User{ID: id, Username: username, Role: role, HasPassword: hash != "", IsDemo: isDemo}, nil
}

// AdminExists reports whether at least one admin account is present.
func (s *Service) AdminExists(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE role = ?`, RoleAdmin).Scan(&n)
	return n > 0, err
}

// Authenticate verifies a username/password and returns the user.
func (s *Service) Authenticate(ctx context.Context, username, password string) (*User, error) {
	var (
		u    User
		hash string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, role, disabled, password_hash FROM users WHERE username = ?`,
		username).Scan(&u.ID, &u.Username, &u.Role, &u.Disabled, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		// Run a dummy verify to keep timing roughly constant for unknown users.
		_, _ = VerifyPassword(password, dummyHash)
		return nil, ErrInvalidCreds
	}
	if err != nil {
		return nil, err
	}
	if hash == "" {
		// Password-less account (auth-code pairing only): no password can match.
		_, _ = VerifyPassword(password, dummyHash)
		return nil, ErrInvalidCreds
	}
	ok, err := VerifyPassword(password, hash)
	if err != nil || !ok {
		return nil, ErrInvalidCreds
	}
	if u.Disabled {
		return nil, ErrInvalidCreds
	}
	return &u, nil
}

// dummyHash is a precomputed argon2id hash used to equalize timing for unknown
// usernames. It corresponds to a random password no one knows. Its cost
// parameters mirror the real ones (argonTime/argonMemory in hash.go) so the
// dummy verify does the same work and doesn't leak account existence by timing.
const dummyHash = "$argon2id$v=19$m=65536,t=2,p=4$YWFhYWFhYWFhYWFhYWFhYQ$3KhZ0r0u2y0xq8b9oQzWtkqj9o0a9hZ0r0u2y0xq8b8"

// IssueToken creates a token of the given kind for a user and returns the
// secret (shown once). ttl <= 0 means no expiry. Tokens minted here are
// unlinked (no parent auth code); pairing tokens derived from a redeemed code
// go through IssuePairingToken instead.
func (s *Service) IssueToken(ctx context.Context, userID int64, kind, deviceName string, ttl time.Duration) (string, error) {
	secret, hash, err := generateToken()
	if err != nil {
		return "", err
	}
	if _, err := s.insertToken(ctx, userID, hash, kind, deviceName, s.expiresAt(ttl), nil); err != nil {
		return "", err
	}
	return secret, nil
}

// insertToken writes one tokens row and returns the write Result (for callers
// that need the new row's id) - the single place the column list lives. expires
// and authCodeID are nil for "no expiry" / "unlinked".
func (s *Service) insertToken(ctx context.Context, userID int64, hash, kind, deviceName string, expires, authCodeID any) (sql.Result, error) {
	return s.db.ExecContext(ctx,
		`INSERT INTO tokens(user_id, token_hash, kind, device_name, created_at, expires_at, auth_code_id)
		 VALUES(?,?,?,?,?,?,?)`, userID, hash, kind, deviceName, s.ts(), expires, authCodeID)
}

// ResolveToken validates a presented token secret of exactly the given kind and
// returns its user, also bumping last_seen. Revoked/expired tokens return
// ErrInvalidToken.
func (s *Service) ResolveToken(ctx context.Context, secret, kind string) (*User, error) {
	return s.ResolveTokenKinds(ctx, secret, kind)
}

// ResolveTokenKinds is ResolveToken generalized over several accepted kinds: it
// validates a presented secret whose kind is one of kinds, bumps last_seen and
// returns the user. Middleware uses it to accept a session OR an api key on the
// same route (an api key acts as its owner), while never accepting a pairing
// token there. At least one kind must be supplied.
func (s *Service) ResolveTokenKinds(ctx context.Context, secret string, kinds ...string) (*User, error) {
	hash := hashSecret(secret)
	u, _, err := s.lookupToken(ctx, hash, kinds...)
	if err != nil {
		return nil, err
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE tokens SET last_seen = ? WHERE token_hash = ?`, s.ts(), hash)
	return u, nil
}

// lookupToken resolves a token hash whose kind is one of the accepted kinds to
// its (partial) user and parent auth-code link, applying the shared validity
// checks: unknown, revoked and expired tokens and disabled users all return
// ErrInvalidToken. The single definition of "is this token valid", shared by
// ResolveToken/ResolveTokenKinds (every authenticated request) and
// ConsumePairingToken (exchange). token_hash is globally unique, so the kind
// filter only rejects a real token presented where its kind is not accepted.
func (s *Service) lookupToken(ctx context.Context, hash string, kinds ...string) (*User, sql.NullInt64, error) {
	var (
		u       User
		expires sql.NullString
		revoked bool
		codeID  sql.NullInt64
	)
	args := make([]any, 0, len(kinds)+1)
	args = append(args, hash)
	for _, k := range kinds {
		args = append(args, k)
	}
	err := s.db.QueryRowContext(ctx,
		`SELECT u.id, u.username, u.role, u.disabled, t.expires_at, t.revoked, t.auth_code_id
		   FROM tokens t JOIN users u ON u.id = t.user_id
		  WHERE t.token_hash = ? AND t.kind IN (`+inPlaceholders(len(kinds))+`)`, args...).
		Scan(&u.ID, &u.Username, &u.Role, &u.Disabled, &expires, &revoked, &codeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, codeID, ErrInvalidToken
	}
	if err != nil {
		return nil, codeID, err
	}
	if revoked || u.Disabled || s.pastExpiry(expires) {
		return nil, codeID, ErrInvalidToken
	}
	return &u, codeID, nil
}

// inPlaceholders returns "?,?,..." with n placeholders for a SQL IN clause.
func inPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return "?" + strings.Repeat(",?", n-1)
}

// RevokeToken revokes a token by its secret.
func (s *Service) RevokeToken(ctx context.Context, secret string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET revoked = 1 WHERE token_hash = ?`, hashSecret(secret))
	return err
}

// APIToken describes a user-minted API key for display. The plaintext secret is
// never included - only its SHA-256 hash is stored, by design - so this carries
// the key's metadata only. LastSeen is nil until the key has authenticated a
// request (rendered as JSON null).
type APIToken struct {
	ID        int64   `json:"id"`
	Label     string  `json:"label"`
	CreatedAt string  `json:"created_at"`
	LastSeen  *string `json:"last_seen"`
}

// keyMetaColumns is the column list backing APIToken (device_name is the key's
// user-facing label). Shared by IssueAPIToken's read-back and ListAPITokens so
// the shape stays in one place.
const keyMetaColumns = `id, device_name, created_at, last_seen`

func scanAPIToken(row interface{ Scan(...any) error }) (APIToken, error) {
	var (
		t        APIToken
		lastSeen sql.NullString
	)
	if err := row.Scan(&t.ID, &t.Label, &t.CreatedAt, &lastSeen); err != nil {
		return t, err
	}
	if lastSeen.Valid {
		t.LastSeen = &lastSeen.String
	}
	return t, nil
}

// IssueAPIToken mints a personal API key (kind=api, no expiry) for a user and
// returns the plaintext secret (shown once) plus the stored row's metadata for
// the create response. The secret is stored only as a hash. label is carried in
// device_name; callers trim/validate it before calling.
func (s *Service) IssueAPIToken(ctx context.Context, userID int64, label string) (string, APIToken, error) {
	secret, hash, err := generateToken()
	if err != nil {
		return "", APIToken{}, err
	}
	res, err := s.insertToken(ctx, userID, hash, KindAPI, label, nil, nil)
	if err != nil {
		return "", APIToken{}, err
	}
	id, _ := res.LastInsertId()
	meta, err := scanAPIToken(s.db.QueryRowContext(ctx,
		`SELECT `+keyMetaColumns+` FROM tokens WHERE id = ?`, id))
	if err != nil {
		return "", APIToken{}, err
	}
	return secret, meta, nil
}

// ListAPITokens returns a user's live (non-revoked) API keys, newest first, as
// metadata only (never a secret or hash).
func (s *Service) ListAPITokens(ctx context.Context, userID int64) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+keyMetaColumns+` FROM tokens
		  WHERE user_id = ? AND kind = ? AND revoked = 0
		  ORDER BY created_at DESC, id DESC`, userID, KindAPI)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		t, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeTokenByID revokes a user's own API key by id. It is scoped to the owner
// and to kind=api, so it never touches another user's token nor a session/
// pairing token: an id matching no live api key of this user returns ErrNotFound
// (the transport maps it to 404). Backs DELETE /auth/tokens/{id}.
func (s *Service) RevokeTokenByID(ctx context.Context, userID, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET revoked = 1
		  WHERE id = ? AND user_id = ? AND kind = ? AND revoked = 0`, id, userID, KindAPI)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// sqlExecer is satisfied by both *store.DB and *sql.Tx, so the auth_codes helpers
// below can run either standalone or inside a transaction.
type sqlExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// expiresAt renders an expiry timestamp ttl from now, or nil for "no expiry"
// (ttl <= 0). The single place auth-code lifetimes are computed.
func (s *Service) expiresAt(ttl time.Duration) any {
	if ttl <= 0 {
		return nil
	}
	return s.now().Add(ttl).UTC().Format(time.RFC3339)
}

// pastExpiry reports whether an RFC3339 expiry stamp is in the past - the
// read-side twin of expiresAt. NULL (no expiry) and unparsable stamps count as
// not expired.
func (s *Service) pastExpiry(expires sql.NullString) bool {
	if !expires.Valid {
		return false
	}
	t, err := time.Parse(time.RFC3339, expires.String)
	return err == nil && s.now().After(t)
}

// insertAuthCode writes one auth_codes row. Shared by invite and recovery mints
// so the column list lives in a single place.
func insertAuthCode(ctx context.Context, ex sqlExecer, hash string, userID int64, label string, maxUses int, expires any, createdAt, kind string) error {
	_, err := ex.ExecContext(ctx,
		`INSERT INTO auth_codes(code_hash, user_id, label, max_uses, expires_at, created_at, kind)
		 VALUES(?,?,?,?,?,?,?)`, hash, userID, label, maxUses, expires, createdAt, kind)
	return err
}

// CreateAuthCode generates a redeemable invite code bound to a user, without the
// supersede-on-mint hygiene - used by the first-run bootstrap, which has no prior
// invites to supersede. maxUses 0 means unlimited; ttl <= 0 means no expiry. The
// code is returned once. Admin minting goes through CreateInvite.
func (s *Service) CreateAuthCode(ctx context.Context, userID int64, label string, maxUses int, ttl time.Duration) (string, error) {
	code, hash, err := generateAuthCode()
	if err != nil {
		return "", err
	}
	if err := insertAuthCode(ctx, s.db, hash, userID, label, maxUses, s.expiresAt(ttl), s.ts(), CodeInvite); err != nil {
		return "", err
	}
	return code, nil
}

// CreateInvite mints a fresh invite for a user and, in the same transaction,
// supersedes the user's currently-active (still-redeemable) invites so there is
// exactly one active invite per user. Spent (used-up) and expired invites are
// left untouched as history. The code is returned once.
func (s *Service) CreateInvite(ctx context.Context, userID int64, label string, maxUses int, ttl time.Duration) (string, error) {
	code, hash, err := generateAuthCode()
	if err != nil {
		return "", err
	}
	if err := s.db.WithTx(ctx, "CreateInvite", func(tx *sql.Tx) error {
		if err := supersedeActiveInvites(ctx, tx, userID, s.ts()); err != nil {
			return err
		}
		return insertAuthCode(ctx, tx, hash, userID, label, maxUses, s.expiresAt(ttl), s.ts(), CodeInvite)
	}); err != nil {
		return "", err
	}
	return code, nil
}

// supersedeActiveInvites deletes a user's still-redeemable invites - those not
// used up and not expired - so a freshly minted invite is the only active one.
// Used-up and expired invites are kept as history. now is an RFC3339 UTC stamp,
// as is expires_at, so the lexical comparison is chronological.
func supersedeActiveInvites(ctx context.Context, ex sqlExecer, userID int64, now string) error {
	_, err := ex.ExecContext(ctx,
		`DELETE FROM auth_codes
		  WHERE user_id = ? AND kind = ?
		    AND (max_uses = 0 OR uses < max_uses)
		    AND (expires_at IS NULL OR expires_at > ?)`,
		userID, CodeInvite, now)
	return err
}

// RotateAuthCode regenerates an existing invite's secret in place: the old code
// stops working and a fresh one is returned (once), without leaving a new row
// behind. The invite's max_uses is preserved, its use counter and redeemed_at are
// reset (pending again), and its expiry is renewed for the same window it was
// originally granted (so resending a nearly-/already-expired invite yields a
// usable one). Pairing tokens linked to the code are revoked in the same
// transaction - the row survives rotation, so the delete cascade never fires -
// which disconnects any QR still on screen from the old secret. Only invite-kind
// codes rotate (recovery codes are user-owned); a missing or non-invite id
// returns ErrNotFound. This backs the admin "Resend".
func (s *Service) RotateAuthCode(ctx context.Context, id int64) (string, error) {
	code, hash, err := generateAuthCode()
	if err != nil {
		return "", err
	}
	if err := s.db.WithTx(ctx, "RotateAuthCode", func(tx *sql.Tx) error {
		var (
			createdAt string
			expires   sql.NullString
		)
		err := tx.QueryRowContext(ctx,
			`SELECT created_at, expires_at FROM auth_codes WHERE id = ? AND kind = ?`,
			id, CodeInvite).Scan(&createdAt, &expires)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		// Renew the expiry for the same duration the invite was originally granted,
		// measured from now - preserving the admin's chosen lifetime without storing
		// the TTL separately.
		var newExpires any
		if expires.Valid && expires.String != "" {
			if c, e1 := time.Parse(time.RFC3339, createdAt); e1 == nil {
				if x, e2 := time.Parse(time.RFC3339, expires.String); e2 == nil && x.After(c) {
					newExpires = s.now().Add(x.Sub(c)).UTC().Format(time.RFC3339)
				}
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE auth_codes
			    SET code_hash = ?, uses = 0, created_at = ?, expires_at = ?, redeemed_at = NULL
			  WHERE id = ? AND kind = ?`,
			hash, s.ts(), newExpires, id, CodeInvite); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE tokens SET revoked = 1 WHERE auth_code_id = ?`, id)
		return err
	}); err != nil {
		return "", err
	}
	return code, nil
}

// GenerateRecoveryCode mints a durable, reusable recovery code the user holds to
// re-authenticate after signing out or losing a device. It atomically replaces
// any existing recovery code for the user (so there is always at most one) and is
// returned once. Recovery codes never expire and have no use cap; they redeem
// through the same path as invites.
func (s *Service) GenerateRecoveryCode(ctx context.Context, userID int64) (string, error) {
	code, hash, err := generateAuthCode()
	if err != nil {
		return "", err
	}
	if err := s.db.WithTx(ctx, "GenerateRecoveryCode", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM auth_codes WHERE user_id = ? AND kind = ?`, userID, CodeRecovery); err != nil {
			return err
		}
		return insertAuthCode(ctx, tx, hash, userID, CodeRecovery, 0, nil, s.ts(), CodeRecovery)
	}); err != nil {
		return "", err
	}
	return code, nil
}

// ClearRecoveryCode removes a user's recovery code, if any. Wired to both the
// user's own DELETE /auth/recovery and the admin's per-user revoke.
func (s *Service) ClearRecoveryCode(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM auth_codes WHERE user_id = ? AND kind = ?`, userID, CodeRecovery)
	return err
}

// AuthCode describes an issued auth code for admin display. The plaintext code
// is never included - only its hash is stored, by design - so this carries the
// code's metadata only (label, lifetimes and usage).
type AuthCode struct {
	ID         int64  `json:"id"`
	Label      string `json:"label"`
	MaxUses    int    `json:"max_uses"` // 0 = unlimited
	Uses       int    `json:"uses"`
	ExpiresAt  string `json:"expires_at,omitempty"`  // empty = no expiry
	RedeemedAt string `json:"redeemed_at,omitempty"` // empty = never redeemed (pending)
	CreatedAt  string `json:"created_at"`
}

// ListAuthCodes returns the invite codes issued for a user, newest first.
// Recovery codes are deliberately excluded - they are user-owned and surfaced to
// the admin only as the User.HasRecovery flag, never as an actionable invite.
func (s *Service) ListAuthCodes(ctx context.Context, userID int64) ([]AuthCode, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, label, max_uses, uses, expires_at, redeemed_at, created_at
		   FROM auth_codes WHERE user_id = ? AND kind = ?
		   ORDER BY created_at DESC, id DESC`, userID, CodeInvite)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuthCode
	for rows.Next() {
		var (
			c        AuthCode
			expires  sql.NullString
			redeemed sql.NullString
		)
		if err := rows.Scan(&c.ID, &c.Label, &c.MaxUses, &c.Uses, &expires, &redeemed, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.ExpiresAt = expires.String
		c.RedeemedAt = redeemed.String
		out = append(out, c)
	}
	return out, rows.Err()
}

// RevokeAuthCode deletes an issued auth code by id, immediately invalidating it.
func (s *Service) RevokeAuthCode(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_codes WHERE id = ?`, id)
	return err
}

// RedeemedCode is a validated auth code resolved WITHOUT consuming a use. The
// use is claimed when a device actually pairs (ConsumePairingToken), so opening
// an invite link never burns a use on its own.
type RedeemedCode struct {
	User    *User
	CodeID  int64
	Kind    string // CodeInvite | CodeRecovery
	MaxUses int    // 0 = unlimited
	Uses    int
	// ExpiresAt is the code's RFC3339 UTC expiry, "" = never.
	ExpiresAt string
}

// UsesRemaining reports how many more devices can pair via this code, or nil
// for unlimited. Always >= 1: ResolveAuthCode (the only constructor) rejects
// exhausted codes. Advisory: concurrent exchanges may consume uses after it is
// computed.
func (rc *RedeemedCode) UsesRemaining() *int {
	if rc.MaxUses == 0 {
		return nil
	}
	n := rc.MaxUses - rc.Uses
	return &n
}

// codeState classifies whether an auth code can still claim a use:
// ErrCodeExpired past its expiry, ErrCodeExhausted at its use cap, nil while
// redeemable. The single Go definition of code liveness, shared by
// ResolveAuthCode and the failed-claim classification in ConsumePairingToken;
// the claim UPDATE's WHERE clause is its atomic SQL twin.
func (s *Service) codeState(maxUses, uses int, expires sql.NullString) error {
	if s.pastExpiry(expires) {
		return ErrCodeExpired
	}
	if maxUses > 0 && uses >= maxUses {
		return ErrCodeExhausted
	}
	return nil
}

// ResolveAuthCode validates a presented code and returns it with its bound
// user - without consuming a use. Expired, exhausted, and disabled-/deleted-user
// codes are rejected (so the caller never renders a QR that could not exchange),
// but nothing is written: no use is burned and redeemed_at is not stamped. The
// caller typically then mints a linked pairing token via IssuePairingToken.
func (s *Service) ResolveAuthCode(ctx context.Context, code string) (*RedeemedCode, error) {
	hash := hashSecret(normalizeCode(code))
	var (
		rc      RedeemedCode
		userID  int64
		expires sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, kind, max_uses, uses, expires_at FROM auth_codes WHERE code_hash = ?`, hash).
		Scan(&rc.CodeID, &userID, &rc.Kind, &rc.MaxUses, &rc.Uses, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidCode
	}
	if err != nil {
		return nil, err
	}
	if s.codeState(rc.MaxUses, rc.Uses, expires) != nil {
		return nil, ErrInvalidCode
	}
	rc.ExpiresAt = expires.String
	u, err := s.GetUser(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrInvalidCode
	}
	if err != nil {
		return nil, err
	}
	if u.Disabled {
		return nil, ErrInvalidCode
	}
	rc.User = u
	return &rc, nil
}

// recoveryPairingTTL bounds a pairing token minted from a recovery code: the
// code itself never expires, so the token carries a short lifetime of its own
// (multi-scan within it) instead of being eternal.
const recoveryPairingTTL = 10 * time.Minute

// IssuePairingToken mints a pairing token linked to rc's code, making the QR
// built from it as redeemable as the code itself: exchange claims a use on the
// code, and the token dies with the code (cascade on delete/supersede, revoke
// on rotate). Invite-kind tokens carry no expiry of their own - the parent
// invite's expiry and use cap govern them at exchange, which is what lets
// ConsumePairingToken report "invite has expired" rather than a generic token
// error. Recovery-kind tokens get recoveryPairingTTL instead.
func (s *Service) IssuePairingToken(ctx context.Context, rc *RedeemedCode) (string, error) {
	secret, hash, err := generateToken()
	if err != nil {
		return "", err
	}
	var expires any
	if rc.Kind == CodeRecovery {
		expires = s.expiresAt(recoveryPairingTTL)
	}
	if _, err := s.insertToken(ctx, rc.User.ID, hash, KindPairing, "", expires, rc.CodeID); err != nil {
		return "", err
	}
	return secret, nil
}

// ConsumePairingToken validates a pairing token and consumes it, returning the
// bound user. An UNLINKED token (minted by /auth/pair, the demo flow, or
// pre-migration) is atomically revoked - strictly single-use, and the
// revoke-if-not-revoked write means two racing exchanges cannot both win. A
// LINKED token instead atomically claims one use on its parent code - folding
// the cap check, the code-expiry check, and the first-claim redeemed_at stamp
// into one UPDATE - and is NOT revoked: the code's cap and expiry govern how
// many more devices may pair with it. A disabled user is rejected before any
// use is consumed.
func (s *Service) ConsumePairingToken(ctx context.Context, secret string) (*User, error) {
	hash := hashSecret(secret)
	u, codeID, err := s.lookupToken(ctx, hash, KindPairing)
	if err != nil {
		return nil, err
	}
	now := s.ts()
	if !codeID.Valid {
		res, err := s.db.ExecContext(ctx,
			`UPDATE tokens SET revoked = 1, last_seen = ? WHERE token_hash = ? AND revoked = 0`, now, hash)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, ErrInvalidToken
		}
		return u, nil
	}
	// Both stamps are RFC3339 UTC, so the lexical expires_at comparison is
	// chronological (same convention as supersedeActiveInvites).
	res, err := s.db.ExecContext(ctx,
		`UPDATE auth_codes SET uses = uses + 1, redeemed_at = COALESCE(redeemed_at, ?)
		   WHERE id = ? AND (max_uses = 0 OR uses < max_uses)
		     AND (expires_at IS NULL OR expires_at > ?)`, now, codeID.Int64, now)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Read-only classification so the transport can tell the user why: the
		// invite being gone means the token is orphaned (treat as invalid).
		var (
			maxUses, uses int
			cexp          sql.NullString
		)
		cerr := s.db.QueryRowContext(ctx,
			`SELECT max_uses, uses, expires_at FROM auth_codes WHERE id = ?`, codeID.Int64).
			Scan(&maxUses, &uses, &cexp)
		if errors.Is(cerr, sql.ErrNoRows) {
			return nil, ErrInvalidToken
		}
		if cerr != nil {
			return nil, cerr
		}
		if serr := s.codeState(maxUses, uses, cexp); serr != nil {
			return nil, serr
		}
		// Unreachable unless the SQL and Go liveness checks disagree; report the
		// claim failure as exhaustion rather than inventing a new state.
		return nil, ErrCodeExhausted
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE tokens SET last_seen = ? WHERE token_hash = ?`, now, hash)
	return u, nil
}

// userColumns selects the user fields plus a derived last-activity timestamp
// (the newest tokens.last_seen across the account's tokens). Activity is bumped
// on every authenticated request in ResolveToken, so this reflects last use, not
// just sign-in, without a dedicated column or extra writes.
const userColumns = `u.id, u.username, u.role, u.disabled, u.password_hash, u.is_demo,
	(SELECT MAX(t.last_seen) FROM tokens t WHERE t.user_id = u.id),
	EXISTS(SELECT 1 FROM auth_codes c WHERE c.user_id = u.id AND c.kind = '` + CodeRecovery + `')`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var (
		u           User
		hash        string
		lastSeen    sql.NullString
		hasRecovery bool
	)
	if err := row.Scan(&u.ID, &u.Username, &u.Role, &u.Disabled, &hash, &u.IsDemo, &lastSeen, &hasRecovery); err != nil {
		return u, err
	}
	u.HasPassword = hash != ""
	u.LastSeenAt = lastSeen.String
	u.HasRecovery = hasRecovery
	return u, nil
}

// GetUser returns a user by ID.
func (s *Service) GetUser(ctx context.Context, id int64) (*User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users u WHERE u.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &u, err
}

// ListUsers returns all accounts ordered by username.
func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+userColumns+` FROM users u ORDER BY u.username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountDemoUsers returns the number of live demo accounts (is_demo = 1). Used to
// cap how many can exist at once.
func (s *Service) CountDemoUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE is_demo = 1`).Scan(&n)
	return n, err
}

// ReapIdleDemoUsers deletes demo accounts (is_demo = 1) whose most recent token
// activity (or, lacking any, their creation time) is older than cutoff. Their
// child rows (progress, bookmarks, notes, history, tokens, share grants) cascade
// via ON DELETE CASCADE. Returns the number of accounts deleted. Timestamps are
// stored as RFC3339 UTC, so the lexical comparison is chronological.
func (s *Service) ReapIdleDemoUsers(ctx context.Context, cutoff time.Time) (int64, error) {
	cut := cutoff.UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM users
		  WHERE is_demo = 1
		    AND COALESCE(
		          (SELECT MAX(COALESCE(t.last_seen, t.created_at))
		             FROM tokens t WHERE t.user_id = users.id),
		          users.created_at
		        ) < ?`,
		cut)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// countEnabledAdmins returns the number of enabled admin accounts, optionally
// excluding one user id (pass 0 to exclude none). Used to prevent locking the
// last admin out of the console by demotion or disabling.
func (s *Service) countEnabledAdmins(ctx context.Context, exclude int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE role = ? AND disabled = 0 AND id != ?`,
		RoleAdmin, exclude).Scan(&n)
	return n, err
}

// SetDisabled enables or disables an account. Disabling the last enabled admin
// is refused so the console can never be locked out.
func (s *Service) SetDisabled(ctx context.Context, id int64, disabled bool) error {
	if disabled {
		u, err := s.GetUser(ctx, id)
		if err != nil {
			return err
		}
		if u.Role == RoleAdmin {
			others, err := s.countEnabledAdmins(ctx, id)
			if err != nil {
				return err
			}
			if others == 0 {
				return ErrLastAdmin
			}
		}
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET disabled = ?, updated_at = ? WHERE id = ?`, disabled, s.ts(), id)
	return err
}

// DeleteUser permanently removes an account. Deleting the last enabled admin is
// refused (ErrLastAdmin) so the console can never be locked out; deleting an
// unknown id is ErrNotFound. All of the user's durable state - sessions, auth
// codes, progress, bookmarks, notes, listening history and share grants - is
// removed by the schema's ON DELETE CASCADE rules (foreign_keys is ON, see
// store.Open). Files on disk are untouched (the library is the source of truth).
func (s *Service) DeleteUser(ctx context.Context, id int64) error {
	u, err := s.GetUser(ctx, id)
	if err != nil {
		return err
	}
	if u.Role == RoleAdmin && !u.Disabled {
		others, err := s.countEnabledAdmins(ctx, id)
		if err != nil {
			return err
		}
		if others == 0 {
			return ErrLastAdmin
		}
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	return err
}

// SetRole changes an account's role. Demoting the last enabled admin is refused.
// Promoting a password-less account to admin requires a password to be set first
// (see SetPassword) - admins must be able to sign in to the console.
func (s *Service) SetRole(ctx context.Context, id int64, role string) error {
	if role != RoleAdmin {
		role = RoleUser
	}
	u, err := s.GetUser(ctx, id)
	if err != nil {
		return err
	}
	if u.Role == RoleAdmin && role != RoleAdmin {
		others, err := s.countEnabledAdmins(ctx, id)
		if err != nil {
			return err
		}
		if others == 0 {
			return ErrLastAdmin
		}
	}
	if role == RoleAdmin && !u.HasPassword {
		return ErrAdminNeedsPassword
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE users SET role = ?, updated_at = ? WHERE id = ?`, role, s.ts(), id)
	return err
}

// SetPassword sets (or clears) an account's password. A non-empty password is
// hashed with argon2id; an empty password clears it (only valid for non-admins).
func (s *Service) SetPassword(ctx context.Context, id int64, password string) error {
	if err := validatePassword(password); err != nil {
		return err
	}
	hash := ""
	if password != "" {
		var err error
		if hash, err = HashPassword(password); err != nil {
			return err
		}
	} else {
		u, err := s.GetUser(ctx, id)
		if err != nil {
			return err
		}
		if u.Role == RoleAdmin {
			return ErrAdminNeedsPassword
		}
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, hash, s.ts(), id)
	return err
}

// CheckPassword verifies a plaintext password against a user's stored hash,
// returning nil on match and ErrInvalidCreds otherwise (including for a
// password-less account). Used to challenge a self-service password change.
func (s *Service) CheckPassword(ctx context.Context, id int64, password string) error {
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, id).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if hash == "" {
		return ErrInvalidCreds
	}
	ok, err := VerifyPassword(password, hash)
	if err != nil || !ok {
		return ErrInvalidCreds
	}
	return nil
}
