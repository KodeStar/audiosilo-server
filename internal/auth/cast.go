package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/kodestar/audiosilo-server/internal/relpath"
)

// Cast tokens (capability cast): a Chromecast receiver plays a book from URLs the
// player hands it, and those URLs carry their credential as ?token=. The player
// never hands over its own session token (a receiver is a device the listener
// does not control, its URLs can surface in its logs or a debugger); it mints a
// cast token instead: a tokens row of kind KindCast with a cast_grants row, so
// lookupToken's one validity check (revoked, expired, disabled user) holds for
// it too. It is narrow on every axis:
//
//   - one book: valid only for the (library, path) it was minted for, or a file
//     inside it on a segment boundary (castGrant.covers), so "Book" never
//     reaches "Book 2";
//   - media only: only ResolveCastRequest accepts it, which the api calls only
//     for the ?token= of GET /libraries/{id}/cover and /stream with no
//     Authorization header; ResolveRequest never does;
//   - short-lived: castTokenTTL, and a person keeps at most maxCastTokens;
//   - its sender's: it dies with the token that minted it (signed out, expired
//     or deleted), and its user must exist and be enabled at every request; the
//     media handlers check the person's CURRENT scope as for any request.
//
// Only the SHA-256 hash of the secret is stored, like every token. A dead cast
// token (expired, or its sender signed out) is deleted with its grant by the
// next mint and by the daily retention (PruneCastTokens), so neither the
// receiver's address nor the book it played outlives it.

const (
	// castTokenTTL is how long a cast token works: a long audiobook in one
	// sitting, not a durable credential.
	castTokenTTL = 24 * time.Hour
	// maxCastTokens is how many cast tokens a person keeps; a mint past it drops
	// the oldest.
	maxCastTokens = 20
)

// millisUTC is the wire format of the times auth hands the player (a cast
// token's expiry, a member's created_at, a feed token's created_at): UTC with
// milliseconds, fixed width (as catalog's session times).
const millisUTC = "2006-01-02T15:04:05.000Z"

// castGrant is what a cast token allows (cast_grants): the book it was minted
// for. libraryID is invalid for a token without a grant, which covers nothing.
type castGrant struct {
	libraryID sql.NullInt64
	// path is the book's library-relative path, as catalog.CleanRelPath leaves it.
	path string
}

// bookMedia names the media a request reads: a library and a clean path in it
// (relpath.Clean, which ResolveCastRequest applies).
type bookMedia struct {
	libraryID int64
	path      string
}

// covers reports whether g reaches m: the same library, and m's path the book
// itself or a file or folder inside it on a segment boundary ("Author/Book"
// reaches "Author/Book/01.mp3", never "Author/Book 2/01.mp3"). An empty path on
// either side covers nothing (relpath.Within reads "" as the whole library).
// Security-critical: the only thing keeping a cast token to its one book.
func (g castGrant) covers(m bookMedia) bool {
	return g.libraryID.Valid && g.libraryID.Int64 == m.libraryID &&
		g.path != "" && m.path != "" && relpath.Within(g.path, m.path)
}

