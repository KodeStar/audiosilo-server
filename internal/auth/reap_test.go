package auth

import (
	"testing"
	"time"
)

func TestReapIdleDemoUsers(t *testing.T) {
	s, ctx := newTestService(t)

	old, _ := s.CreateDemoUser(ctx, DemoUsernamePrefix+"old")
	fresh, _ := s.CreateDemoUser(ctx, DemoUsernamePrefix+"fresh")
	keeper, _ := s.CreateUser(ctx, "real_user", "", RoleUser)
	// A real account an admin happened to name with the demo prefix: it is NOT
	// is_demo, so the reaper must never touch it even when idle (the flag, not the
	// username, decides — this is the regression guard for reaping by prefix).
	prefixed, _ := s.CreateUser(ctx, DemoUsernamePrefix+"realadmin", "", RoleUser)

	for _, id := range []int64{old.ID, fresh.ID, keeper.ID, prefixed.ID} {
		if _, err := s.IssueToken(ctx, id, KindSession, "d", 0); err != nil {
			t.Fatal(err)
		}
	}
	// An auth code on the old demo user lets us confirm child rows cascade.
	if _, err := s.CreateAuthCode(ctx, old.ID, "x", 0, 0); err != nil {
		t.Fatal(err)
	}

	// Age the old demo user's, the real user's, and the prefixed real user's
	// activity past the cutoff.
	stale := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	for _, id := range []int64{old.ID, keeper.ID, prefixed.ID} {
		if _, err := s.db.ExecContext(ctx, `UPDATE tokens SET last_seen = ? WHERE user_id = ?`, stale, id); err != nil {
			t.Fatal(err)
		}
	}

	// Only the flagged demo accounts are counted (not the prefix-named real one).
	if n, err := s.CountDemoUsers(ctx); err != nil || n != 2 {
		t.Fatalf("count = %d err=%v, want 2", n, err)
	}

	n, err := s.ReapIdleDemoUsers(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reaped %d, want 1 (only the idle demo user)", n)
	}

	if _, err := s.GetUser(ctx, old.ID); err != ErrNotFound {
		t.Fatalf("idle demo user should be deleted, got err=%v", err)
	}
	if _, err := s.GetUser(ctx, fresh.ID); err != nil {
		t.Fatalf("fresh demo user should survive: %v", err)
	}
	if _, err := s.GetUser(ctx, keeper.ID); err != nil {
		t.Fatalf("non-demo user must never be reaped (even when idle): %v", err)
	}
	if _, err := s.GetUser(ctx, prefixed.ID); err != nil {
		t.Fatalf("a real account named with the demo prefix must never be reaped: %v", err)
	}

	// Child rows of the deleted user cascade away.
	var tokens, codes int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens WHERE user_id = ?`, old.ID).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_codes WHERE user_id = ?`, old.ID).Scan(&codes); err != nil {
		t.Fatal(err)
	}
	if tokens != 0 || codes != 0 {
		t.Fatalf("cascade failed: %d tokens, %d auth_codes remain", tokens, codes)
	}
}
