// Package auth manages users, opaque session tokens, redeemable auth codes and
// the QR pairing payload used by the "enter your auth code" connect flow.
package auth

import (
	"context"
	"database/sql"
	"errors"
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
)

// Errors returned by the service.
var (
	ErrNotFound     = errors.New("not found")
	ErrInvalidCreds = errors.New("invalid credentials")
	ErrInvalidToken = errors.New("invalid or expired token")
	ErrInvalidCode  = errors.New("invalid or expired auth code")
	// ErrLastAdmin is returned when an operation would leave no enabled admin.
	ErrLastAdmin = errors.New("cannot remove the last admin")
	// ErrAdminNeedsPassword is returned when an account would become (or remain)
	// an admin without a password to sign in to the console.
	ErrAdminNeedsPassword = errors.New("admin accounts require a password")
	// ErrPasswordTooShort is returned when a non-empty password is below the
	// minimum length.
	ErrPasswordTooShort = errors.New("password must be at least 8 characters")
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
	if username == "" {
		return nil, errors.New("username is required")
	}
	if role != RoleAdmin {
		role = RoleUser
	}
	if password == "" && role == RoleAdmin {
		return nil, errors.New("a password is required for admin accounts")
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
		`INSERT INTO users(username, password_hash, role, created_at, updated_at)
		 VALUES(?,?,?,?,?)`, username, hash, role, now, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &User{ID: id, Username: username, Role: role, HasPassword: hash != ""}, nil
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
// secret (shown once). ttl <= 0 means no expiry.
func (s *Service) IssueToken(ctx context.Context, userID int64, kind, deviceName string, ttl time.Duration) (string, error) {
	secret, hash, err := generateToken()
	if err != nil {
		return "", err
	}
	var expires any
	if ttl > 0 {
		expires = s.now().Add(ttl).UTC().Format(time.RFC3339)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO tokens(user_id, token_hash, kind, device_name, created_at, expires_at)
		 VALUES(?,?,?,?,?,?)`, userID, hash, kind, deviceName, s.ts(), expires)
	if err != nil {
		return "", err
	}
	return secret, nil
}

// ResolveToken validates a presented token secret and returns its user, also
// bumping last_seen. Revoked/expired tokens return ErrInvalidToken.
func (s *Service) ResolveToken(ctx context.Context, secret, kind string) (*User, error) {
	hash := hashSecret(secret)
	var (
		u       User
		expires sql.NullString
		revoked bool
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT u.id, u.username, u.role, u.disabled, t.expires_at, t.revoked
		   FROM tokens t JOIN users u ON u.id = t.user_id
		  WHERE t.token_hash = ? AND t.kind = ?`, hash, kind).
		Scan(&u.ID, &u.Username, &u.Role, &u.Disabled, &expires, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, err
	}
	if revoked || u.Disabled {
		return nil, ErrInvalidToken
	}
	if expires.Valid {
		if t, perr := time.Parse(time.RFC3339, expires.String); perr == nil && s.now().After(t) {
			return nil, ErrInvalidToken
		}
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE tokens SET last_seen = ? WHERE token_hash = ?`, s.ts(), hash)
	return &u, nil
}

// RevokeToken revokes a token by its secret.
func (s *Service) RevokeToken(ctx context.Context, secret string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET revoked = 1 WHERE token_hash = ?`, hashSecret(secret))
	return err
}

// CreateAuthCode generates a redeemable auth code bound to a user. maxUses 0
// means unlimited; ttl <= 0 means no expiry. The code is returned once.
func (s *Service) CreateAuthCode(ctx context.Context, userID int64, label string, maxUses int, ttl time.Duration) (string, error) {
	code, hash, err := generateAuthCode()
	if err != nil {
		return "", err
	}
	var expires any
	if ttl > 0 {
		expires = s.now().Add(ttl).UTC().Format(time.RFC3339)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO auth_codes(code_hash, user_id, label, max_uses, expires_at, created_at)
		 VALUES(?,?,?,?,?,?)`, hash, userID, label, maxUses, expires, s.ts())
	if err != nil {
		return "", err
	}
	return code, nil
}

// AuthCode describes an issued auth code for admin display. The plaintext code
// is never included — only its hash is stored, by design — so this carries the
// code's metadata only (label, lifetimes and usage).
type AuthCode struct {
	ID        int64  `json:"id"`
	Label     string `json:"label"`
	MaxUses   int    `json:"max_uses"` // 0 = unlimited
	Uses      int    `json:"uses"`
	ExpiresAt string `json:"expires_at,omitempty"` // empty = no expiry
	CreatedAt string `json:"created_at"`
}

// ListAuthCodes returns the auth codes issued for a user, newest first.
func (s *Service) ListAuthCodes(ctx context.Context, userID int64) ([]AuthCode, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, label, max_uses, uses, expires_at, created_at
		   FROM auth_codes WHERE user_id = ? ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuthCode
	for rows.Next() {
		var (
			c       AuthCode
			expires sql.NullString
		)
		if err := rows.Scan(&c.ID, &c.Label, &c.MaxUses, &c.Uses, &expires, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.ExpiresAt = expires.String
		out = append(out, c)
	}
	return out, rows.Err()
}

// RevokeAuthCode deletes an issued auth code by id, immediately invalidating it.
func (s *Service) RevokeAuthCode(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_codes WHERE id = ?`, id)
	return err
}

// RedeemAuthCode validates a presented code, increments its use counter and
// returns the bound user. The caller typically then issues a pairing token.
func (s *Service) RedeemAuthCode(ctx context.Context, code string) (*User, error) {
	hash := hashSecret(normalizeCode(code))
	var (
		id, userID    int64
		maxUses, uses int
		expires       sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, max_uses, uses, expires_at FROM auth_codes WHERE code_hash = ?`, hash).
		Scan(&id, &userID, &maxUses, &uses, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidCode
	}
	if err != nil {
		return nil, err
	}
	if expires.Valid {
		if t, perr := time.Parse(time.RFC3339, expires.String); perr == nil && s.now().After(t) {
			return nil, ErrInvalidCode
		}
	}
	if maxUses > 0 && uses >= maxUses {
		return nil, ErrInvalidCode
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE auth_codes SET uses = uses + 1 WHERE id = ?`, id); err != nil {
		return nil, err
	}
	u, err := s.GetUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u.Disabled {
		return nil, ErrInvalidCode
	}
	return u, nil
}

// userColumns selects the user fields plus a derived last-activity timestamp
// (the newest tokens.last_seen across the account's tokens). Activity is bumped
// on every authenticated request in ResolveToken, so this reflects last use, not
// just sign-in, without a dedicated column or extra writes.
const userColumns = `u.id, u.username, u.role, u.disabled, u.password_hash,
	(SELECT MAX(t.last_seen) FROM tokens t WHERE t.user_id = u.id)`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var (
		u        User
		hash     string
		lastSeen sql.NullString
	)
	if err := row.Scan(&u.ID, &u.Username, &u.Role, &u.Disabled, &hash, &lastSeen); err != nil {
		return u, err
	}
	u.HasPassword = hash != ""
	u.LastSeenAt = lastSeen.String
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

// SetRole changes an account's role. Demoting the last enabled admin is refused.
// Promoting a password-less account to admin requires a password to be set first
// (see SetPassword) — admins must be able to sign in to the console.
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
