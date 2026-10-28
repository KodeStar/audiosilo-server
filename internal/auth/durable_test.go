package auth

import (
	"fmt"
	"testing"
	"time"
)

// Every write that ends a credential commits durably (store.WithDurableTx, under
// synchronous=FULL): a power cut after it answered must not bring the old
// credential back. Each path is one durable commit, checked by the store's count.
func TestCredentialEndsAreDurable(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	n := 0
	user := func() *User {
		t.Helper()
		n++
		return mustUser(t, s, ctx, fmt.Sprintf("u%d", n), "user-password", RoleUser)
	}
	session := func(u *User) string {
		t.Helper()
		tok, err := s.IssueToken(ctx, u.ID, KindSession, "phone", 0)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	deviceID := func(u *User) int64 {
		t.Helper()
		session(u)
		d, err := s.ListDevices(ctx, u.ID)
		if err != nil || len(d) == 0 {
			t.Fatalf("devices %v %v", d, err)
		}
		return d[0].ID
	}
	member := func(owner *User) Member {
		t.Helper()
		n++
		return mustMember(t, s, ctx, owner.ID, NewMember{Name: fmt.Sprintf("Member %d", n), Kind: MemberGrownup, Access: MemberAll})
	}
	pairBridge(t, s, ctx, "boss")

	for name, op := range map[string]func() func() error{
		"sign-out (RevokeToken)": func() func() error {
			tok := session(user())
			return func() error { return s.RevokeToken(ctx, tok) }
		},
		"API key delete": func() func() error {
			u := user()
			_, meta, err := s.IssueAPIToken(ctx, u.ID, "key")
			if err != nil {
				t.Fatal(err)
			}
			return func() error { return s.RevokeTokenByID(ctx, u.ID, meta.ID) }
		},
		"admin device sign-out": func() func() error {
			id := deviceID(user())
			return func() error { return s.RevokeDevice(ctx, id) }
		},
		"own device sign-out": func() func() error {
			u := user()
			id := deviceID(u)
			return func() error { return s.RevokeOwnDevice(ctx, u.ID, id) }
		},
		"member device sign-out": func() func() error {
			owner := user()
			m := member(owner)
			if _, _, err := s.MemberSession(ctx, owner.ID, m.ID, "tablet", 0); err != nil {
				t.Fatal(err)
			}
			d, err := s.MemberDevices(ctx, owner.ID, m.ID)
			if err != nil || len(d) == 0 {
				t.Fatalf("member devices %v %v", d, err)
			}
			return func() error { _, err := s.RevokeMemberDevice(ctx, owner.ID, m.ID, d[0].ID); return err }
		},
		"member becomes a kid": func() func() error {
			owner := user()
			m := member(owner)
			kid := MemberKid
			return func() error { _, _, err := s.UpdateMember(ctx, owner.ID, m.ID, MemberUpdate{Kind: &kid}); return err }
		},
		"member delete": func() func() error {
			owner := user()
			m := member(owner)
			return func() error { _, err := s.DeleteMember(ctx, owner.ID, m.ID); return err }
		},
		"password change": func() func() error {
			u := user()
			return func() error { return s.SetPassword(ctx, u.ID, "another-password") }
		},
		"account disabled": func() func() error {
			u := user()
			yes := true
			return func() error { return s.UpdateUser(ctx, u.ID, UserChanges{Disabled: &yes}) }
		},
		"account deleted": func() func() error {
			u := user()
			return func() error { _, err := s.DeleteUser(ctx, u.ID, nil); return err }
		},
		"invite rotated": func() func() error {
			u := user()
			if _, err := s.CreateInvite(ctx, u.ID, "invite", 1, time.Hour); err != nil {
				t.Fatal(err)
			}
			id := mustOneInviteID(t, s, ctx, u.ID)
			return func() error { _, err := s.RotateAuthCode(ctx, id); return err }
		},
		"invite revoked": func() func() error {
			u := user()
			if _, err := s.CreateInvite(ctx, u.ID, "invite", 1, time.Hour); err != nil {
				t.Fatal(err)
			}
			id := mustOneInviteID(t, s, ctx, u.ID)
			return func() error { return s.RevokeAuthCode(ctx, id) }
		},
		"recovery code cleared": func() func() error {
			u := user()
			if _, err := s.GenerateRecoveryCode(ctx, u.ID); err != nil {
				t.Fatal(err)
			}
			return func() error { return s.ClearRecoveryCode(ctx, u.ID) }
		},
		"recovery code replaced": func() func() error {
			u := user()
			if _, err := s.GenerateRecoveryCode(ctx, u.ID); err != nil {
				t.Fatal(err)
			}
			return func() error { _, err := s.GenerateRecoveryCode(ctx, u.ID); return err }
		},
		"invite superseded by a new one": func() func() error {
			u := user()
			if _, err := s.CreateInvite(ctx, u.ID, "invite", 1, time.Hour); err != nil {
				t.Fatal(err)
			}
			return func() error { _, err := s.CreateInvite(ctx, u.ID, "invite", 1, time.Hour); return err }
		},
		"single-use pairing token used": func() func() error {
			u := user()
			tok, err := s.IssueToken(ctx, u.ID, KindPairing, "", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			return func() error { _, _, err := s.ConsumePairing(ctx, tok); return err }
		},
		"invite use claimed by a pairing": func() func() error {
			u := user()
			m, err := s.CreateInvite(ctx, u.ID, "invite", 1, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			rc, err := s.ResolveAuthCode(ctx, m.Code)
			if err != nil {
				t.Fatal(err)
			}
			tok, err := s.IssuePairingToken(ctx, rc)
			if err != nil {
				t.Fatal(err)
			}
			return func() error { _, _, err := s.ConsumePairing(ctx, tok); return err }
		},
		"Bridge pairing code replaced": func() func() error {
			admin := mustUser(t, s, ctx, "coder", "admin-password", RoleAdmin)
			return func() error { _, _, err := s.IssueBridgePairCode(ctx, admin.ID); return err }
		},
		"Bridge paired (earlier tokens revoked)": func() func() error {
			admin := mustUser(t, s, ctx, "pairer", "admin-password", RoleAdmin)
			code, _, err := s.IssueBridgePairCode(ctx, admin.ID)
			if err != nil {
				t.Fatal(err)
			}
			return func() error { _, err := s.ExchangeBridgeCode(ctx, code); return err }
		},
		"Bridge unpaired": func() func() error {
			return func() error { return s.UnpairBridge(ctx) }
		},
	} {
		run := op()
		before := s.db.DurableCommits()
		if err := run(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := s.db.DurableCommits() - before; got != 1 {
			t.Errorf("%s: %d durable commits, want 1", name, got)
		}
	}
}

// A listening-side write stays NORMAL: resolving a token (its last-seen touch)
// and issuing one are not durable commits.
func TestOrdinaryAuthWritesAreNotDurable(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	u := mustUser(t, s, ctx, "u", "user-password", RoleUser)
	before := s.db.DurableCommits()
	tok, err := s.IssueToken(ctx, u.ID, KindSession, "phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRequest(ctx, tok, Presence{}, KindSession); err != nil {
		t.Fatal(err)
	}
	if got := s.db.DurableCommits(); got != before {
		t.Fatalf("durable commits %d -> %d", before, got)
	}
}
