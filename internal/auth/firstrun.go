package auth

import (
	"context"
	"database/sql"
	"errors"

	"github.com/kodestar/audiosilo-server/internal/store"
)

// The first-run admin (server#369): a headless start with no admin makes one,
// "admin" with a random password printed once (pkg/launcher ensureAdmin). A
// recreated container loses that log, so until an admin first signs in with a
// password the server says so again on every start, with the reset-admin
// command. The state is one server_state row written only by MarkFirstAdmin, at
// that creation: an install from before it existed, or one whose admin chose a
// password in the --setup wizard, has no row, so it never prints the hint and
// needs no backfill. Never a password or a code: only who the admin is, two
// facts and which auth code was printed beside the password.

// InitialAdminCodeLabel labels the auth code the first start prints beside the
// admin's password (pkg/launcher ensureAdmin): unlimited uses, no expiry. The
// row records which code that is (firstAdmin.CodeID); RevokeInitialAdminCode
// finds it by the label only on an install whose row doesn't.
const InitialAdminCodeLabel = "initial admin code"

// firstAdminKey is the row's key in server_state (migration 0040).
const firstAdminKey = "first_admin"

// firstAdmin is the row's value.
type firstAdmin struct {
	// UserID is the admin ensureAdmin made.
	UserID int64 `json:"user_id"`
	// SignedIn is set by the first password sign-in of any admin, or a
	// password set on an admin account (their own change, another admin's,
	// reset-admin): someone holds a working password.
	SignedIn bool `json:"signed_in"`
	// ChosePassword is set once UserID's password is replaced, by them,
	// another admin or reset-admin, so the printed one is gone (the console's
	// Your account step words itself by it).
	ChosePassword bool `json:"chose_password"`
	// CodeID and CodeCreatedAt name the auth code printed beside the password
	// (auth_codes.id and created_at). A rotation in the console keeps the row
	// but stamps it anew, so a rotated code, which was never printed, no longer
	// matches. 0 and "" when no code was made, and in a row written before
	// they were recorded.
	CodeID        int64  `json:"code_id,omitempty"`
	CodeCreatedAt string `json:"code_created_at,omitempty"`
}

