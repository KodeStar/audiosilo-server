package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// audible.sync's queued params are strict: an account id and nothing else
// (server#303: the titles are the claim's); a list of titles, even an empty
// one, or any other field is refused.
func TestAudibleSyncParams(t *testing.T) {
	t.Parallel()
	got, err := audibleSyncParams(json.RawMessage(`{"account":"acct0001"}`))
	if err != nil || string(got) != `{"account":"acct0001"}` {
		t.Fatalf("an account = %s, %v", got, err)
	}
	for name, in := range map[string]string{
		"no account":    `{}`,
		"bad account":   `{"account":"Not-An-Id"}`,
		"titles":        `{"account":"acct0001","titles":[{"asin":"B000000001","library_id":1,"path":"A/B","runtime_s":0}]}`,
		"no titles":     `{"account":"acct0001","titles":[]}`,
		"unknown field": `{"account":"acct0001","user_id":2}`,
		"not an object": `[]`,
	} {
		if got, err := audibleSyncParams(json.RawMessage(in)); err == nil {
			t.Errorf("%s: %s accepted as %s", name, in, got)
		}
	}
}

// AudibleSyncClaim answers the account and its titles, leaving out the last
// ones of a list that would pass the bound; none is an empty list, never null.
func TestAudibleSyncClaim(t *testing.T) {
	t.Parallel()
	many := make([]AudibleSyncTitle, 20000)
	for i := range many {
		many[i] = AudibleSyncTitle{ASIN: fmt.Sprintf("B%09d", i), LibraryID: 1, Path: fmt.Sprintf("Author %d/Series/Book %d", i, i), RuntimeS: 36000}
	}
	raw, dropped := AudibleSyncClaim("acct0001", many)
	var p AudibleSyncParams
	if err := json.Unmarshal(raw, &p); err != nil || dropped == 0 || len(raw) > maxAudibleSyncParamsBytes ||
		len(p.Titles)+dropped != len(many) || p.Titles[0] != many[0] {
		t.Fatalf("dropped %d, %d bytes, %d titles, %v", dropped, len(raw), len(p.Titles), err)
	}
	if raw, dropped := AudibleSyncClaim("acct0001", nil); string(raw) != `{"account":"acct0001","titles":[]}` || dropped != 0 {
		t.Fatalf("none = %s, %d", raw, dropped)
	}
}

