package auth

import (
	"context"
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

// firstAdminKey is the row's key in server_state (migration 0040).
const firstAdminKey = "first_admin"

// firstAdmin is the row's value.
type firstAdmin struct {
	// UserID is the admin ensureAdmin made.
	UserID int64 `json:"user_id"`
	// SignedIn is set by the first password sign-in of any admin (or an
	// admin's own password change): someone holds a working password.
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
// account gone since is nil too.
func (s *Service) FirstAdminWaiting(ctx context.Context) (*User, error) {
	fa, ok, err := s.readFirstAdmin(ctx)
	if err != nil || !ok || fa.SignedIn {
		return nil, err
	}
	u, err := s.GetUser(ctx, fa.UserID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return u, err
}

// NoteAdminSignIn records that an admin signed in with a password, ending the
// start's hint. It writes only the first time (a write moves the content
// generation), and nothing on a server without the row.
func (s *Service) NoteAdminSignIn(ctx context.Context) error {
	fa, ok, err := s.readFirstAdmin(ctx)
	if err != nil || !ok || fa.SignedIn {
		return err
	}
	fa.SignedIn = true
	return s.putFirstAdmin(ctx, fa)
}

// NoteOwnPassword records that admin id set their own password: signed in (as
// NoteAdminSignIn), and, when id is the first-run admin, the printed password
// replaced. Writes only what changes.
func (s *Service) NoteOwnPassword(ctx context.Context, id int64) error {
	fa, ok, err := s.readFirstAdmin(ctx)
	if err != nil || !ok {
		return err
	}
	next := fa
	next.SignedIn = true
	if fa.UserID == id {
		next.ChosePassword = true
	}
	if next == fa {
		return nil
	}
	return s.putFirstAdmin(ctx, next)
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