// castGrantOf reads the grant of row, a cast token that passed lookupToken's
// shared checks: its book, and its parent (the sender's token that minted it)
// as the device it acts for. A token without a grant, or whose parent is gone,
// signed out or expired, is ErrInvalidToken.
func (s *Service) castGrantOf(ctx context.Context, row *tokenRow) error {
	var (
		parentRevoked bool
		parentExpires sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT g.library_id, g.rel_path, g.parent_token_id, p.revoked, p.expires_at
		   FROM cast_grants g JOIN tokens p ON p.id = g.parent_token_id
		  WHERE g.token_id = ?`, row.id).
		Scan(&row.grant.libraryID, &row.grant.path, &row.deviceID, &parentRevoked, &parentExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidToken
	}
	if err != nil {
		return err
	}
	if parentRevoked || s.pastExpiry(parentExpires) {
		return ErrInvalidToken
	}
	return nil
}

// pruneCastTokensSQL deletes every dead cast token: expired (?1 is now, as
// s.ts() writes it; expires_at is RFC3339 UTC, so it compares as text), or
// whose grant is missing or whose parent is gone, signed out or expired. None
// can work again (a signed-out token never signs back in), and its row would keep
// the receiver's address and its grant, which person cast which book; the
// grant goes with the token (FK cascade). kind is a literal so the planner sees
// idx_tokens_cast_expiry when it prepares the statement.
const pruneCastTokensSQL = `DELETE FROM tokens
  WHERE kind = '` + KindCast + `'
    AND (expires_at <= ?1
         OR NOT EXISTS (SELECT 1 FROM cast_grants g JOIN tokens p ON p.id = g.parent_token_id
                         WHERE g.token_id = tokens.id AND p.revoked = 0
                           AND (p.expires_at IS NULL OR p.expires_at > ?1)))`

// PruneCastTokens deletes the dead cast tokens and their grants
// (pruneCastTokensSQL). Run by the daily retention job; every mint does the same.
func (s *Service) PruneCastTokens(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, pruneCastTokensSQL, s.ts())
	return err
}

// IssueCastToken mints a cast token for userID on the book at (libraryID,
// bookPath), on behalf of parentTokenID (the caller's own token, whose sign-out
// ends it), and returns the secret (shown once) and its expiry (fixed-width
// millisecond UTC). The caller has checked the person may see the book. In the
// same transaction it deletes every dead cast token (pruneCastTokensSQL) and
// keeps only the person's newest maxCastTokens.
func (s *Service) IssueCastToken(ctx context.Context, userID, parentTokenID, libraryID int64, bookPath string) (secret, expiresAt string, err error) {
	if userID <= 0 || parentTokenID <= 0 || libraryID <= 0 || bookPath == "" {
		return "", "", errors.New("auth: a cast token needs a user, its token and a book")
	}
	secret, hash, err := generateToken()
	if err != nil {
		return "", "", err
	}
	// Whole seconds: tokens.expires_at is RFC3339, and the wire's milliseconds
	// must name the same instant.
	expires := s.now().UTC().Add(castTokenTTL).Truncate(time.Second)
	err = s.db.WithTx(ctx, "IssueCastToken", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, pruneCastTokensSQL, s.ts()); err != nil {
			return err
		}
		res, err := s.insertToken(ctx, tx, userID, hash, KindCast, "", expires.Format(time.RFC3339), nil, "")
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cast_grants(token_id, library_id, rel_path, parent_token_id) VALUES(?, ?, ?, ?)`,
			id, libraryID, bookPath, parentTokenID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`DELETE FROM tokens WHERE kind = '`+KindCast+`' AND user_id = ?1 AND id NOT IN
			   (SELECT id FROM tokens WHERE kind = '`+KindCast+`' AND user_id = ?1 ORDER BY id DESC LIMIT ?2)`,
			userID, maxCastTokens)
		return err
	})
	if err != nil {
		return "", "", err
	}
	return secret, expires.Format(millisUTC), nil
}

// ResolveCastRequest is ResolveRequest for a request reading a book's media at
// (libraryID, path): it also accepts a cast token, when kinds lists KindCast,
// but only one whose book covers that path (castGrant.covers). path is cleaned
// here (relpath.Clean, as the media handlers clean ?path=) before the grant's
// literal prefix rule judges it, so "Book/../Book 2/01.mp3" is "Book 2/01.mp3"
// whoever calls. Any refusal is ErrInvalidToken. The api calls it only for the
// ?token= of a book's cover or stream with no Authorization header.
func (s *Service) ResolveCastRequest(ctx context.Context, secret string, p Presence, libraryID int64, path string, kinds ...string) (*User, Credential, error) {
	return s.resolve(ctx, secret, p, &bookMedia{libraryID, relpath.Clean(path)}, kinds)
}
