package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/relpath"
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
	raw, bad, dropped := AudibleSyncClaim("acct0001", many)
	var p AudibleSyncParams
	if err := json.Unmarshal(raw, &p); err != nil || dropped == 0 || len(bad) != 0 || len(raw) > maxAudibleSyncParamsBytes ||
		len(p.Titles)+dropped != len(many) || p.Titles[0] != many[0] {
		t.Fatalf("dropped %d, bad %d, %d bytes, %d titles, %v", dropped, len(bad), len(raw), len(p.Titles), err)
	}
	if raw, bad, dropped := AudibleSyncClaim("acct0001", nil); string(raw) != `{"account":"acct0001","titles":[]}` ||
		len(bad) != 0 || dropped != 0 {
		t.Fatalf("none = %s, %d, %d", raw, len(bad), dropped)
	}
}

// A title the Bridge's strict params would refuse the whole list for (the
// checks the queued params had while they held the titles) is left out of the
// claim and reported, the titles around it kept in order.
func TestAudibleSyncClaimLeavesOutBadTitles(t *testing.T) {
	t.Parallel()
	good := func(asin string) AudibleSyncTitle {
		return AudibleSyncTitle{ASIN: asin, LibraryID: 1, Path: "A/B", RuntimeS: 36000}
	}
	with := func(change func(*AudibleSyncTitle)) AudibleSyncTitle {
		title := good("B00000BAD0")
		change(&title)
		return title
	}
	titles := []AudibleSyncTitle{
		good("B000000001"),
		with(func(t *AudibleSyncTitle) { t.ASIN = "b00000bad0" }),
		with(func(t *AudibleSyncTitle) { t.ASIN = "" }),
		with(func(t *AudibleSyncTitle) { t.LibraryID = 0 }),
		with(func(t *AudibleSyncTitle) { t.Path = "" }),
		with(func(t *AudibleSyncTitle) { t.Path = "A/../B" }),
		with(func(t *AudibleSyncTitle) { t.Path = "/A/B" }),
		with(func(t *AudibleSyncTitle) { t.Path = strings.Repeat("a", relpath.MaxBytes+1) }),
		with(func(t *AudibleSyncTitle) { t.RuntimeS = -1 }),
		good("B000000001"), // an ASIN a second time
		{ASIN: "B000000002", LibraryID: 2, Path: ".", RuntimeS: 0},
	}
	raw, bad, dropped := AudibleSyncClaim("acct0001", titles)
	want := `{"account":"acct0001","titles":[{"asin":"B000000001","library_id":1,"path":"A/B","runtime_s":36000},` +
		`{"asin":"B000000002","library_id":2,"path":".","runtime_s":0}]}`
	if string(raw) != want || len(bad) != len(titles)-2 || dropped != 0 {
		t.Fatalf("claim = %s, %d bad, %d dropped", raw, len(bad), dropped)
	}
	// What is left is what the queued params' check took.
	var p AudibleSyncParams
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	for _, title := range p.Titles {
		if !audibleSyncTitleOK(title) {
			t.Errorf("kept %+v", title)
		}
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
		raw, _, _ := AudibleSyncClaim(account, titles)
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
		raw, _, _ := AudibleSyncClaim(account, nil)
		return raw, nil
	}
	if got, err := q.claim(ctx, q.token, []string{KindAudibleSync}); err != nil || got == nil || got.ID != job.ID ||
		got.Attempts != 1 || string(got.Params) != `{"account":"acct0001","titles":[]}` {
		t.Fatalf("claim after = %+v, %v", got, err)
	}
}

