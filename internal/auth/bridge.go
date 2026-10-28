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
//   - bridge routes only: routes opt in to the token kinds they take, and only
//     the Bridge's own (/api/v1/bridge/*) list KindBridge, so every other route
//     refuses it;
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

// bridgeLiveWhere is what makes a tokens row t (with its user tu) a live bridge
// token: not revoked, and its admin still an enabled admin.
const bridgeLiveWhere = `t.kind = '` + KindBridge + `' AND t.revoked = 0 AND tu.disabled = 0 AND tu.role = '` + RoleAdmin + `'`

// BridgeLiveSQL is true while a bridge token is live. It is the one statement
// of "a Bridge is paired"; the bridge package's heartbeat reads use it too.
const BridgeLiveSQL = `EXISTS(SELECT 1 FROM tokens t JOIN users tu ON tu.id = t.user_id WHERE ` + bridgeLiveWhere + `)`

// BridgeTokenLiveSQL is BridgeLiveSQL for one token, its id the statement's
// next parameter: whether a request's bridge token is still the paired Bridge's.
// A Bridge's call was authenticated when it arrived; a pairing or an unpair may
// have revoked its token since (a heartbeat queued for the writer, a claim
// waiting out its long poll), so the bridge package's writes ask again.
const BridgeTokenLiveSQL = `EXISTS(SELECT 1 FROM tokens t JOIN users tu ON tu.id = t.user_id
	WHERE t.id = ? AND ` + bridgeLiveWhere + `)`

// revokeBridgeTokensSQL revokes every bridge token (pairing and unpairing).
const revokeBridgeTokensSQL = `UPDATE tokens SET revoked = 1 WHERE kind = '` + KindBridge + `' AND revoked = 0`

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
	_, err = s.db.ExecDurable(ctx, "IssueBridgePairCode",
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
	err = s.db.WithDurableTx(ctx, "ExchangeBridgeCode", func(tx *sql.Tx) error {
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
		if _, err := tx.ExecContext(ctx, revokeBridgeTokensSQL); err != nil {
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
	return s.db.WithDurableTx(ctx, "UnpairBridge", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, revokeBridgeTokensSQL); err != nil {
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
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT b.paired_at, b.paired_by, u.username, b.pair_expires_at, `+BridgeLiveSQL+`
		   FROM bridge_status b LEFT JOIN users u ON u.id = b.paired_by
		  WHERE b.id = 1`).
		Scan(&pairedAt, &byID, &byName, &expires, &p.Paired)
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
	// The code's hash and expiry are set and cleared together.
	if expires.Valid && !s.pastExpiry(expires) {
		p.CodeExpiresAt = expires.String
	}
	return p, nil
}
