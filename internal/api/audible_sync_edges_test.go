package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// server#359: a sign-in in the same second as a sync's claim can't be ordered
// by refusal_ended_at (whole seconds), so the sign-in asks the Bridge to stop
// the sync it holds, and that sync's refusal marks nothing. A sync claimed
// after the sign-in (none held then) that Audible refuses still marks the
// account.
func TestAudibleSyncRefusalInTheSignInsSecond(t *testing.T) {
	t.Parallel()
	s := newAudibleSyncEnv(t)
	ctx := context.Background()
	now := time.Now()
	s.queueSync(t, now)
	job := s.claim(t, bridge.KindAudibleSync)
	if _, err := s.db.ExecContext(ctx, `UPDATE bridge_jobs SET claimed_at = ? WHERE id = ?`,
		now.UTC().Format(time.RFC3339), job.ID); err != nil {
		t.Fatal(err)
	}
	if code, out := s.putAccount(t, testAccount, s.patID, s.lib.ID); code != http.StatusOK {
		t.Fatalf("sign in again: %d %s", code, out)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE audible_accounts SET refusal_ended_at = ? WHERE account = ?`,
		now.UTC().Format(time.RFC3339Nano), testAccount); err != nil {
		t.Fatal(err)
	}
	held, err := s.api.bridge.Job(ctx, job.ID)
	if err != nil || !held.CancelRequested || held.Error != bridge.CancelAudibleSignedIn {
		t.Fatalf("the held sync after a sign-in = %+v, %v; want asked to stop", held, err)
	}
	s.finish(t, job, bridge.StateFailed, `{"code":"audible_refused"}`, "audible_refused")
	if a, _ := s.cat.AudibleAccount(ctx, testAccount); a.RefusedAt != nil {
		t.Fatalf("a refusal in the sign-in's second marked the account: %+v", a)
	}

	// Denied: claimed after the sign-in, a refusal marks the account.
	s.queueSync(t, now.Add(16*time.Minute))
	job = s.claim(t, bridge.KindAudibleSync)
	s.finish(t, job, bridge.StateFailed, `{"code":"audible_refused"}`, "audible_refused")
	if a, _ := s.cat.AudibleAccount(ctx, testAccount); a.RefusedAt == nil {
		t.Fatalf("a refusal after the sign-in left the account unmarked: %+v", a)
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
		pos       float64
		status    string
		reason    string
		storedPos float64
	}{
		{"after the restart, Bridge ahead", 2 * time.Minute, 10 * time.Minute, 500, "applied", "", 500},
		{"before the restart, Bridge behind", 6 * time.Minute, -10 * time.Minute, 500, "not_applied", "behind", 100},
	} {
		s := newAudibleSyncEnv(t)
		wantSync(t, s.patListens(t, "pat-1", mistbornBook, 0, 100, "0"), "applied", "")
		restart := time.Now().Add(-5 * time.Minute)
		if _, err := s.db.ExecContext(context.Background(), `UPDATE progress SET finish_changed_at = ? WHERE user_id = ?`,
			catalog.FormatStamp(restart), s.patID); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"sent_at":%q,"events":[%s]}`, time.Now().Add(tc.skew).UTC().Format(time.RFC3339Nano),
			strings.Join([]string{place("aud-1", s.lib.ID, mistbornBook, tc.pos, tc.heard)}, ","))
		resp, out := s.do(t, "POST", audibleListeningURL, s.bridgeTok, body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.name, resp.StatusCode, out)
		}
		var a catalog.SyncAnswer
		decodeInto(t, out, &a)
		wantSync(t, a.Results[0], tc.status, tc.reason)
		if p := s.patProgress(t, mistbornBook); p.Position != tc.storedPos {
			t.Fatalf("%s: row %+v, want position %v", tc.name, p, tc.storedPos)
		}
	}
}
