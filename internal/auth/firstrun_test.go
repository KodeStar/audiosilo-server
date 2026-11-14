package auth

import (
	"context"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// The first-run admin's row: waiting until an admin signs in, the printed
// password until the admin sets their own, and each note written once (a
// write moves the content generation, which empties the caches keyed on it).
// No row (the --setup wizard's admin, an older install): nothing waits,
// nothing was printed, and the notes write nothing.
func TestFirstAdminState(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	s := New(db, time.Now)
	admin, err := s.CreateUser(ctx, "admin", "first-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	gen := db.ContentGeneration()
	if err := s.NoteAdminSignIn(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.NoteOwnPassword(ctx, admin.ID); err != nil {
		t.Fatal(err)
	}
	if db.ContentGeneration() != gen {
		t.Error("a note without the row wrote")
	}
	if u, _ := s.FirstAdminWaiting(ctx); u != nil {
		t.Error("waiting without the row")
	}
	if p, _ := s.PrintedPassword(ctx, admin.ID); p {
		t.Error("printed password without the row")
	}

	if err := s.MarkFirstAdmin(ctx, admin.ID); err != nil {
		t.Fatal(err)
	}
	if u, err := s.FirstAdminWaiting(ctx); err != nil || u == nil || u.ID != admin.ID {
		t.Fatalf("FirstAdminWaiting = %+v, %v; want the admin", u, err)
	}
	if p, _ := s.PrintedPassword(ctx, admin.ID); !p {
		t.Error("no printed password after MarkFirstAdmin")
	}
	if err := s.NoteAdminSignIn(ctx); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.FirstAdminWaiting(ctx); u != nil {
		t.Error("still waiting after a sign-in")
	}
	if p, _ := s.PrintedPassword(ctx, admin.ID); !p {
		t.Error("a sign-in replaced the printed password")
	}
	gen = db.ContentGeneration()
	if err := s.NoteAdminSignIn(ctx); err != nil {
		t.Fatal(err)
	}
	if db.ContentGeneration() != gen {
		t.Error("a second sign-in wrote again")
	}
	other, err := s.CreateUser(ctx, "alex", "alex-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	gen = db.ContentGeneration()
	if err := s.NoteOwnPassword(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	if db.ContentGeneration() != gen {
		t.Error("another admin's own password wrote")
	}
	if err := s.NoteOwnPassword(ctx, admin.ID); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.PrintedPassword(ctx, admin.ID); p {
		t.Error("printed password after the admin set their own")
	}
	// The account gone: nothing to wait for.
	if err := s.MarkFirstAdmin(ctx, 9999); err != nil {
		t.Fatal(err)
	}
	if u, err := s.FirstAdminWaiting(ctx); err != nil || u != nil {
		t.Errorf("FirstAdminWaiting for a deleted account = %+v, %v", u, err)
	}
}
