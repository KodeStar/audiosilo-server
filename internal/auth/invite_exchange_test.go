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
	// A cached bearer is one a request presented, which a replay never rotates;
	// clearing its touch (a user-state write: the cache stays warm) stands for
	// a replay landing between a request's lookup and its touch.
	warm(t, s, first.Token, KindSession)
	if _, err := s.db.ExecUserState(ctx, `UPDATE tokens SET last_seen = NULL WHERE token_hash = ?`, hashSecret(first.Token)); err != nil {
		t.Fatal(err)
	}
	if !cached(s, first.Token) {
		t.Fatal("clearing the touch emptied the token cache")
	}
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
	var count int
	var name string
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), device_name FROM tokens WHERE user_id = ? AND kind = 'session'`, user.ID).Scan(&count, &name); err != nil || count != 1 || name != "phone" {
		t.Fatalf("original device: %d sessions, name %q, %v", count, name, err)
	}
	if codes, err := s.ListAuthCodes(ctx, user.ID); err != nil || len(codes) != 1 || codes[0].Uses != 1 {
		t.Fatalf("uses after replay: %+v, %v", codes, err)
	}
}

func TestInviteReplayRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		elapsed, inviteTTL time.Duration
		revoke, used       bool
	}{
		{name: "after the minute", elapsed: 61 * time.Second},
		{name: "clock moved before the first exchange", elapsed: -time.Second},
		{name: "invite expired", elapsed: 30 * time.Second, inviteTTL: 30 * time.Second},
		{name: "original session signed out", revoke: true},
		// The answer was not lost: its bearer authenticated a request, so a
		// replay (another holder of the token and device key) takes nothing.
		{name: "original bearer already used", used: true},
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
			if tc.used {
				if _, err := s.ResolveToken(ctx, first.Token, KindSession); err != nil {
					t.Fatal(err)
				}
			}
			*now = now.Add(tc.elapsed)
			if got, err := s.ConsumePairing(ctx, pair, "phone", testInviteDevice); !errors.Is(err, ErrInviteUsedByDevice) || got.Token != "" {
				t.Fatalf("replay: %v, want refused and no bearer", err)
			}
			if tc.used {
				if _, err := s.ResolveToken(ctx, first.Token, KindSession); err != nil {
					t.Fatalf("refused replay ended the bearer in use: %v", err)
				}
			}
		})
	}
}

// A retry racing its own first exchange: the writer orders them, so one claims
// the invite's only use and the other rotates that session's bearer. One use,
// one device row, and only the later answer's bearer works.
func TestInviteReplayRacingFirstExchange(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	u := mustUser(t, s, ctx, "listener", "", RoleUser)
	code, err := s.CreateAuthCode(ctx, u.ID, "invite", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	pair := pairThrough(t, s, ctx, code)
	type answer struct {
		session PairingSession
		err     error
	}
	ch := make(chan answer, 2)
	for range 2 {
		go func() {
			result, err := s.ConsumePairing(ctx, pair, "phone", testInviteDevice)
			ch <- answer{result, err}
		}()
	}
	var first, replay PairingSession
	for range 2 {
		a := <-ch
		switch {
		case a.err != nil:
			t.Fatalf("racing exchange: %v", a.err)
		case a.session.Replayed:
			replay = a.session
		default:
			first = a.session
		}
	}
	if first.Token == "" || replay.Token == "" || first.CodeKind != CodeInvite {
		t.Fatalf("want one exchange and one replay: first %+v, replay %+v", first, replay)
	}
	if _, err := s.ResolveToken(ctx, first.Token, KindSession); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("the earlier answer's bearer still works: %v", err)
	}
	if _, err := s.ResolveToken(ctx, replay.Token, KindSession); err != nil {
		t.Fatalf("the later answer's bearer: %v", err)
	}
	var sessions int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens WHERE user_id = ? AND kind = 'session'`, u.ID).Scan(&sessions); err != nil || sessions != 1 {
		t.Fatalf("sessions: %d, %v", sessions, err)
	}
	if codes, err := s.ListAuthCodes(ctx, u.ID); err != nil || len(codes) != 1 || codes[0].Uses != 1 {
		t.Fatalf("uses: %+v, %v", codes, err)
	}
}

