package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// audible.sync's params are strict: an account id and titles each with an
// ASIN (once), a library and a clean book path (or the library root "."),
// and no negative runtime; unknown fields are refused. Allowed: a list, an
// empty one, none (stored as []).
func TestAudibleSyncParams(t *testing.T) {
	t.Parallel()
	ok := map[string]string{
		`{"account":"acct0001","titles":[{"asin":"B000000001","library_id":1,"path":"A/B","runtime_s":36000}]}`: `{"account":"acct0001","titles":[{"asin":"B000000001","library_id":1,"path":"A/B","runtime_s":36000}]}`,
		`{"account":"acct0001","titles":[{"asin":"B000000001","library_id":1,"path":".","runtime_s":0}]}`:       `{"account":"acct0001","titles":[{"asin":"B000000001","library_id":1,"path":".","runtime_s":0}]}`,
		`{"account":"acct0001","titles":[]}`: `{"account":"acct0001","titles":[]}`,
		`{"account":"acct0001"}`:             `{"account":"acct0001","titles":[]}`,
	}
	for in, want := range ok {
		got, err := audibleSyncParams(json.RawMessage(in))
		if err != nil || string(got) != want {
			t.Errorf("%s = %s, %v; want %s", in, got, err, want)
		}
	}
	title := func(asin, path string, lib, runtime int64) string {
		return fmt.Sprintf(`{"asin":%q,"library_id":%d,"path":%q,"runtime_s":%d}`, asin, lib, path, runtime)
	}
	for name, in := range map[string]string{
		"no account":     `{"titles":[]}`,
		"bad account":    `{"account":"Not-An-Id","titles":[]}`,
		"unknown field":  `{"account":"acct0001","titles":[],"user_id":2}`,
		"title field":    `{"account":"acct0001","titles":[{"asin":"B000000001","library_id":1,"path":"A/B","user_id":2}]}`,
		"bad asin":       `{"account":"acct0001","titles":[` + title("b01", "A/B", 1, 0) + `]}`,
		"asin twice":     `{"account":"acct0001","titles":[` + title("B000000001", "A/B", 1, 0) + "," + title("B000000001", "A/C", 1, 0) + `]}`,
		"no library":     `{"account":"acct0001","titles":[` + title("B000000001", "A/B", 0, 0) + `]}`,
		"no path":        `{"account":"acct0001","titles":[` + title("B000000001", "", 1, 0) + `]}`,
		"unclean path":   `{"account":"acct0001","titles":[` + title("B000000001", "A/../B", 1, 0) + `]}`,
		"leading slash":  `{"account":"acct0001","titles":[` + title("B000000001", "/A/B", 1, 0) + `]}`,
		"long path":      `{"account":"acct0001","titles":[` + title("B000000001", strings.Repeat("a", 4097), 1, 0) + `]}`,
		"negative":       `{"account":"acct0001","titles":[` + title("B000000001", "A/B", 1, -1) + `]}`,
		"not an object":  `[]`,
		"titles a value": `{"account":"acct0001","titles":"all"}`,
	} {
		if got, err := audibleSyncParams(json.RawMessage(in)); err == nil {
			t.Errorf("%s: %s accepted as %s", name, in, got)
		}
	}
}

// AudibleSync leaves out the last titles of a list whose params would pass
// the kind's bound, and the queue takes the rest; one open sync per account,
// a request for a queued one gives it the newest titles, the newest ended one
// kept beside it.
func TestAudibleSyncQueued(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	q.beat(t, KindVerify, KindAudibleSync)
	many := make([]AudibleSyncTitle, 20000)
	for i := range many {
		many[i] = AudibleSyncTitle{ASIN: fmt.Sprintf("B%09d", i), LibraryID: 1, Path: fmt.Sprintf("Author %d/Series/Book %d", i, i), RuntimeS: 36000}
	}
	job, dropped := AudibleSync("acct0001", many, -10)
	if dropped == 0 || len(job.Params) > maxAudibleSyncParamsBytes {
		t.Fatalf("dropped %d, params %d bytes", dropped, len(job.Params))
	}
	big, created, err := q.Enqueue(ctx, job)
	if err != nil || !created {
		t.Fatalf("big sync = %v, %v", created, err)
	}
	small, _ := AudibleSync("acct0001", many[:1], -10)
	again, created, err := q.Enqueue(ctx, small)
	if err != nil || created || again.ID != big.ID {
		t.Fatalf("a second sync = %+v, %v, %v; want the open one", again, created, err)
	}
	var p AudibleSyncParams
	if err := json.Unmarshal(again.Params, &p); err != nil || len(p.Titles) != 1 {
		t.Fatalf("the queued sync's params = %d titles, %v; want the newest request's", len(p.Titles), err)
	}
	other, _ := AudibleSync("acct0002", nil, -10)
	if j, created, err := q.Enqueue(ctx, other); err != nil || !created || j.ID == big.ID {
		t.Fatalf("another account's sync = %+v, %v, %v", j, created, err)
	}
	if ModuleFor(KindAudibleSync, nil) != ModuleAudible || !kinds[KindAudibleSync].internal || !kinds[KindAudibleSync].quiet {
		t.Fatal("audible.sync must be internal, quiet and need the audible module")
	}
	if !IsCancelReason(CancelAudibleSyncOff) {
		t.Fatal("CancelAudibleSyncOff is not a server reason")
	}
}

// AudibleSyncCode reads audible_refused from a failed sync's result code or
// its error's prefix; nothing from a done or cancelled one, or another error.
func TestAudibleSyncCode(t *testing.T) {
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
		{Job{State: StateCancelled, Error: "audible_refused: x"}, ""},
		{Job{State: StateDone, Result: json.RawMessage(`{"code":"audible_refused"}`)}, ""},
	} {
		if got := AudibleSyncCode(c.job); got != c.want {
			t.Errorf("%+v = %q, want %q", c.job, got, c.want)
		}
	}
}