// server#303: a queued audible.sync stores the account alone, whatever the
// size of its title list, and the claim answers the titles as they are then
// (Service.ClaimAudibleSync), in the claim's shape; the job as stored keeps
// the small params. One open sync per account (a request for it is the open
// one), the newest ended one kept beside it.
func TestAudibleSyncQueued(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	q.beat(t, KindVerify, KindAudibleSync)
	titles := []AudibleSyncTitle{{ASIN: "B000000001", LibraryID: q.lib, Path: "A/B", RuntimeS: 36000}}
	var asked []string
	q.ClaimAudibleSync = func(_ context.Context, account string) (json.RawMessage, error) {
		asked = append(asked, account)
		raw, _ := AudibleSyncClaim(account, titles)
		return raw, nil
	}
	job, created, err := q.Enqueue(ctx, AudibleSync("acct0001", -10))
	if err != nil || !created || string(job.Params) != `{"account":"acct0001"}` {
		t.Fatalf("queued sync = %+v, %v, %v", job, created, err)
	}
	again, created, err := q.Enqueue(ctx, AudibleSync("acct0001", -10))
	if err != nil || created || again.ID != job.ID {
		t.Fatalf("a second sync = %+v, %v, %v; want the open one", again, created, err)
	}
	other, created, err := q.Enqueue(ctx, AudibleSync("acct0002", -10))
	if err != nil || !created || other.ID == job.ID {
		t.Fatalf("another account's sync = %+v, %v, %v", other, created, err)
	}

	// Claimed after the titles changed: the claim answers them as they are.
	titles = append(titles, AudibleSyncTitle{ASIN: "B000000002", LibraryID: q.lib, Path: "A/C", RuntimeS: 0})
	claimed, err := q.claim(ctx, q.token, []string{KindAudibleSync})
	if err != nil || claimed == nil || claimed.ID != job.ID {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	var p AudibleSyncParams
	if err := json.Unmarshal(claimed.Params, &p); err != nil || p.Account != "acct0001" || len(p.Titles) != 2 ||
		p.Titles[1].ASIN != "B000000002" || len(asked) != 1 || asked[0] != "acct0001" {
		t.Fatalf("claimed params = %s, %v (asked %v)", claimed.Params, err, asked)
	}
	if stored, err := q.Job(ctx, job.ID); err != nil || string(stored.Params) != `{"account":"acct0001"}` {
		t.Fatalf("stored params after the claim = %+v, %v", stored, err)
	}
	if ModuleFor(KindAudibleSync, nil) != ModuleAudible || !kinds[KindAudibleSync].internal || !kinds[KindAudibleSync].quiet {
		t.Fatal("audible.sync must be internal, quiet and need the audible module")
	}
	if !IsCancelReason(CancelAudibleSyncOff) {
		t.Fatal("CancelAudibleSyncOff is not a server reason")
	}
}

// A claim whose titles can't be read hands nothing out and puts the sync back
// in the queue unspent (no attempt counted), so the next claim takes it.
func TestAudibleSyncClaimFailsUnspent(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	q.beat(t, KindVerify, KindAudibleSync)
	failing := errors.New("database is locked")
	q.ClaimAudibleSync = func(context.Context, string) (json.RawMessage, error) { return nil, failing }
	job, _, err := q.Enqueue(ctx, AudibleSync("acct0001", -10))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := q.claim(ctx, q.token, []string{KindAudibleSync}); !errors.Is(err, failing) || got != nil {
		t.Fatalf("claim = %+v, %v; want the error and no job", got, err)
	}
	if stored, err := q.Job(ctx, job.ID); err != nil || stored.State != StateQueued || stored.Attempts != 0 {
		t.Fatalf("after a failed fill = %+v, %v; want it queued, unspent", stored, err)
	}
	q.ClaimAudibleSync = func(_ context.Context, account string) (json.RawMessage, error) {
		raw, _ := AudibleSyncClaim(account, nil)
		return raw, nil
	}
	if got, err := q.claim(ctx, q.token, []string{KindAudibleSync}); err != nil || got == nil || got.ID != job.ID ||
		got.Attempts != 1 || string(got.Params) != `{"account":"acct0001","titles":[]}` {
		t.Fatalf("claim after = %+v, %v", got, err)
	}
}

// AudibleRefusalCode reads audible_refused from a failed sync's result code or
// its error's prefix; nothing from a done or cancelled one, or another error.
func TestAudibleRefusalCode(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		job  Job
		want string
	}{
		{Job{State: StateFailed, Result: json.RawMessage(`{"code":"audible_refused"}`)}, SyncAudibleRefused},
		{Job{State: StateFailed, Error: "audible_refused: sign in again"}, SyncAudibleRefused},
		{Job{State: StateFailed, Error: "audible_refused"}, SyncAudibleRefused},
		{Job{State: StateFailed, Error: "audible_refusedx"}, ""},
		{Job{State: StateFailed, Error: "lastpositions: 503"}, ""},
		// A sign-in refused for now is not the device refused: an ordinary failure.
		{Job{State: StateFailed, Result: json.RawMessage(`{"code":"audible_sign_in"}`)}, SyncAudibleSignIn},
		{Job{State: StateFailed, Error: "audible_sign_in: try later"}, ""},
		{Job{State: StateCancelled, Error: "audible_refused: x"}, ""},
		{Job{State: StateDone, Result: json.RawMessage(`{"code":"audible_refused"}`)}, ""},
	} {
		if got := AudibleRefusalCode(c.job); got != c.want {
			t.Errorf("%+v = %q, want %q", c.job, got, c.want)
		}
	}
}
