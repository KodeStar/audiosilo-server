package bridge

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// An account keeps one ended poll, its newest: a poll every few hours would
// otherwise pile up in the job list. Another account's polls stay.
func TestAudiblePollsPruned(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	q.beat(t, KindVerify, KindAudiblePoll)
	poll := func(account string) int64 {
		t.Helper()
		job, created, err := q.Enqueue(ctx, AudiblePoll(account, "", 0))
		if err != nil || !created {
			t.Fatalf("poll %s = %+v, %v, %v", account, job, created, err)
		}
		return job.ID
	}
	finish := func(account string) int64 {
		t.Helper()
		id := poll(account)
		j := q.work(t, KindAudiblePoll, "")
		if j.ID != id {
			t.Fatalf("claimed %d, want %d", j.ID, id)
		}
		if _, err := q.UpdateHeld(ctx, id, Update{Attempt: j.Attempts, State: ptr(StateDone)}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	other := finish("acct0002")
	finish("acct0001")
	finish("acct0001")
	last := finish("acct0001")
	open := poll("acct0001")
	var ids []int64
	rows, err := q.db.QueryContext(ctx, `SELECT id FROM bridge_jobs WHERE kind = ? ORDER BY id`, KindAudiblePoll)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []int64{other, last, open}; !slices.Equal(ids, want) {
		t.Fatalf("polls kept = %v, want %v (the other account's, the newest ended, the open one)", ids, want)
	}
}

// A request for an open poll raises it to its own priority (Check now on the
// scheduler's poll), never lowers it; the server's reasons for cancelling a
// poll are its own (IsCancelReason), so a Bridge's report keeps them.
func TestAudiblePollPromotedAndCancelled(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	q.beat(t, KindVerify, KindAudiblePoll)
	scheduled, created, err := q.Enqueue(ctx, AudiblePoll("acct0001", "", -10))
	if err != nil || !created {
		t.Fatalf("scheduled = %+v, %v, %v", scheduled, created, err)
	}
	asked, created, err := q.Enqueue(ctx, AudiblePoll("acct0001", "", 0))
	if err != nil || created || asked.ID != scheduled.ID || asked.Priority != 0 {
		t.Fatalf("Check now = %+v, %v, %v; want the open poll at priority 0", asked, created, err)
	}
	if again, _, err := q.Enqueue(ctx, AudiblePoll("acct0001", "", -10)); err != nil || again.Priority != 0 {
		t.Fatalf("the scheduler again = %+v, %v; want priority kept at 0", again, err)
	}
	for _, reason := range []string{CancelAudibleRemoved, CancelAudiblePaused} {
		if !IsCancelReason(reason) {
			t.Errorf("IsCancelReason(%q) = false", reason)
		}
	}
}

// audible.poll's params carry the account's template (omitted when ""),
// decoded strictly; a template is bounded but never parsed, so one the grammar
// refuses still polls (the Bridge reports such a plan as an error).
func TestAudiblePollParams(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		raw, want string
		ok        bool
	}{
		{`{"account":"acct0001"}`, `{"account":"acct0001"}`, true},
		{`{"account":"acct0001","template":""}`, `{"account":"acct0001"}`, true},
		{`{"account":"acct0001","template":"{Author}/{Title}"}`, `{"account":"acct0001","template":"{Author}/{Title}"}`, true},
		{`{"account":"acct0001","template":"{Nope}"}`, `{"account":"acct0001","template":"{Nope}"}`, true},
		{`{"account":"acct0001","template":"` + strings.Repeat("a", MaxAudibleTemplate+1) + `"}`, "", false},
		{`{"account":"acct0001","template":7}`, "", false},
		{`{"account":"acct0001","extra":1}`, "", false},
		{`{"account":"BAD"}`, "", false},
	} {
		got, err := audiblePollParams(json.RawMessage(tc.raw))
		if (err == nil) != tc.ok || (tc.ok && string(got) != tc.want) {
			t.Errorf("audiblePollParams(%s) = %s, %v; want %s (ok %v)", tc.raw, got, err, tc.want, tc.ok)
		}
	}
	raw := AudiblePoll("acct0001", "{Author}/{Title}", 0).Params
	if string(raw) != `{"account":"acct0001","template":"{Author}/{Title}"}` {
		t.Fatalf("AudiblePoll params = %s", raw)
	}
	// One open poll an account, whatever its template.
	if audiblePollSuffix(AudiblePoll("acct0001", "a", 0)) != audiblePollSuffix(AudiblePoll("acct0001", "", 0)) {
		t.Fatal("the dedupe key depends on the template")
	}
}
