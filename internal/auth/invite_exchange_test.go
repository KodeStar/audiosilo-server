package auth

import (
	"errors"
	"testing"
	"time"
)

const testInviteDevice = "device-random-key-0123456789"

func TestInviteReplay(t *testing.T) {
	t.Parallel()
	s, ctx, now := newTestServiceWithClock(t)
	user := mustUser(t, s, ctx, "listener", "", RoleUser)
	code, err := s.CreateAuthCode(ctx, user.ID, "one device", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	pair := pairThrough(t, s, ctx, code)
	otherPair := pairThrough(t, s, ctx, code)
	first, err := s.ConsumePairing(ctx, pair, "phone", testInviteDevice)
	if err != nil || first.Replayed || first.CodeKind != CodeInvite {
		t.Fatalf("first exchange: replayed %t, kind %q, %v", first.Replayed, first.CodeKind, err)
	}
	for _, tc := range []struct {
		token, device string
		want          error
	}{
		{pair, "other-device-123456", ErrCodeExhausted},
		{pair, "", ErrCodeExhausted},
		{otherPair, testInviteDevice, ErrCodeExhausted},
		{"unknown", testInviteDevice, ErrInvalidToken},
	} {
		if got, err := s.ConsumePairing(ctx, tc.token, "phone", tc.device); !errors.Is(err, tc.want) || got.Token != "" {
			t.Fatalf("different credential or device: %v, want %v and no session", err, tc.want)
		}
	}

	*now = now.Add(time.Minute)
	if _, err := s.PruneSingleUseTokens(ctx); err != nil {
		t.Fatal(err)
	}
	// A server restart between replies keeps the recovery. Two simultaneous
	// retries compete for the same allowance.
	s = New(s.db, func() time.Time { return *now })
	warm(t, s, first.Token, KindSession)
	type answer struct {
		session PairingSession
		err     error
	}
	ch := make(chan answer, 2)
	for range 2 {
		go func() {
			result, err := s.ConsumePairing(ctx, pair, "renamed phone", testInviteDevice)
			ch <- answer{result, err}
		}()
	}
	var replay string
	var denied int
	for range 2 {
		a := <-ch
		switch {
		case errors.Is(a.err, ErrInviteUsedByDevice) && a.session.Token == "":
			denied++
		case a.err == nil && a.session.Replayed && a.session.User.ID == user.ID:
			replay = a.session.Token
		default:
			t.Fatalf("race: replayed %t, %v", a.session.Replayed, a.err)
		}
	}
	if replay == "" || replay == first.Token || denied != 1 {
		t.Fatal("want one new bearer and one refusal")
	}
	if _, err := s.ResolveToken(ctx, first.Token, KindSession); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("original cached bearer still works: %v", err)
	}
	if u, err := s.ResolveToken(ctx, replay, KindSession); err != nil || u.ID != user.ID {
		t.Fatalf("rotated bearer did not authenticate the same account: %v", err)
	}
	var count, uses int
	var name string
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), device_name FROM tokens WHERE user_id = ? AND kind = 'session'`, user.ID).Scan(&count, &name); err != nil || count != 1 || name != "phone" {
		t.Fatalf("original device: %d sessions, name %q, %v", count, name, err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT uses FROM auth_codes WHERE user_id = ?`, user.ID).Scan(&uses); err != nil || uses != 1 {
		t.Fatalf("uses after replay: %d, %v", uses, err)
	}
}

func TestInviteReplayRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		elapsed, inviteTTL time.Duration
		revoke             bool
	}{
		{name: "after the minute", elapsed: 61 * time.Second},
		{name: "clock moved before the first exchange", elapsed: -time.Second},
		{name: "invite expired", elapsed: 30 * time.Second, inviteTTL: 30 * time.Second},
		{name: "original session signed out", revoke: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ctx, now := newTestServiceWithClock(t)
			u := mustUser(t, s, ctx, "listener", "", RoleUser)
			code, err := s.CreateAuthCode(ctx, u.ID, "invite", 2, tc.inviteTTL)
			if err != nil {
				t.Fatal(err)
			}
			pair := pairThrough(t, s, ctx, code)
			first, err := s.ConsumePairing(ctx, pair, "phone", testInviteDevice)
			if err != nil {
				t.Fatal(err)
			}
			if tc.revoke {
				if err := s.RevokeToken(ctx, first.Token); err != nil {
					t.Fatal(err)
				}
			}
			*now = now.Add(tc.elapsed)
			if got, err := s.ConsumePairing(ctx, pair, "phone", testInviteDevice); !errors.Is(err, ErrInviteUsedByDevice) || got.Token != "" {
				t.Fatalf("replay: %v, want refused and no bearer", err)
			}
		})
	}
}

func TestMultiUseInviteReplayRollback(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	u := mustUser(t, s, ctx, "listener", "", RoleUser)
	code, err := s.CreateAuthCode(ctx, u.ID, "invite", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	pair := pairThrough(t, s, ctx, code)
	first, err := s.ConsumePairing(ctx, pair, "phone", testInviteDevice)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_replay BEFORE UPDATE ON invite_exchanges
		BEGIN SELECT RAISE(ABORT,'cannot spend retry'); END`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ConsumePairing(ctx, pair, "phone", testInviteDevice); err == nil || Refused(err) || got.Token != "" {
		t.Fatalf("failed write: %v, want internal error with no bearer", err)
	}
	if _, err := s.ResolveToken(ctx, first.Token, KindSession); err != nil {
		t.Fatalf("failed replay invalidated original: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER fail_replay`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ConsumePairing(ctx, pair, "phone", testInviteDevice); err != nil || !got.Replayed {
		t.Fatalf("retry after rollback: %v", err)
	}
	if _, err := s.ConsumePairing(ctx, pair, "phone", testInviteDevice); !errors.Is(err, ErrInviteUsedByDevice) {
		t.Fatalf("third exchange spent a second use: %v", err)
	}
	if got, err := s.ConsumePairing(ctx, pair, "tablet", "another-random-device"); err != nil || got.Replayed {
		t.Fatalf("tablet's own use: %v", err)
	}
	if _, err := s.ResolveAuthCode(ctx, code); !errors.Is(err, ErrCodeExhausted) {
		t.Fatalf("invite after two devices: %v", err)
	}
}

// An exchange without a random key has no device binding a later request can
// prove, even when the later request supplies a key.
func TestInviteReplayNeedsOriginalDeviceID(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"", "short"} {
		s, ctx := newTestService(t)
		u := mustUser(t, s, ctx, "listener", "", RoleUser)
		code, err := s.CreateAuthCode(ctx, u.ID, "invite", 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		pair := pairThrough(t, s, ctx, code)
		if first, err := s.ConsumePairing(ctx, pair, "phone", id); err != nil || first.Replayed {
			t.Fatalf("original exchange: %v", err)
		}
		for _, retryID := range []string{id, testInviteDevice} {
			if retry, err := s.ConsumePairing(ctx, pair, "phone", retryID); !errors.Is(err, ErrCodeExhausted) || retry.Token != "" {
				t.Fatalf("retry acquired a device binding: %v", err)
			}
		}
	}
}
