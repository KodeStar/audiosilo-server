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
)

// User is an account record (without the password hash for callers).
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	Disabled bool   `json:"disabled"`
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

// CreateUser creates an account and returns it.
func (s *Service) CreateUser(ctx context.Context, username, password, role string) (*User, error) {
	if username == "" || password == "" {
		return nil, errors.New("username and password are required")
	}
	if role != RoleAdmin {
		role = RoleUser
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	now := s.ts()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users(username, password_hash, role, created_at, updated_at)
		 VALUES(?,?,?,?,?)`, username, hash, role, now, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &User{ID: id, Username: username, Role: role}, nil
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
// usernames. It corresponds to a random password no one knows.
const dummyHash = "$argon2id$v=19$m=65536,t=1,p=4$YWFhYWFhYWFhYWFhYWFhYQ$3KhZ0r0u2y0xq8b9oQzWtkqj9o0a9hZ0r0u2y0xq8b8"

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

// GetUser returns a user by ID.
func (s *Service) GetUser(ctx context.Context, id int64) (*User, error) {
	var u User
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, role, disabled FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Username, &u.Role, &u.Disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &u, err
}

// ListUsers returns all accounts ordered by username.
func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, username, role, disabled FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.Disabled); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetDisabled enables or disables an account.
func (s *Service) SetDisabled(ctx context.Context, id int64, disabled bool) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET disabled = ?, updated_at = ? WHERE id = ?`, disabled, s.ts(), id)
	return err
}
