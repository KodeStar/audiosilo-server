package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestCheckAudibleDestPath(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"", "Author/Series/Book.m4b", "Book.m4b", "A b/C, d (2)/x y.m4b"} {
		if err := CheckAudibleDestPath(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"../x.m4b", "a/../b.m4b", "/abs.m4b", "a//b.m4b", "a/./b.m4b", "a/", ".",
		".hidden/b.m4b", "a/.audiosilo/x", `a\b.m4b`, "a/b\x00.m4b", "a/b\n.m4b", "\xff.m4b", strings.Repeat("a", MaxAudibleDestPath+1)} {
		if err := CheckAudibleDestPath(bad); !errors.Is(err, ErrAudibleDestPath) {
			t.Errorf("%q: %v, want refused", bad, err)
		}
	}
}

// The server checks a template by the Bridge's placement grammar.
func TestCheckAudibleTemplate(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"", "{Author}/{Series}/{ { PaddedSeq } - }{Title}", "{Author}/{Title:upper,trim}",
		"{Author}/{PaddedSeq:width=3} {Title}{Ext}", `Books/\{literal\}/{ASIN}`, "{SHORTCODE}{Seq} - {TitleShort}",
		"{Narrator}/{Year}/{OriginalFilename}", "{ plain group }", "{Title:upper,,trim}", "{Author}/{Title}.m4b"} {
		if err := CheckAudibleTemplate(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	// A hidden part (the scan skips it) and an empty modifier name with a
	// value (the Bridge's parseMods refuses it) too.
	for _, bad := range []string{"{Nope}", "{Author", "{Author}}", "{ unclosed", "{Title:width}", "{Title:width=-1}",
		"{Title:bold}", "{Title:upper{", "/{Author}", " /x", "{Author}/../{Title}", "a\nb", strings.Repeat("x", MaxAudibleTemplate+1),
		"{Title:=x}", "{Title:upper, =1}", ".hidden/{Title}", "{Author}/.x/{Title}", `\.x/{Title}`, "{Author}/{Title}/.m4b"} {
		if err := CheckAudibleTemplate(bad); !errors.Is(err, ErrAudibleTemplate) {
			t.Errorf("%q: %v, want refused", bad, err)
		}
	}
}

func (q *testQueue) beatWritable(t *testing.T, writable bool, kinds ...string) {
	t.Helper()
	st := Status{Version: "0.4.0", Kinds: kinds, Libraries: []LibraryStatus{{LibraryID: q.lib, MountPath: "/m", Mapped: true, Writable: writable}}}
	if err := st.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := q.SaveHeartbeat(context.Background(), q.token, st); err != nil {
		t.Fatal(err)
	}
}

// audible.import: strict params built by the server, one open per account and
// title, a writing job (a library the Bridge can write to), internal and
// gated by the audible module; an account's removal cancels its open ones.
func TestAudibleImportJobs(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	p := AudibleImportParams{Account: "acct0001", ASIN: "B000000001", LibraryID: q.lib, DestPath: "A/B.m4b", Template: "{Author}/{Title}"}
	if _, _, err := q.Enqueue(ctx, AudibleImport(p, q.admin, 0)); !errors.Is(err, ErrKindUnavailable) {
		t.Fatalf("no Bridge offering it: %v", err)
	}
	q.beatWritable(t, false, KindVerify, KindAudibleImport)
	if _, _, err := q.Enqueue(ctx, AudibleImport(p, q.admin, 0)); !errors.Is(err, ErrLibraryNotWritable) {
		t.Fatalf("not writable: %v", err)
	}
	q.beatWritable(t, true, KindVerify, KindAudibleImport)
	first, created, err := q.Enqueue(ctx, AudibleImport(p, q.admin, 0))
	if err != nil || !created || *first.LibraryID != q.lib || first.Path != nil {
		t.Fatalf("queue = %+v, %t, %v", first, created, err)
	}
	if string(first.Params) != `{"account":"acct0001","asin":"B000000001","library_id":`+itoa(q.lib)+`,"dest_path":"A/B.m4b","template":"{Author}/{Title}"}` {
		t.Fatalf("params = %s", first.Params)
	}
	again, created, err := q.Enqueue(ctx, AudibleImport(p, q.admin, 0))
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("dedupe = %+v, %t, %v", again, created, err)
	}
	other := p
	other.ASIN = "B000000002"
	second, created, err := q.Enqueue(ctx, AudibleImport(other, q.admin, 0))
	if err != nil || !created || second.ID == first.ID {
		t.Fatalf("another title = %+v, %t, %v", second, created, err)
	}
	another := p
	another.Account = "acct0002"
	third, created, err := q.Enqueue(ctx, AudibleImport(another, q.admin, 0))
	if err != nil || !created {
		t.Fatalf("another account = %+v, %t, %v", third, created, err)
	}
	if ModuleFor(KindAudibleImport, nil) != ModuleAudible || !kinds[KindAudibleImport].internal || kinds[KindAudibleImport].quiet {
		t.Fatal("audible.import: internal, the audible module, notified")
	}
	for name, raw := range map[string]string{
		"unknown field": `{"account":"acct0001","asin":"B000000001","library_id":1,"x":1}`,
		"bad account":   `{"account":"ACCT","asin":"B000000001","library_id":1}`,
		"bad asin":      `{"account":"acct0001","asin":"b000000001","library_id":1}`,
		"no library":    `{"account":"acct0001","asin":"B000000001"}`,
		"bad dest":      `{"account":"acct0001","asin":"B000000001","library_id":1,"dest_path":"../x"}`,
		"bad template":  `{"account":"acct0001","asin":"B000000001","library_id":1,"template":"{Nope}"}`,
	} {
		if _, err := audibleImportParams(json.RawMessage(raw)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Removing acct0001 cancels both of its backups, not acct0002's.
	if err := q.db.WithTx(ctx, "t", func(tx *sql.Tx) error {
		return CancelAudibleImports(ctx, tx, "acct0001", CancelAudibleRemoved, *q.now)
	}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int64]string{first.ID: StateCancelled, second.ID: StateCancelled, third.ID: StateQueued} {
		if j, err := q.Job(ctx, id); err != nil || j.State != want {
			t.Errorf("job %d = %+v, %v, want %s", id, j, err, want)
		}
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// A failed backup's code: the result's, else a coded error; a park's code.
func TestAudibleImportCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		j    Job
		want string
	}{
		{Job{State: StateFailed, Result: json.RawMessage(`{"code":"exists","message":"a different file"}`)}, "exists"},
		{Job{State: StateFailed, Error: "disk_full: 2 GB free, 5 GB needed"}, "disk_full"},
		{Job{State: StateFailed, Error: "something else"}, ""},
		{Job{State: StateNeedsAttention, Progress: json.RawMessage(`{"park":{"code":"audible_refused"}}`)}, "audible_refused"},
		{Job{State: StateDone, Result: json.RawMessage(`{"code":"exists"}`)}, ""},
		{Job{State: StateRunning, Error: "exists: x"}, ""},
	} {
		if got := AudibleImportCode(tc.j); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.j, got, tc.want)
		}
	}
}
