package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Cast tokens (capability cast): a Chromecast receiver plays a book from URLs the
// player hands it, and those URLs carry their credential as ?token=. The player
// never hands over its own session token (a receiver is a device the listener
// does not control, its URLs can surface in its logs or a debugger); it mints a
// cast token instead, which is narrow on every axis:
//
//   - one book: valid only for the (library, path) it was minted for, or a file
//     inside it on a segment boundary (CastGrant.Covers), so "Book" never reaches
//     "Book 2";
//   - media only: the api accepts it only as the ?token= query parameter of
//     GET /libraries/{id}/cover and /stream, never as a bearer header, never on
//     any other route (api.castAuthenticate);
//   - short-lived: CastTokenTTL, and a person keeps at most MaxCastTokens;
//   - still the person's: its user must exist and be enabled at every request,
//     and the media handlers check the person's CURRENT scope as for any request.
//
// Only the SHA-256 hash of the secret is stored, like every token.

// KindCast is the Credential.Kind of a request authenticated by a cast token. It
// is never a tokens.kind: cast tokens live in their own table.
const KindCast = "cast"

const (
	// CastTokenTTL is how long a cast token works: a long audiobook in one
	// sitting, not a durable credential.
	CastTokenTTL = 24 * time.Hour
	// MaxCastTokens is how many live cast tokens a person keeps; a mint past it
	// drops the oldest.
	MaxCastTokens = 20
)

// castTime is the cast tokens' timestamp format: UTC with milliseconds, fixed
// width (as catalog's session times), so expiries compare as text.
const castTime = "2006-01-02T15:04:05.000Z"

// CastGrant is what a cast token allows: the book it was minted for.
type CastGrant struct {
	// ID is the cast_tokens row id (the request's Credential.ID, Kind KindCast).
	ID        int64
	LibraryID int64
	// Path is the book's library-relative path, as catalog.CleanRelPath leaves it.
	Path string
}

// Covers reports whether the grant reaches path in library: the book itself or a
// file or folder inside it, on a segment boundary ("Author/Book" reaches
// "Author/Book/01.mp3", never "Author/Book 2/01.mp3"). path must already be
// clean (catalog.CleanRelPath), as every path the api checks is. Security-
// critical: the only thing keeping a cast token to its one book.
func (g CastGrant) Covers(libraryID int64, path string) bool {
	if g.LibraryID != libraryID || g.Path == "" || path == "" {
		return false
	}
	return path == g.Path || strings.HasPrefix(path, g.Path+"/")
}

// IssueCastToken mints a cast token for userID on the book at (libraryID,
// bookPath) and returns the secret (shown once) and its expiry (fixed-width
// millisecond UTC). The caller has checked the person may see the book. In the
// same transaction it deletes every expired cast token and keeps only the
// person's newest MaxCastTokens.
func (s *Service) IssueCastToken(ctx context.Context, userID, libraryID int64, bookPath string) (secret, expiresAt string, err error) {
	if userID <= 0 || libraryID <= 0 || bookPath == "" {
		return "", "", errors.New("auth: a cast token needs a user and a book")
	}
	secret, hash, err := generateToken()
	if err != nil {
		return "", "", err
	}
	now := s.now().UTC()
	created, expires := now.Format(castTime), now.Add(CastTokenTTL).Format(castTime)
	err = s.db.WithTx(ctx, "IssueCastToken", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM cast_tokens WHERE expires_at <= ?`, created); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cast_tokens(token_hash, user_id, library_id, rel_path, created_at, expires_at)
			 VALUES(?, ?, ?, ?, ?, ?)`, hash, userID, libraryID, bookPath, created, expires); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`DELETE FROM cast_tokens WHERE user_id = ?1 AND id NOT IN
			   (SELECT id FROM cast_tokens WHERE user_id = ?1 ORDER BY id DESC LIMIT ?2)`, userID, MaxCastTokens)
		return err
	})
	if err != nil {
		return "", "", err
	}
	return secret, expires, nil
}

// ResolveCastToken validates a presented cast token secret and returns its user
// (id, username, role) and the book it allows. An unknown or expired token, or
// one whose user is disabled, is ErrInvalidToken (a deleted user's tokens went
// with the account). It writes nothing. The caller still checks the request's
// path with CastGrant.Covers and the person's current scope.
func (s *Service) ResolveCastToken(ctx context.Context, secret string) (*User, CastGrant, error) {
	if secret == "" {
		return nil, CastGrant{}, ErrInvalidToken
	}
	var (
		u       User
		g       CastGrant
		expires string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT u.id, u.username, u.role, u.disabled, c.id, c.library_id, c.rel_path, c.expires_at
		   FROM cast_tokens c JOIN users u ON u.id = c.user_id
		  WHERE c.token_hash = ?`, hashSecret(secret)).
		Scan(&u.ID, &u.Username, &u.Role, &u.Disabled, &g.ID, &g.LibraryID, &g.Path, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, CastGrant{}, ErrInvalidToken
	}
	if err != nil {
		return nil, CastGrant{}, err
	}
	until, perr := time.Parse(time.RFC3339Nano, expires)
	if u.Disabled || perr != nil || !s.now().Before(until) {
		return nil, CastGrant{}, ErrInvalidToken
	}
	return &u, g, nil
}
