package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// server#359: a sign-in in the same second as a job's claim can't be ordered
// against it (refusal_ended_at against claimed_at, whole seconds), so the
// refusal a sync or a poll claimed in that second brings back marks nothing:
// it is the old sign-in's, or can't be told from one that is. Claimed a
// second later, a refusal marks the account.
func TestAudibleRefusalInTheSignInsSecond(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, kind := range []string{bridge.KindAudibleSync, bridge.KindAudiblePoll} {
		s := newAudibleSyncEnv(t)
		s.beat(t, bridge.KindVerify, bridge.KindAudibleSync, bridge.KindAudiblePoll)
		open := func() bridge.Job {
			t.Helper()
			if kind == bridge.KindAudibleSync {
				s.queueSync(t, time.Now().Add(16*time.Minute))
			} else if resp, out := s.do(t, "POST", "/api/v1/admin/audible/accounts/"+testAccount+"/poll", s.adminTok, ""); resp.StatusCode != http.StatusCreated {
				t.Fatalf("Check now: %d %s", resp.StatusCode, out)
			}
			return s.claim(t, kind)
		}
		refused := func() bool {
			t.Helper()
			a, err := s.cat.AudibleAccount(ctx, testAccount)
			if err != nil {
				t.Fatal(err)
			}
			return a.RefusedAt != nil
		}
		job := open()
		if code, out := s.putAccount(t, testAccount, s.patID, s.lib.ID); code != http.StatusOK {
			t.Fatalf("%s: sign in again: %d %s", kind, code, out)
		}
		var ended string
		if err := s.db.QueryRowContext(ctx, `SELECT refusal_ended_at FROM audible_accounts WHERE account = ?`, testAccount).
			Scan(&ended); err != nil {
			t.Fatal(err)
		}
		signedIn, ok := catalog.ParseStamp(ended)
		if !ok {
			t.Fatalf("%s: refusal_ended_at %q", kind, ended)
		}
		s.claimedAt(t, job, signedIn) // the sign-in's own second
		s.finish(t, job, bridge.StateFailed, `{"code":"audible_refused"}`, "audible_refused")
		if refused() {
			t.Fatalf("%s: a refusal claimed in the sign-in's second marked the account", kind)
		}

		// Denied: claimed in the next second, a refusal marks the account.
		job = open()
		s.claimedAt(t, job, signedIn.Add(time.Second))
		s.finish(t, job, bridge.StateFailed, `{"code":"audible_refused"}`, "audible_refused")
		if !refused() {
			t.Fatalf("%s: a refusal claimed after the sign-in left the account unmarked", kind)
		}
	}
}

// An account PUT for the same person (the Bridge can't say whether it is a
// sign-in or an edit of the label) stops no sync the Bridge is working on, and
// records no reason on it; mapped to another person, the sync is cancelled.
func TestAudibleAccountPutKeepsAWorkingSync(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	ctx := context.Background()
	s.queueSync(t, time.Now())
	job := s.claim(t, bridge.KindAudibleSync)
	if code, out := s.putAccount(t, testAccount, s.patID, s.lib.ID); code != http.StatusOK {
		t.Fatalf("put again: %d %s", code, out)
	}
	if held, err := s.api.bridge.Job(ctx, job.ID); err != nil || held.CancelRequested || held.Error != "" {
		t.Fatalf("the working sync after a PUT for the same person = %+v, %v; want left alone", held, err)
	}

	// Denied: mapped to another person, the working sync is asked to stop.
	quinnID, _ := newUserWithSession(t, s.testEnv, "quinn", false)
	if code, out := s.putAccount(t, testAccount, quinnID, s.lib.ID); code != http.StatusOK {
		t.Fatalf("mapped to quinn: %d %s", code, out)
	}
	if held, err := s.api.bridge.Job(ctx, job.ID); err != nil || !held.CancelRequested || held.Error != bridge.CancelAudibleSyncOff {
		t.Fatalf("the working sync after a remap = %+v, %v; want asked to stop", held, err)
	}
}

// server#415: Audible's places carry Audible's own times, so a Bridge whose
// clock is off (its sent_at) doesn't move them across a restart here (the
// row's finish_changed_at, five minutes ago): a place heard two minutes ago
// applies with the Bridge's clock ten minutes ahead, and one heard six minutes
// ago is behind with its clock ten minutes behind.
func TestAudibleListeningDatedByAudible(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		heard     time.Duration // before now
		skew      time.Duration // the Bridge's clock against the server's
		status    string
		reason    string
		storedPos float64
	}{
		{"after the restart, Bridge ahead", 2 * time.Minute, 10 * time.Minute, "applied", "", 500},
		{"before the restart, Bridge behind", 6 * time.Minute, -10 * time.Minute, "not_applied", "behind", 100},
	} {
		s := newAudibleSyncEnv(t)
		wantSync(t, s.patListens(t, "pat-1", mistbornBook, 0, 100, "0"), "applied", "")
		restart := time.Now().Add(-5 * time.Minute)
		if _, err := s.db.ExecContext(context.Background(), `UPDATE progress SET finish_changed_at = ? WHERE user_id = ?`,
			catalog.FormatStamp(restart), s.patID); err != nil {
			t.Fatal(err)
		}
		res := s.postResultsAt(t, time.Now().Add(tc.skew), place("aud-1", s.lib.ID, mistbornBook, 500, tc.heard))
		wantSync(t, res[0], tc.status, tc.reason)
		if p := s.patProgress(t, mistbornBook); p.Position != tc.storedPos {
			t.Fatalf("%s: row %+v, want position %v", tc.name, p, tc.storedPos)
		}
	}
}