// While the titles keep failing to read (a database fault), a sync is put back
// unspent only maxUnspentSyncFills claims in a row: the next failing claim
// leaves it claimed and undelivered, so the claim after it hands out the job
// behind it, each lapsed lease spends an attempt, and the last attempt's
// failing claim ends it, naming the titles (syncTitlesUnread), and hands out
// the job behind it rather than an error.
func TestAudibleSyncClaimFailsSpentInTheEnd(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	q.beat(t, KindVerify, KindAudibleSync)
	failing := errors.New("disk I/O error")
	logged := logTo(q)
	q.ClaimAudibleSync = func(_ context.Context, account string) (json.RawMessage, error) {
		if account == "acct0001" {
			return nil, failing
		}
		raw, _, _ := AudibleSyncClaim(account, nil)
		return raw, nil
	}
	job, _, err := q.Enqueue(ctx, AudibleSync("acct0001", -10))
	if err != nil {
		t.Fatal(err)
	}
	behind, _, err := q.Enqueue(ctx, AudibleSync("acct0002", -20))
	if err != nil {
		t.Fatal(err)
	}
	failed := func(what, state string, attempts int) {
		t.Helper()
		if got, err := q.claim(ctx, q.token, []string{KindAudibleSync}); !errors.Is(err, failing) || got != nil {
			t.Fatalf("%s: claim = %+v, %v; want the error and no job", what, got, err)
		}
		if stored, err := q.Job(ctx, job.ID); err != nil || stored.State != state || stored.Attempts != attempts {
			t.Fatalf("%s: job = %+v, %v; want %s after %d attempts", what, stored, err, state, attempts)
		}
	}
	for range maxUnspentSyncFills {
		failed("a failed fill", StateQueued, 0)
	}
	failed("one more", StateClaimed, 1)
	// The job behind it is no longer held up.
	if got, err := q.claim(ctx, q.token, []string{KindAudibleSync}); err != nil || got == nil || got.ID != behind.ID {
		t.Fatalf("the claim after = %+v, %v; want the sync behind it", got, err)
	}
	if _, err := q.UpdateHeld(ctx, behind.ID, Update{Attempt: 1, State: ptr(StateDone)}); err != nil {
		t.Fatal(err)
	}
	// Each lapsed lease puts it back spent, and the next failing claim spends
	// another attempt at once; the last attempt's ends it there, naming the
	// server's read rather than the Bridge.
	for attempt := 2; attempt < MaxAttempts; attempt++ {
		q.advance(Lease + time.Second)
		failed("after a lapsed lease", StateClaimed, attempt)
	}
	if logged.Len() != 0 {
		t.Fatalf("logged before the last attempt: %s", logged)
	}
	behind, _, err = q.Enqueue(ctx, AudibleSync("acct0002", -20))
	if err != nil {
		t.Fatal(err)
	}
	q.advance(Lease + time.Second)
	if got, err := q.claim(ctx, q.token, []string{KindAudibleSync}); err != nil || got == nil || got.ID != behind.ID {
		t.Fatalf("the last attempt's claim = %+v, %v; want the sync behind it", got, err)
	}
	if stored, err := q.Job(ctx, job.ID); err != nil || stored.State != StateFailed || stored.Attempts != MaxAttempts ||
		stored.Error != syncTitlesUnread {
		t.Fatalf("in the end = %+v, %v; want it failed, the titles named as the reason", stored, err)
	}
	// The claim answered no error, so the read's is logged, once.
	if out := logged.String(); strings.Count(out, "\n") != 1 || !strings.Contains(out, "level=WARN") ||
		!strings.Contains(out, fmt.Sprintf("job=%d", job.ID)) || !strings.Contains(out, "account=acct0001") ||
		!strings.Contains(out, failing.Error()) {
		t.Fatalf("logged = %q; want one warning naming the job, the account and the read's error", out)
	}
	if _, err := q.UpdateHeld(ctx, behind.ID, Update{Attempt: 1, State: ptr(StateDone)}); err != nil {
		t.Fatal(err)
	}
	if got, err := q.claim(ctx, q.token, []string{KindAudibleSync}); err != nil || got != nil {
		t.Fatalf("the claim after = %+v, %v; want nothing left", got, err)
	}

	// A read that works again forgets the failures: the account's next sync is
	// put back unspent anew.
	q.ClaimAudibleSync = func(_ context.Context, account string) (json.RawMessage, error) {
		raw, _, _ := AudibleSyncClaim(account, nil)
		return raw, nil
	}
	next, _, err := q.Enqueue(ctx, AudibleSync("acct0001", -10))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := q.claim(ctx, q.token, []string{KindAudibleSync}); err != nil || got == nil || got.ID != next.ID {
		t.Fatalf("the next sync's claim = %+v, %v", got, err)
	}
	if n := len(q.syncFills); n != 0 {
		t.Fatalf("%d accounts still counted after a read that worked", n)
	}
}

