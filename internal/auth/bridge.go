package auth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"time"
)

// The Bridge's credential (BRIDGE-PLAN.md Phase 0). The Bridge is the optional
// companion that writes to the libraries and runs jobs; the server never
// connects to it, so it holds a token of its own and calls in. A bridge token is
// a tokens row of kind KindBridge, hashed like every token, minted by
// exchanging a one-time pairing code an admin made in the console
// (IssueBridgePairCode, ExchangeBridgeCode). It is narrow:
//
//   - bridge routes only: only ResolveBridgeRequest accepts it, and the api calls
//     that only for /api/v1/bridge/*; ResolveRequest and ResolveCastRequest never
//     do, so every other route refuses it (routes opt in to the kinds they take);
//   - its pairing admin's: it belongs to the admin who made the code, and works
//     only while that account exists, is enabled and is still an admin;
//   - one at a time: pairing revokes every earlier bridge token, and UnpairBridge
//     revokes the current one.
//
// It is never a device: the device lists and sign-outs name their kinds, and
// this is not one of them.

// bridgePairTTL is how long a pairing code can be exchanged: long enough to
// start a container and paste it, short enough not to linger.
const bridgePairTTL = 15 * time.Minute

// BridgeTokenName is the device name a bridge token carries.
const BridgeTokenName = "AudioSilo Bridge"

// BridgePairing is the pairing side of the bridge_status row: whether a Bridge
// is paired, by whom and when, and a code waiting to be exchanged.
type BridgePairing struct {
	// Paired reports whether a bridge token is live (not revoked, its admin
	// still an enabled admin).
	Paired   bool
	PairedAt string
	PairedBy *UserRef
	// CodeExpiresAt is the pending pairing code's expiry, "" with none (or an
	// expired one).
	CodeExpiresAt string
}

// IssueBridgePairCode makes a pairing code for adminID and returns it (shown
// once) with its expiry (RFC3339). It replaces any code still waiting; the
// paired Bridge, if any, keeps working until the code is exchanged. The caller
// has checked adminID is an admin.
func (s *Service) IssueBridgePairCode(ctx context.Context, adminID int64) (code, expiresAt string, err error) {
	code, hash, err := generateAuthCode()
	if err != nil {
		return "", "", err
	}
	expiresAt = s.now().Add(bridgePairTTL).UTC().Format(time.RFC3339)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO bridge_status(id, pair_code_hash, pair_expires_at, pair_created_by) VALUES(1, ?1, ?2, ?3)
		 ON CONFLICT(id) DO UPDATE SET pair_code_hash = ?1, pair_expires_at = ?2, pair_created_by = ?3`,
		hash, expiresAt, adminID)
	if err != nil {
		return "", "", err
	}
	return code, expiresAt, nil
}

// ExchangeBridgeCode trades a pairing code for a bridge token (the secret, shown
// once). In one transaction it checks the code (its hash, its expiry, and that
// the admin who made it is still an enabled admin), uses it up, revokes every
// earlier bridge token and clears the last Bridge's status, so a new Bridge
// starts from nothing. A wrong, used or expired code is ErrInvalidCode.
func (s *Service) ExchangeBridgeCode(ctx context.Context, code string) (secret string, err error) {
	presented := hashSecret(normalizeCode(code))
	secret, hash, err := generateToken()
	if err != nil {
		return "", err
	}
	err = s.db.WithTx(ctx, "ExchangeBridgeCode", func(tx *sql.Tx) error {
		var (
			stored  sql.NullString
			expires sql.NullString
			creator sql.NullInt64
		)
		err := tx.QueryRowContext(ctx,
			`SELECT pair_code_hash, pair_expires_at, pair_created_by FROM bridge_status WHERE id = 1`).
			Scan(&stored, &expires, &creator)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvalidCode
		}
		if err != nil {
			return err
		}
		if !stored.Valid || subtle.ConstantTimeCompare([]byte(stored.String), []byte(presented)) != 1 ||
			!expires.Valid || s.pastExpiry(expires) || !creator.Valid {
			return ErrInvalidCode
		}
		u, err := getUser(ctx, tx, creator.Int64)
		if errors.Is(err, ErrNotFound) {
			return ErrInvalidCode
		}
		if err != nil {
			return err
		}
		if u.Disabled || u.Role != RoleAdmin {
			return ErrInvalidCode
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tokens SET revoked = 1 WHERE kind = '`+KindBridge+`' AND revoked = 0`); err != nil {
			return err
		}
		if _, err := s.insertToken(ctx, tx, u.ID, hash, KindBridge, BridgeTokenName, nil, nil, ""); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE bridge_status SET pair_code_hash = NULL, pair_expires_at = NULL, pair_created_by = NULL,
			        paired_at = ?1, paired_by = ?2, heartbeat_at = NULL, status = '{}'
			  WHERE id = 1`, s.ts(), u.ID)
		return err
	})
	if err != nil {
		return "", err
	}
	return secret, nil
}

// UnpairBridge revokes every bridge token and forgets the paired Bridge, its
// status and any pairing code waiting. A Bridge's next call is then refused.
func (s *Service) UnpairBridge(ctx context.Context) error {
	return s.db.WithTx(ctx, "UnpairBridge", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE tokens SET revoked = 1 WHERE kind = '`+KindBridge+`' AND revoked = 0`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM bridge_status WHERE id = 1`)
		return err
	})
}

// GetBridgePairing reads the pairing side of the bridge_status row.
func (s *Service) GetBridgePairing(ctx context.Context) (BridgePairing, error) {
	var (
		p        BridgePairing
		pairedAt sql.NullString
		byID     sql.NullInt64
		byName   sql.NullString
		expires  sql.NullString
		hasCode  bool
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT b.paired_at, b.paired_by, u.username, b.pair_expires_at, b.pair_code_hash IS NOT NULL,
		        EXISTS(SELECT 1 FROM tokens t JOIN users tu ON tu.id = t.user_id
		                WHERE t.kind = '`+KindBridge+`' AND t.revoked = 0
		                  AND tu.disabled = 0 AND tu.role = '`+RoleAdmin+`')
		   FROM bridge_status b LEFT JOIN users u ON u.id = b.paired_by
		  WHERE b.id = 1`).
		Scan(&pairedAt, &byID, &byName, &expires, &hasCode, &p.Paired)
	if errors.Is(err, sql.ErrNoRows) {
		return BridgePairing{}, nil
	}
	if err != nil {
		return BridgePairing{}, err
	}
	if p.Paired {
		p.PairedAt = pairedAt.String
		if byID.Valid && byName.Valid {
			p.PairedBy = &UserRef{ID: byID.Int64, Username: byName.String}
		}
	}
	if hasCode && expires.Valid && !s.pastExpiry(expires) {
		p.CodeExpiresAt = expires.String
	}
	return p, nil
}

// ResolveBridgeRequest is ResolveRequest for the Bridge's own routes: it accepts
// a bridge token and nothing else, and only while its admin is still an enabled
// admin (lookupToken refuses a disabled one; a demoted one is refused here). Any
// refusal is ErrInvalidToken. The api calls it only for /api/v1/bridge/*.
func (s *Service) ResolveBridgeRequest(ctx context.Context, secret string, p Presence) (*User, Credential, error) {
	return s.resolve(ctx, secret, p, resolveFor{bridge: true}, []string{KindBridge})
}