// The daily retention keeps a spent invite's pairing token through its open
// retry and deletes it, with its exchange record, once the minute is over.
func TestPruneAfterInviteReplayWindow(t *testing.T) {
	t.Parallel()
	s, ctx, now := newTestServiceWithClock(t)
	u := mustUser(t, s, ctx, "listener", "", RoleUser)
	code, err := s.CreateAuthCode(ctx, u.ID, "invite", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	pair := pairThrough(t, s, ctx, code)
	if _, err := s.ConsumePairing(ctx, pair, "phone", testInviteDevice); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneSingleUseTokens(ctx); err != nil || n != 0 {
		t.Fatalf("prune inside the minute: %d, %v", n, err)
	}
	*now = now.Add(61 * time.Second)
	if n, err := s.PruneSingleUseTokens(ctx); err != nil || n != 1 {
		t.Fatalf("prune after the minute: %d, %v", n, err)
	}
	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM invite_exchanges`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("exchange records after prune: %d, %v", rows, err)
	}
	if _, err := s.ConsumePairing(ctx, pair, "phone", testInviteDevice); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("pruned token: %v", err)
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
	// Peek: checking it without the touch that would make it a used bearer.
	if _, err := s.Peek(ctx, first.Token, KindSession); err != nil {
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

// A replay committing between a request's token check and its touch rotates a
// bearer that request checked: the touch finds the row holding another bearer,
// so the request is refused and nothing is authorized (or minted by row id)
// through the bearer the replay ended. The replay's own bearer works.
func TestInviteReplayBetweenCheckAndTouch(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	u := mustUser(t, s, ctx, "listener", "", RoleUser)
	code, err := s.CreateAuthCode(ctx, u.ID, "invite", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	pair := pairThrough(t, s, ctx, code)
	first, err := s.ConsumePairing(ctx, pair, "phone", testInviteDevice)
	if err != nil {
		t.Fatal(err)
	}
	var replay PairingSession
	var replayErr error
	s.beforeTouch = func() {
		s.beforeTouch = nil
		replay, replayErr = s.ConsumePairing(ctx, pair, "phone", testInviteDevice)
	}
	if got, _, err := s.ResolveRequest(ctx, first.Token, Presence{IP: "192.0.2.1"}, KindSession); !errors.Is(err, ErrInvalidToken) || got != nil {
		t.Fatalf("request on the rotated bearer: %v, want refused", err)
	}
	if replayErr != nil || !replay.Replayed {
		t.Fatalf("replay inside the request: %v", replayErr)
	}
	if got, _, err := s.ResolveRequest(ctx, replay.Token, Presence{}, KindSession); err != nil || got.ID != u.ID {
		t.Fatalf("replayed bearer: %v", err)
	}
	if _, err := s.ConsumePairing(ctx, pair, "phone", testInviteDevice); !errors.Is(err, ErrInviteUsedByDevice) {
		t.Fatalf("second replay: %v", err)
	}
}

// A request whose touch commits first marks the bearer presented, so a replay
// committing after it (still inside that request) is refused and the request's
// bearer keeps working.
func TestInviteReplayAfterTouch(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	u := mustUser(t, s, ctx, "listener", "", RoleUser)
	code, err := s.CreateAuthCode(ctx, u.ID, "invite", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	pair := pairThrough(t, s, ctx, code)
	first, err := s.ConsumePairing(ctx, pair, "phone", testInviteDevice)
	if err != nil {
		t.Fatal(err)
	}
	if _, cred, err := s.ResolveRequest(ctx, first.Token, Presence{}, KindSession); err != nil {
		t.Fatal(err)
	} else if _, _, err := s.IssueAPIToken(ctx, u.ID, cred.ID, "script"); err != nil {
		t.Fatalf("key minted on the presented session: %v", err)
	}
	if got, err := s.ConsumePairing(ctx, pair, "phone", testInviteDevice); !errors.Is(err, ErrInviteUsedByDevice) || got.Token != "" {
		t.Fatalf("replay after the touch: %v, want refused", err)
	}
	if _, err := s.ResolveToken(ctx, first.Token, KindSession); err != nil {
		t.Fatalf("presented bearer after the refused replay: %v", err)
	}
}

// A session's first touch is what marks it presented, so a failed one fails
// the request (not a refusal: the 503 of a token not checked) and leaves the
// bearer unauthorized, which a replay may then still rotate. A presented
// session's touch stays best effort.
func TestFirstTouchFailureFailsRequest(t *testing.T) {
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
	used, err := s.ConsumePairing(ctx, pairThrough(t, s, ctx, code), "tablet", "tablet-random-key-0123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveToken(ctx, used.Token, KindSession); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_touch BEFORE UPDATE OF last_seen ON tokens
		BEGIN SELECT RAISE(ABORT,'touch failed'); END`); err != nil {
		t.Fatal(err)
	}
	if got, _, err := s.ResolveRequest(ctx, first.Token, Presence{}, KindSession); err == nil || errors.Is(err, ErrInvalidToken) || got != nil {
		t.Fatalf("first request with a failed touch: %v, want an unchecked token", err)
	}
	// A new address makes the presented bearer's request touch it.
	if got, _, err := s.ResolveRequest(ctx, used.Token, Presence{IP: "192.0.2.7"}, KindSession); err != nil || got.ID != u.ID {
		t.Fatalf("presented bearer with a failed touch: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER fail_touch`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ConsumePairing(ctx, pair, "phone", testInviteDevice); err != nil || !got.Replayed {
		t.Fatalf("replay of the never-authorized bearer: %v", err)
	}
}