// MarkFirstAdmin records id as the admin the server made at first start with a
// printed password, not yet signed in, and code ("" for none) as the auth code
// printed beside it.
func (s *Service) MarkFirstAdmin(ctx context.Context, id int64, code string) error {
	fa := firstAdmin{UserID: id}
	if code != "" {
		err := s.db.QueryRowContext(ctx, `SELECT id, created_at FROM auth_codes WHERE code_hash = ? AND user_id = ?`,
			hashSecret(normalizeCode(code)), id).Scan(&fa.CodeID, &fa.CodeCreatedAt)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	return s.putFirstAdmin(ctx, fa)
}

// readFirstAdmin is the row; ok is false without one (or with one that doesn't
// read, which counts as none: a real row names its admin).
func (s *Service) readFirstAdmin(ctx context.Context) (fa firstAdmin, ok bool, err error) {
	fa, err = store.GetState[firstAdmin](ctx, s.db, firstAdminKey)
	return fa, err == nil && fa.UserID != 0, err
}

func (s *Service) putFirstAdmin(ctx context.Context, fa firstAdmin) error {
	return store.PutState(ctx, s.db, firstAdminKey, fa, s.ts())
}

// FirstAdminWaiting returns the admin the server made at first start while no
// admin has signed in with a password yet (the start's hint), else nil. An
// account gone since is nil too, and so is one no longer an enabled admin: the
// hint would name an account that can't sign in to the console (and that
// reset-admin refuses).
func (s *Service) FirstAdminWaiting(ctx context.Context) (*User, error) {
	fa, ok, err := s.readFirstAdmin(ctx)
	if err != nil || !ok || fa.SignedIn {
		return nil, err
	}
	u, err := s.GetUser(ctx, fa.UserID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil || u.Role != RoleAdmin || u.Disabled {
		return nil, err
	}
	return u, nil
}

// NoteAdminSignIn records that an admin signed in with a password, ending the
// start's hint. It writes only the first time (a write moves the content
// generation), and nothing on a server without the row.
func (s *Service) NoteAdminSignIn(ctx context.Context) error {
	return s.noteFirstAdmin(ctx, func(fa *firstAdmin) { fa.SignedIn = true })
}

// notePasswordSet is UpdateUser's note of a password set on account id (admin:
// an admin account once the change is made), inside its transaction tx: an
// admin's ends the start's hint (signed in, as NoteAdminSignIn: the hint would
// point at the first start's password while someone holds a working one), and
// the first-run admin's, whoever set it (their own change in the Your account
// step or People, another admin's, reset-admin), replaces the printed password
// (printed_password false) and ends the auth code printed beside it in the same
// log (deletePrintedCode). Writes only what changes.
func notePasswordSet(ctx context.Context, tx *sql.Tx, id int64, admin bool, now string) error {
	fa, err := store.GetState[firstAdmin](ctx, tx, firstAdminKey)
	if err != nil || fa.UserID == 0 {
		return err
	}
	next := fa
	next.SignedIn = next.SignedIn || admin
	if fa.UserID == id {
		next.ChosePassword = true
		if _, err := deletePrintedCode(ctx, tx, fa); err != nil {
			return err
		}
	}
	if next == fa {
		return nil
	}
	return store.PutState(ctx, tx, firstAdminKey, next, now)
}

// deletePrintedCode deletes the auth code the row says was printed (CodeID with
// its CodeCreatedAt: a code rotated since is not it) and reports whether there
// was one. A row with no code recorded deletes nothing.
func deletePrintedCode(ctx context.Context, ex sqlExecer, fa firstAdmin) (bool, error) {
	if fa.CodeID == 0 {
		return false, nil
	}
	res, err := ex.ExecContext(ctx,
		`DELETE FROM auth_codes WHERE id = ? AND user_id = ? AND kind = ? AND created_at = ?`,
		fa.CodeID, fa.UserID, CodeInvite, fa.CodeCreatedAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// noteFirstAdmin applies note to the row, when there is one and note changes
// it. The common case (nothing to change) is one read; a change reads the row
// again and writes it in one transaction, so two notes at once (a sign-in
// beside a password change) never lose one another's fact.
func (s *Service) noteFirstAdmin(ctx context.Context, note func(*firstAdmin)) error {
	changed := func(fa firstAdmin) (firstAdmin, bool) {
		next := fa
		note(&next)
		return next, fa.UserID != 0 && next != fa
	}
	fa, _, err := s.readFirstAdmin(ctx)
	if err != nil {
		return err
	}
	if _, ok := changed(fa); !ok {
		return nil
	}
	return s.db.WithTx(ctx, "NoteFirstAdmin", func(tx *sql.Tx) error {
		fa, err := store.GetState[firstAdmin](ctx, tx, firstAdminKey)
		if err != nil {
			return err
		}
		next, ok := changed(fa)
		if !ok {
			return nil
		}
		return store.PutState(ctx, tx, firstAdminKey, next, s.ts())
	})
}

// PrintedPassword reports whether admin id still has the password the server
// printed at first start: id is the first-run admin and hasn't set their own.
// An admin who chose one in the --setup wizard, or any admin of an install
// from before the row existed, has not.
func (s *Service) PrintedPassword(ctx context.Context, id int64) (bool, error) {
	fa, ok, err := s.readFirstAdmin(ctx)
	if err != nil || !ok {
		return false, err
	}
	return fa.UserID == id && !fa.ChosePassword, nil
}

// RevokeInitialAdminCode deletes admin id's first-run auth code, the one the
// first start printed in the same log as the password (reset-admin ends it
// before it sets a new password), and reports whether there was one. The
// first-run row names it (deletePrintedCode), so a code the admin rotated in the
// console, which was never printed, stays; on an install whose row records no
// code (from before the row, or before it named the code) it is found by its
// shape: an invite of id's labelled InitialAdminCodeLabel with unlimited uses
// and no expiry, as only ensureAdmin makes one (an admin's invites carry the
// label they were given, and the console's always have limits by default).
// Devices it paired stay signed in unless their sessions end otherwise (a new
// password ends them); its unspent pairing tokens go with it (the
// tokens.auth_code_id cascade).
func (s *Service) RevokeInitialAdminCode(ctx context.Context, id int64) (bool, error) {
	var revoked bool
	err := s.db.WithDurableTx(ctx, "RevokeInitialAdminCode", func(tx *sql.Tx) error {
		fa, err := store.GetState[firstAdmin](ctx, tx, firstAdminKey)
		if err != nil {
			return err
		}
		if fa.CodeID != 0 {
			if fa.UserID == id {
				revoked, err = deletePrintedCode(ctx, tx, fa)
			}
			return err
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM auth_codes
			  WHERE user_id = ? AND kind = ? AND label = ? AND max_uses = 0 AND expires_at IS NULL`,
			id, CodeInvite, InitialAdminCodeLabel)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		revoked = n > 0
		return err
	})
	return revoked, err
}
