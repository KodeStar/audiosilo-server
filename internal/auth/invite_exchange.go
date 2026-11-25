package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// inviteReplayWindow is how long after an invite exchange its device may
// replay it once to recover a lost answer (replayInvite).
const inviteReplayWindow = time.Minute

// PairingSession is a successful exchange. CodeKind is the kind of code the
// token came from (CodeInvite, CodeRecovery; "" for an unlinked token or a
// replay). Replayed answers the original sign-in again, so it never needs a
// second sign-in notification.
type PairingSession struct {
	User     *User
	Token    string
	CodeKind string
	Replayed bool
}

// ConsumePairing spends an unlinked token once, or claims one use on a linked
// token's parent code. Only invite-derived exchanges with a random device key
// can recover a lost answer (see replayInvite). A linked token is never
// revoked here (its code's cap and expiry govern it), and a token signed out,
// or of an account disabled, since its read is refused before any use is
// claimed (spendToken).
func (s *Service) ConsumePairing(ctx context.Context, secret, deviceName, deviceID string) (PairingSession, error) {
	hash := hashSecret(secret)
	u, row, err := s.lookupToken(ctx, hash, KindPairing)
	if err != nil {
		return PairingSession{}, err
	}
	var result PairingSession
	result.User, result.Token, err = s.exchangeSession(ctx, "ConsumePairing", u.ID, nil, func(tx *sql.Tx, now string) (string, error) {
		if err := spendToken(ctx, tx, hash, KindPairing, now, !row.codeID.Valid); err != nil {
			return "", err
		}
		if !row.codeID.Valid {
			return s.issueToken(ctx, tx, u.ID, KindSession, deviceName, 0)
		}
		deviceHash := ""
		if signInKeyPattern.MatchString(deviceID) {
			deviceHash = hashSecret(deviceID)
			token, err := replayInvite(ctx, tx, u.ID, row, deviceHash, now)
			if err != nil {
				return "", err
			}
			if token != "" {
				result.Replayed = true
				return token, nil
			}
		}
		// Both stamps are RFC3339 UTC, so their lexical order is chronological.
		err := tx.QueryRowContext(ctx,
			`UPDATE auth_codes SET uses = uses + 1, redeemed_at = COALESCE(redeemed_at, ?1)
			   WHERE id = ?2 AND (max_uses = 0 OR uses < max_uses)
			     AND (expires_at IS NULL OR expires_at > ?1)
			 RETURNING kind`, now, row.codeID.Int64).Scan(&result.CodeKind)
		if errors.Is(err, sql.ErrNoRows) {
			return "", s.claimRefused(ctx, tx, row.codeID.Int64)
		}
		if err != nil {
			return "", err
		}
		token, sessionID, err := s.issueTokenID(ctx, tx, u.ID, KindSession, deviceName, 0)
		if err != nil || result.CodeKind != CodeInvite || deviceHash == "" {
			return token, err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO invite_exchanges(pairing_id, device_hash, session_id, retry_until) VALUES(?, ?, ?, ?)`,
			row.id, deviceHash, sessionID, s.expiresAt(inviteReplayWindow))
		return token, err
	})
	if err != nil {
		return PairingSession{}, err
	}
	return result, nil
}

// replayInvite rotates the bearer on the original session row. The caller has
// rechecked the pairing credential in this transaction. An empty answer with
// no error means this device has never used this particular pairing token.
// Only a bearer never presented is rotated (last_seen NULL: an authenticated
// request's touch sets it): a lost answer was never used, and a session already
// in use is neither taken from its device nor left with credentials minted
// through the old bearer (an API key, a cast or pairing token).
func replayInvite(ctx context.Context, tx *sql.Tx, userID int64, pair tokenRow, deviceHash, now string) (string, error) {
	var sessionID int64
	var canReplay bool
	err := tx.QueryRowContext(ctx,
		`SELECT s.id, e.retry_until IS NOT NULL AND ?3 BETWEEN s.created_at AND e.retry_until
		        AND s.revoked = 0 AND s.last_seen IS NULL AND (s.expires_at IS NULL OR s.expires_at > ?3)
		        AND (c.expires_at IS NULL OR c.expires_at > ?3)
		   FROM invite_exchanges e JOIN tokens s ON s.id = e.session_id JOIN auth_codes c ON c.id = ?4
		  WHERE e.pairing_id = ?1 AND e.device_hash = ?2 AND s.user_id = ?5 AND s.kind = ?6`,
		pair.id, deviceHash, now, pair.codeID.Int64, userID, KindSession).Scan(&sessionID, &canReplay)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !canReplay {
		return "", ErrInviteUsedByDevice
	}
	secret, hash, err := generateToken()
	if err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tokens SET token_hash = ? WHERE id = ?`, hash, sessionID); err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `UPDATE invite_exchanges SET retry_until = NULL WHERE session_id = ?`, sessionID)
	return secret, err
}
