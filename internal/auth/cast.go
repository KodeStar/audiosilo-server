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
//   - short-lived: castTokenTTL from its last use, never more than
//     castTokenMaxLife after its mint (castExpiry: a receiver reading a long book
//     keeps its token past the first castTokenTTL, server#446), and each device
//     keeps at most maxCastTokens
//     (counted by the sending token, cast_grants.parent_token_id, so a run of
//     casts on one device never ends the cast another device is playing), with
//     maxCastTokensPerPerson over all of a person's devices; past a cap the
//     least recently used go (trimCastTokensSQL), so a sender re-minting drops
//     an idle token before the one its receiver is reading (a long response
//     keeps its token's use current while it runs: KeepCastToken); the
//     console's chapter editor, whose <audio> streams a book the same way, mints
//     an editor token (IssueEditorToken, cast_grants.editor): the same token with
//     a shorter, fixed life and a cap of its own, so editing many books never
//     drops a token a receiver is playing with;
//   - its sender's: it dies with the token that minted it (signed out, expired
//     or deleted), and its user must exist and be enabled at every request; the
//     media handlers check the person's CURRENT scope as for any request.
//
// Only the SHA-256 hash of the secret is stored, like every token. A dead cast
// token (expired, or its sender signed out) is deleted with its grant by the
// next mint and by the daily retention (PruneCastTokens), so neither the
// receiver's address nor the book it played outlives it.

const (
	// castTokenTTL is how long a cast token works after its mint, and after each
	// use (castExpiry): a long audiobook in one sitting, not a durable
	// credential.
	castTokenTTL = 24 * time.Hour
	// castTokenMaxLife is the most a cast token in use lives, from its mint: a
	// book that plays on over a long weekend, not a durable credential either.
	castTokenMaxLife = 72 * time.Hour
	// CastTouchInterval is how stale a cast token's last_seen may be while it is
	// in use: a request touches it at most this often (needsTouch; other tokens
	// wait touchInterval), and the api keeps a long response's token current at
	// this pace (KeepCastToken). The cap ranks tokens by it, so a token a
	// receiver is reading never looks older than one minted a few seconds ago.
	CastTouchInterval = 10 * time.Second
	// maxCastTokens is how many cast tokens one sending device (the token that
	// mints them) keeps; a mint past it drops that device's least recently used.
	maxCastTokens = 20
	// maxCastTokensPerPerson bounds a person's cast tokens over all their
	// devices; a mint past it drops the person's least recently used.
	maxCastTokensPerPerson = 100
	// editorTokenTTL is how long an editor token works: a sitting at the
	// console's chapter editor.
	editorTokenTTL = 4 * time.Hour
	// maxEditorTokens is how many editor tokens a person keeps, counted apart
	// from their cast tokens: an editor open on each of a few books at once.
	// The same number bounds a device and the person (one console, as before).
	maxEditorTokens = 5
)

// castUse is what a cast token is minted for, which sets its life and the caps
// it counts against: a receiver (useCast), or the console's chapter editor
// (useEditor).
type castUse struct {
	// editor is cast_grants.editor: 1 for the chapter editor's token.
	editor int
	ttl    time.Duration
	// keep is how many of this use one sending token keeps, perPerson how many
	// the person keeps over all their sending tokens.
	keep, perPerson int
}

var (
	useCast   = castUse{editor: 0, ttl: castTokenTTL, keep: maxCastTokens, perPerson: maxCastTokensPerPerson}
	useEditor = castUse{editor: 1, ttl: editorTokenTTL, keep: maxEditorTokens, perPerson: maxEditorTokens}
)

