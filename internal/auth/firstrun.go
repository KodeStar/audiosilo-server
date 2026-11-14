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
// needs no backfill. Never a password: only who the admin is and two facts.

// InitialAdminCodeLabel labels the auth code the first start prints beside the
// admin's password (pkg/launcher ensureAdmin): unlimited uses, no expiry.
// RevokeInitialAdminCode finds it by it.
const InitialAdminCodeLabel = "initial admin code"

// firstAdminKey is the row's key in server_state (migration 0040).
const firstAdminKey = "first_admin"

// firstAdmin is the row's value.
type firstAdmin struct {
	// UserID is the admin ensureAdmin made.
	UserID int64 `json:"user_id"`
	// SignedIn is set by the first password sign-in of any admin, an admin's
	// own password change or a reset-admin: someone holds a working password.
	SignedIn bool `json:"signed_in"`
	// ChosePassword is set once UserID changes their own password, so the
	// printed one is gone (the console's Your account step words itself by it).
	ChosePassword bool `json:"chose_password"`
}

// MarkFirstAdmin records id as the admin the server made at first start with a
// printed password, not yet signed in.
func (s *Service) MarkFirstAdmin(ctx context.Context, id int64) error {
	return s.putFirstAdmin(ctx, firstAdmin{UserID: id})
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
// start's hint; reset-admin notes it too, for the password it just printed
// (the hint would still point at the first start's, which it replaced). It
// writes only the first time (a write moves the content generation), and
// nothing on a server without the row.
func (s *Service) NoteAdminSignIn(ctx context.Context) error {
	return s.noteFirstAdmin(ctx, func(fa *firstAdmin) { fa.SignedIn = true })
}

// NoteOwnPassword records that admin id set their own password: signed in (as
// NoteAdminSignIn), and, when id is the first-run admin, the printed password
// replaced. Writes only what changes.
func (s *Service) NoteOwnPassword(ctx context.Context, id int64) error {
	return s.noteFirstAdmin(ctx, func(fa *firstAdmin) {
		fa.SignedIn = true
		if fa.UserID == id {
			fa.ChosePassword = true
		}
	})
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
// with the password), and reports whether there was one. It is found by its
// shape: an invite of id's labelled InitialAdminCodeLabel with unlimited uses
// and no expiry, as only ensureAdmin makes one (an admin's invites carry the
// label they were given, and the console's always have limits by default).
// Found that way it covers an install from before the first_admin row too. An
// admin who rotated it in the console has a code that was never printed: it
// goes all the same, since its row can't tell. Devices it paired stay signed
// in unless their sessions end otherwise; its unspent pairing tokens go with it
// (the tokens.auth_code_id cascade).
func (s *Service) RevokeInitialAdminCode(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecDurable(ctx, "RevokeInitialAdminCode",
		`DELETE FROM auth_codes
		  WHERE user_id = ? AND kind = ? AND label = ? AND max_uses = 0 AND expires_at IS NULL`,
		id, CodeInvite, InitialAdminCodeLabel)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
