package auth

import (
	"context"
	"database/sql"
	"errors"
)

// The calendar feed token (Track U3, capability upcoming_calendar). A listener
// subscribes a calendar app to GET /me/upcoming/releases.ics?token=<secret>; the
// calendar app holds the secret, so it is a credential of its own, narrow by
// construction:
//
//   - one route: only the ICS route lists KindFeed (api.requireFeedAuth), so
//     every other route refuses it, and the ICS route refuses every other kind;
//   - one per user: a mint deletes the user's earlier feed token in the same
//     transaction (and 0060's partial unique index keeps two live rows out);
//   - no expiry, ended by a new mint ("Reset link"), DELETE /me/upcoming/calendar
//     ("Turn off"), the person becoming a kid (clearKidCredentials) or the
//     account's deletion (the FK cascade); a disabled account's is refused by
//     lookupToken like every token;
//   - stored as its SHA-256 hash only, shown once;
//   - never a device and never touched (resolve): a calendar's polls are not the
//     person's activity.
//
// A feed token's row is deleted rather than flagged revoked when it ends, so no
// rows pile up from resets and a reset leaves no trace of the old secret.

// FeedTokenName is the device name a feed token carries.
const FeedTokenName = "Calendar feed"

// deleteFeedTokensSQL removes a user's feed tokens (the statement's one
// parameter): a mint's first step, Turn off, and the switch to a kid.
const deleteFeedTokensSQL = `DELETE FROM tokens WHERE user_id = ?1 AND kind = '` + KindFeed + `'`

// FeedStatus is whether a user has a live feed token, and since when (RFC3339).
type FeedStatus struct {
	Active    bool   `json:"active"`
	CreatedAt string `json:"created_at,omitempty"`
}

// IssueFeedToken mints userID's calendar feed token, deleting any earlier one in
// the same transaction, and returns the secret (shown once) with its creation
// time. A kid's is refused inside the write (ErrKidAccount, refuseKid), so a
// request that passed the route's check just before the switch to a kid
// committed still mints nothing; a demo account is refused by the route.
func (s *Service) IssueFeedToken(ctx context.Context, userID int64) (secret, createdAt string, err error) {
	secret, hash, err := generateToken()
	if err != nil {
		return "", "", err
	}
	err = s.db.WithDurableTx(ctx, "IssueFeedToken", func(tx *sql.Tx) error {
		if err := refuseKid(ctx, tx, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, deleteFeedTokensSQL, userID); err != nil {
			return err
		}
		if _, err := s.insertToken(ctx, tx, userID, hash, KindFeed, FeedTokenName, nil, nil, ""); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT created_at FROM tokens WHERE token_hash = ?`, hash).Scan(&createdAt)
	})
	if err != nil {
		return "", "", err
	}
	return secret, createdAt, nil
}

// RevokeFeedToken ends userID's calendar feed token, if any (idempotent).
func (s *Service) RevokeFeedToken(ctx context.Context, userID int64) error {
	_, err := s.db.ExecDurable(ctx, "RevokeFeedToken", deleteFeedTokensSQL, userID)
	return err
}

// FeedTokenStatus reports whether userID has a live calendar feed token. The
// secret itself is never returned again.
func (s *Service) FeedTokenStatus(ctx context.Context, userID int64) (FeedStatus, error) {
	var createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT created_at FROM tokens WHERE user_id = ? AND kind = '`+KindFeed+`' AND revoked = 0
		  ORDER BY id DESC LIMIT 1`, userID).Scan(&createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return FeedStatus{}, nil
	}
	if err != nil {
		return FeedStatus{}, err
	}
	return FeedStatus{Active: true, CreatedAt: createdAt}, nil
}