// trimCastTokensSQL deletes all but the ?3 most recently used of a person's
// (?1) cast tokens of one use (?2, cast_grants.editor), among those one sending
// token minted (?4) or, with ?4 = 0, all of them. Recent use is the token's
// last_seen, which every cover or stream request it authenticates bumps
// (resolve, at most once a CastTouchInterval) and a long response keeps current
// while it runs (KeepCastToken), else its mint time (created_at; both
// fixed-width millisecond UTC, millisUTC, so they compare as text; a token
// minted before server#446 has whole seconds, which only misorders it within its
// second), the newer mint first on a tie: a receiver playing a book keeps its
// token while a sender re-mints, and a token minted and never used goes first.
// The token the mint just made (?5) is kept whatever the times say: after the
// clock steps back, the others' times are ahead of its mint, and the mint must
// never hand out a token it has already deleted.
const trimCastTokensSQL = `DELETE FROM tokens WHERE id IN
  (SELECT t.id FROM tokens t JOIN cast_grants g ON g.token_id = t.id
    WHERE t.kind = '` + KindCast + `' AND t.user_id = ?1 AND g.editor = ?2
      AND (?4 = 0 OR g.parent_token_id = ?4)
    ORDER BY t.id = ?5 DESC, COALESCE(t.last_seen, t.created_at) DESC, t.id DESC LIMIT -1 OFFSET ?3)`

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
	// editor and mintedAt (the token's created_at) are its use and mint, which
	// set how far a use moves its expiry (castExpiry).
	editor   bool
	mintedAt string
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
// as the device it acts for. A token without a grant, whose parent is gone,
// signed out or expired, or whose account is disabled (lookupToken refused that
// already; KeepCastToken relies on it here) is ErrInvalidToken.
func (s *Service) castGrantOf(ctx context.Context, row *tokenRow) error {
	var (
		parentRevoked, disabled bool
		parentExpires           sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT g.library_id, g.rel_path, g.editor, c.created_at, g.parent_token_id, p.revoked, p.expires_at, u.disabled
		   FROM cast_grants g JOIN tokens p ON p.id = g.parent_token_id JOIN tokens c ON c.id = g.token_id
		        JOIN users u ON u.id = c.user_id
		  WHERE g.token_id = ?`, row.id).
		Scan(&row.grant.libraryID, &row.grant.path, &row.grant.editor, &row.grant.mintedAt, &row.deviceID,
			&parentRevoked, &parentExpires, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidToken
	}
	if err != nil {
		return err
	}
	if parentRevoked || disabled || s.pastExpiry(parentExpires) {
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
// millisecond UTC). The caller has checked the person may see the book. A
// parent signed out since the request's check, another person's, or a disabled
// account is ErrInvalidToken, read inside the write (signedIn): the token
// would be dead as minted (castGrantOf), and the caller is told so rather than
// handed it. In the
// same transaction it deletes every dead cast token (pruneCastTokensSQL) and
// keeps only the maxCastTokens most recently used the sending token minted, and
// the person's maxCastTokensPerPerson most recently used (editor tokens apart,
// trimCastTokensSQL): a device casting many books never ends the cast another
// device is playing.
func (s *Service) IssueCastToken(ctx context.Context, userID, parentTokenID, libraryID int64, bookPath string) (secret, expiresAt string, err error) {
	return s.issueCastToken(ctx, useCast, userID, parentTokenID, libraryID, bookPath)
}

// IssueEditorToken is IssueCastToken for the console's chapter editor: the same
// one-book, media-only token, valid for editorTokenTTL and counted against
// maxEditorTokens instead, so it never takes the place of a cast token (a
// receiver mid-book) and a cast never takes its.
func (s *Service) IssueEditorToken(ctx context.Context, userID, parentTokenID, libraryID int64, bookPath string) (secret, expiresAt string, err error) {
	return s.issueCastToken(ctx, useEditor, userID, parentTokenID, libraryID, bookPath)
}

func (s *Service) issueCastToken(ctx context.Context, use castUse, userID, parentTokenID, libraryID int64, bookPath string) (secret, expiresAt string, err error) {
	if userID <= 0 || parentTokenID <= 0 || libraryID <= 0 || bookPath == "" {
		return "", "", errors.New("auth: a cast token needs a user, its token and a book")
	}
	secret, hash, err := generateToken()
	if err != nil {
		return "", "", err
	}
	// Whole seconds: tokens.expires_at is RFC3339, and the wire's milliseconds
	// must name the same instant.
	expires := s.now().UTC().Add(use.ttl).Truncate(time.Second)
	err = s.db.WithTx(ctx, "IssueCastToken", func(tx *sql.Tx) error {
		if err := signedIn(ctx, tx, userID, parentTokenID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, pruneCastTokensSQL, s.ts()); err != nil {
			return err
		}
		res, err := s.insertTokenOn(ctx, tx, userID, hash, KindCast, "", expires.Format(time.RFC3339), nil, "", nil,
			s.castStamp())
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cast_grants(token_id, library_id, rel_path, parent_token_id, editor) VALUES(?, ?, ?, ?, ?)`,
			id, libraryID, bookPath, parentTokenID, use.editor); err != nil {
			return err
		}
		// The caps, among the person's tokens of this use only (every cast
		// token has a grant by now: the prune above deleted any without one):
		// all but the use.keep most recently used of this sending token's go,
		// then all but the person's use.perPerson most recently used (alone
		// when it is no larger), the token just minted (id) kept by both.
		if use.keep < use.perPerson {
			if _, err := tx.ExecContext(ctx, trimCastTokensSQL, userID, use.editor, use.keep, parentTokenID, id); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, trimCastTokensSQL, userID, use.editor, use.perPerson, 0, id)
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

// castStamp is a cast token's created_at and last_seen: millisecond UTC
// (millisUTC), so the cap's least-recently-used rank (trimCastTokensSQL) tells
// apart uses within a second.
func (s *Service) castStamp() string { return s.now().UTC().Format(millisUTC) }

// castExpiry is the expiry a use of a cast token with grant g moves it to
// (server#446): castTokenTTL from now, never past castTokenMaxLife after its
// mint, so a receiver reading a long book keeps its token as long as it plays
// (up to that bound) and an idle one ends as before. "" leaves the expiry: an
// editor token (a fixed editorTokenTTL; the console mints one per sitting) or a
// mint time that doesn't parse. The touch never shortens an expiry, nor moves
// one already past (touchRowSQL).
func (s *Service) castExpiry(g castGrant) string {
	minted, err := time.Parse(time.RFC3339, g.mintedAt)
	if g.editor || err != nil {
		return ""
	}
	to := s.now().UTC().Add(castTokenTTL)
	if limit := minted.UTC().Add(castTokenMaxLife); to.After(limit) {
		to = limit
	}
	return to.Truncate(time.Second).Format(time.RFC3339)
}

// KeepCastToken records that cast token id is still in use by a response it
// authenticated (server#446): a request touches its token only as it starts
// (resolve), so a long single-file stream would look idle to the cap
// (trimCastTokensSQL) and let its expiry run out mid-book. The api calls it
// every CastTouchInterval while such a response runs, and once as it ends. It
// moves last_seen and the expiry (castExpiry) as a request's touch does; a
// token deleted or signed out since, whose sender is, or whose account is
// disabled (castGrantOf: a suspended token's expiry stays where the suspension
// found it), is left as it is. The grant read and the touch are separate statements: what the read
// takes (the token's use and mint time) never changes, and the touch moves no
// expiry already past nor a revoked token (touchRowSQL).
func (s *Service) KeepCastToken(ctx context.Context, id int64) error {
	row := tokenRow{id: id}
	if err := s.castGrantOf(ctx, &row); errors.Is(err, ErrInvalidToken) {
		return nil
	} else if err != nil {
		return err
	}
	return s.touch(ctx, id, KindCast, Presence{}, s.castExpiry(row.grant))
}