// logTo points q's log at a buffer the test reads.
func logTo(q *testQueue) *bytes.Buffer {
	var buf bytes.Buffer
	q.Log = slog.New(slog.NewTextHandler(&buf, nil))
	return &buf
}

// A claimed audible.sync whose stored params can't be read can never be
// filled: neither put back unspent (every claim would take it again) nor left
// for its lease to spend every attempt, it ends at its first claim with its
// own reason (syncParamsUnread), and that claim hands out the job behind it.
func TestAudibleSyncClaimUnreadableParamsFailed(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	q.beat(t, KindVerify, KindAudibleSync)
	logged := logTo(q)
	q.ClaimAudibleSync = func(_ context.Context, account string) (json.RawMessage, error) {
		raw, _, _ := AudibleSyncClaim(account, nil)
		return raw, nil
	}
	job, _, err := q.Enqueue(ctx, AudibleSync("acct0001", -10))
	if err != nil {
		t.Fatal(err)
	}
	behind, _, err := q.Enqueue(ctx, AudibleSync("acct0002", -20))
	if err != nil {
		t.Fatal(err)
	}
	q.damageParams(t, job.ID)
	if got, err := q.claim(ctx, q.token, []string{KindAudibleSync}); err != nil || got == nil || got.ID != behind.ID {
		t.Fatalf("claim = %+v, %v; want the sync behind it", got, err)
	}
	if stored, err := q.Job(ctx, job.ID); err != nil || stored.State != StateFailed || stored.Attempts != 1 ||
		stored.Error != syncParamsUnread {
		t.Fatalf("after an unreadable fill = %+v, %v; want it failed at once, naming its params", stored, err)
	}
	if out := logged.String(); strings.Count(out, "\n") != 1 || !strings.Contains(out, "level=WARN") ||
		!strings.Contains(out, fmt.Sprintf("job=%d", job.ID)) {
		t.Fatalf("logged = %q; want one warning naming the job", out)
	}
}

// A queued audible.sync whose stored params are not JSON (a damaged row) runs
// with the next request's (a promoting kind's queued job takes the newest
// params): the queue's answer carries them, no longer flagged unreadable, and
// the claim hands the job out rather than ending it.
func TestAudibleSyncEnqueueReplacesUnreadableParams(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	q.beat(t, KindVerify, KindAudibleSync)
	job, _, err := q.Enqueue(ctx, AudibleSync("acct0001", -10))
	if err != nil {
		t.Fatal(err)
	}
	q.damageParams(t, job.ID)
	again, created, err := q.Enqueue(ctx, AudibleSync("acct0001", -10))
	if err != nil || created || again.ID != job.ID || again.ParamsUnreadable || string(again.Params) != `{"account":"acct0001"}` {
		t.Fatalf("queued again = %+v, %v, %v; want the open sync with the new params, unflagged", again, created, err)
	}
	if got, err := q.claim(ctx, q.token, []string{KindAudibleSync}); err != nil || got == nil || got.ID != job.ID ||
		got.State != StateClaimed {
		t.Fatalf("claim = %+v, %v; want the repaired sync handed out", got, err)
	}
}

// AudibleRefusalCode reads audible_refused from a failed sync's result code or
// its error's prefix, and from a cancelled one's result code alone (a failure
// reported after a cancel, UpdateHeld); nothing from a done one, a cancelled
// one's error, or another error.
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
		{Job{State: StateCancelled, Error: CancelAudibleSyncOff, Result: json.RawMessage(`{"code":"audible_refused"}`)}, SyncAudibleRefused},
		{Job{State: StateCancelled, Error: CancelAudibleSyncOff}, ""},
		{Job{State: StateDone, Result: json.RawMessage(`{"code":"audible_refused"}`)}, ""},
	} {
		if got := AudibleRefusalCode(c.job); got != c.want {
			t.Errorf("%+v = %q, want %q", c.job, got, c.want)
		}
	}
}
