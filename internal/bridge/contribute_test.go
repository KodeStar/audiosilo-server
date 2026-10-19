package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// Phase 2b: the setup checklist on the heartbeat, can_contribute's precedence,
// and the two community job kinds.

// decodeHeartbeat decodes a heartbeat body as the api does (unknown top-level
// fields refused) and validates it.
func decodeHeartbeat(t *testing.T, body string) Status {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	var st Status
	if err := dec.Decode(&st); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if err := st.Validate(); err != nil {
		t.Fatalf("validate %s: %v", body, err)
	}
	return st
}

// The setup block is read leniently and bounded: an unknown key is ignored, a
// block that doesn't decode is dropped (never the heartbeat), an item with a
// bad or repeated id is left out, an unknown id kept, details cleaned, library
// ids kept positive and once each, at most 16 items, and a login that isn't
// GitHub's shape dropped.
func TestSetupIsLenientAndBounded(t *testing.T) {
	t.Parallel()
	st := decodeHeartbeat(t, `{"version":"1","setup":{"items":[
		{"id":"paired","done":true,"required":true,"future":1},
		{"id":"libraries","done":false,"required":true,"detail":"1 of 2\nwritable","library_ids":[3,3,-1,0,4]},
		{"id":"Bad-Id","done":true},
		{"id":"paired","done":false},
		{"id":"telescope","done":false,"required":false}
	],"can_contribute":true,"github_login":"octo-cat","unknown":{"x":1}}}`)
	s := st.Setup
	if s == nil || !s.CanContribute || s.GitHubLogin != "octo-cat" || len(s.Items) != 3 {
		t.Fatalf("setup = %+v", s)
	}
	if lib := s.Items[1]; lib.ID != "libraries" || lib.Detail != "1 of 2 writable" || !slices.Equal(lib.LibraryIDs, []int64{3, 4}) {
		t.Fatalf("libraries item = %+v", lib)
	}
	if s.Items[0].ID != "paired" || !s.Items[0].Done || s.Items[2].ID != "telescope" {
		t.Fatalf("items = %+v", s.Items)
	}

	// Denied, never refused: a block of the wrong shape is dropped whole.
	for name, block := range map[string]string{
		"items not a list":      `{"items":{"id":"paired"}}`,
		"can_contribute string": `{"can_contribute":"yes"}`,
		"not an object":         `[1,2]`,
	} {
		if st := decodeHeartbeat(t, `{"version":"1","setup":`+block+`}`); st.Setup != nil {
			t.Errorf("%s: kept %+v", name, st.Setup)
		}
	}
	if st := decodeHeartbeat(t, `{"version":"1","setup":null}`); st.Setup != nil {
		t.Errorf("null setup kept: %+v", st.Setup)
	}

	// Bounds: 16 items, a detail of 256 bytes, a bad login dropped.
	var items []string
	for i := range maxSetupItems + 4 {
		items = append(items, fmt.Sprintf(`{"id":"step_%d","detail":%q}`, i, strings.Repeat("d", 400)))
	}
	st = decodeHeartbeat(t, `{"setup":{"items":[`+strings.Join(items, ",")+`],"github_login":"-bad login"}}`)
	if len(st.Setup.Items) != maxSetupItems || len(st.Setup.Items[0].Detail) != maxSetupDetail || st.Setup.GitHubLogin != "" {
		t.Fatalf("bounded = %d items, detail %d, login %q", len(st.Setup.Items), len(st.Setup.Items[0].Detail), st.Setup.GitHubLogin)
	}
	if b, _ := json.Marshal(st.Setup); bytes.Contains(b, []byte("github_login")) {
		t.Fatalf("an empty login is sent: %s", b)
	}
}

// can_contribute: the setup block's when the Bridge sent one (either way),
// else the estimates' (Phase 2a), else false; the contribute feature follows.
func TestCanContributePrecedence(t *testing.T) {
	t.Parallel()
	yes := &Estimates{Sidecars: &SidecarsEstimates{CanContribute: true}}
	no := &Estimates{Sidecars: &SidecarsEstimates{}}
	for name, c := range map[string]struct {
		st   Status
		want bool
	}{
		"neither":                {Status{}, false},
		"estimates only, yes":    {Status{Estimates: yes}, true},
		"estimates only, no":     {Status{Estimates: no}, false},
		"setup no over yes":      {Status{Estimates: yes, Setup: &Setup{}}, false},
		"setup yes over no":      {Status{Estimates: no, Setup: &Setup{CanContribute: true}}, true},
		"setup yes, no estimate": {Status{Setup: &Setup{CanContribute: true}}, true},
	} {
		if got := c.st.CanContribute(); got != c.want {
			t.Errorf("%s: CanContribute = %t, want %t", name, got, c.want)
		}
		if got := slices.Contains(c.st.Features(), FeatureContribute); got != c.want {
			t.Errorf("%s: contribute feature = %t, want %t", name, got, c.want)
		}
	}
}

// beatSetup records a heartbeat offering kinds on the test library (mapped,
// writable as given) whose setup says can.
func (q *testQueue) beatSetup(t *testing.T, writable, can bool, kinds ...string) {
	t.Helper()
	q.beatStatus(t, Status{Version: "0.3.0", Kinds: kinds,
		Libraries: []LibraryStatus{{LibraryID: q.lib, MountPath: "/media/books", Mapped: true, Writable: writable}},
		Setup:     &Setup{CanContribute: can}})
}

func (q *testQueue) contribute(params string, path *string) (*Job, bool, error) {
	j := NewJob{Kind: KindContribute, LibraryID: &q.lib, Path: path, RequestedBy: q.admin}
	if params != "" {
		j.Params = json.RawMessage(params)
	}
	return q.Enqueue(context.Background(), j)
}

func (q *testQueue) correct(params string, path *string) (*Job, bool, error) {
	j := NewJob{Kind: KindCorrect, LibraryID: &q.lib, Path: path, RequestedBy: q.admin, Params: json.RawMessage(params)}
	return q.Enqueue(context.Background(), j)
}

// sidecars.contribute: strict params with both dimensions by default, a book
// required, a Bridge that runs it and can contribute; it writes nothing, so a
// read-only library is fine; a second press is the open job.
func TestContributeQueueRules(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	book := ptr("Author/Book")
	q.beatSetup(t, false, true, KindVerify, KindSidecars)
	if _, _, err := q.contribute("", book); !errors.Is(err, ErrKindUnavailable) {
		t.Fatalf("a Bridge without the kind: %v, want ErrKindUnavailable", err)
	}
	q.beatSetup(t, false, false, KindVerify, KindSidecars, KindContribute)
	if _, _, err := q.contribute("", book); !errors.Is(err, ErrCannotContribute) {
		t.Fatalf("a Bridge that can't contribute: %v, want ErrCannotContribute", err)
	}
	q.beatSetup(t, false, true, KindVerify, KindSidecars, KindContribute)
	job, created, err := q.contribute("", book)
	if err != nil || !created || string(job.Params) != `{"dimensions":["characters","recaps"]}` {
		t.Fatalf("defaults (read-only library): %v, %v, %+v", err, created, job)
	}
	if again, created, err := q.contribute(`{"dimensions":["recaps"]}`, book); err != nil || created || again.ID != job.ID {
		t.Fatalf("second press = %+v, %v, %v", again, created, err)
	}
	for params, want := range map[string]string{
		`{"dimensions":["recaps","characters"],"work_id":"mistborn"}`: `{"work_id":"mistborn","dimensions":["characters","recaps"]}`,
		`{"dimensions":["recaps"]}`:                                   `{"dimensions":["recaps"]}`,
		`{"dimensions":[]}`:                                           `{"dimensions":["characters","recaps"]}`,
	} {
		got, err := contributeParams(json.RawMessage(params))
		if err != nil || string(got) != want {
			t.Errorf("%s = %s, %v; want %s", params, got, err, want)
		}
	}
	for name, params := range map[string]string{
		"unknown field":     `{"source":"audio"}`,
		"unknown dimension": `{"dimensions":["description"]}`,
		"twice":             `{"dimensions":["recaps","recaps"]}`,
		"work id control":   `{"work_id":"a\nb"}`,
		"work id too long":  `{"work_id":"` + strings.Repeat("w", maxWorkIDLen+1) + `"}`,
	} {
		if _, _, err := q.contribute(params, ptr("Author/Other")); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if _, _, err := q.contribute("", nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("no path: %v, want ErrInvalid", err)
	}
	if got := (Status{Kinds: []string{KindContribute, KindCorrect}}).Features(); !slices.Equal(got, []string{"meta", "sidecars"}) {
		t.Errorf("features = %v", got)
	}
}

// A contribute job files what the last generate run made: it is refused while
// a generate job is open for the book, and a generate job is refused while a
// contribute job is queued or working (it would replace what is being filed),
// while a parked one gives way and is cancelled.
func TestContributeAndGenerate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := newTestQueue(t)
	q.beatSetup(t, true, true, KindVerify, KindSidecars, KindContribute)
	book := ptr("Author/Book")

	gen, _, err := q.sidecars("", book)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.contribute("", book); !errors.Is(err, ErrGenerateOpen) {
		t.Fatalf("contribute with a generate open: %v, want ErrGenerateOpen", err)
	}
	if _, err := q.Cancel(ctx, gen.ID); err != nil {
		t.Fatal(err)
	}
	con, created, err := q.contribute("", book)
	if err != nil || !created {
		t.Fatalf("contribute once the generate ended: %v, %v", created, err)
	}
	// Denied: queued, then working.
	if _, _, err := q.sidecars("", book); !errors.Is(err, ErrContributeOpen) {
		t.Fatalf("generate with a queued contribute: %v, want ErrContributeOpen", err)
	}
	q.work(t, KindContribute, "")
	if _, _, err := q.sidecars("", book); !errors.Is(err, ErrContributeOpen) {
		t.Fatalf("generate with a running contribute: %v, want ErrContributeOpen", err)
	}
	// Allowed once parked: the generate job is queued, the contribute cancelled.
	if _, err := q.UpdateHeld(ctx, con.ID, Update{Attempt: 1, State: ptr(StateNeedsAttention),
		Progress: json.RawMessage(`{"park":{"code":"github_refused"}}`)}); err != nil {
		t.Fatal(err)
	}
	if _, created, err := q.sidecars("", book); err != nil || !created {
		t.Fatalf("generate with a parked contribute: %v, %v", created, err)
	}
	if got, err := q.Job(ctx, con.ID); err != nil || got.State != StateCancelled || got.Error == "" {
		t.Fatalf("the parked contribute = %+v, %v; want cancelled with a reason", got, err)
	}
}

// A valid meta.correct params body (one title change).
const goodCorrection = `{"work_id":"the-final-empire","asin":"B002UZMLXM","region":"us",
	"changes":[{"field":"title","current":"The Final Empire ","corrected":" Mistborn: The Final Empire"}],
	"evidence":"The cover says so.\r\nSee the spine."}`

// meta.correct: strict params, trimmed and stored; 1 to 25 changes of the
// allowed fields, each once, bounded single-line values that change something;
// evidence required and bounded; a Bridge that runs it and can contribute.
func TestCorrectQueueRules(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	book := ptr("Author/Book")
	q.beatSetup(t, false, false, KindVerify, KindCorrect)
	if _, _, err := q.correct(goodCorrection, book); !errors.Is(err, ErrCannotContribute) {
		t.Fatalf("a Bridge that can't contribute: %v, want ErrCannotContribute", err)
	}
	q.beatSetup(t, false, true, KindVerify, KindCorrect)
	job, created, err := q.correct(goodCorrection, book)
	want := `{"work_id":"the-final-empire","asin":"B002UZMLXM","region":"us","changes":[{"field":"title","current":"The Final Empire","corrected":"Mistborn: The Final Empire"}],"evidence":"The cover says so.\nSee the spine."}`
	if err != nil || !created || string(job.Params) != want {
		t.Fatalf("queue (read-only library) = %v, %v, %s", err, created, job.Params)
	}

	change := func(field, current, corrected string) string {
		return fmt.Sprintf(`{"field":%q,"current":%q,"corrected":%q}`, field, current, corrected)
	}
	body := func(changes ...string) string {
		return `{"work_id":"w","changes":[` + strings.Join(changes, ",") + `],"evidence":"e"}`
	}
	var many []string
	for range MaxCorrections + 1 {
		many = append(many, change("title", "a", "b"))
	}
	for name, params := range map[string]string{
		"unknown field":         `{"work_id":"w","changes":[` + change("title", "a", "b") + `],"evidence":"e","x":1}`,
		"unknown change field":  body(`{"field":"title","current":"a","corrected":"b","why":"x"}`),
		"no work":               `{"changes":[` + change("title", "a", "b") + `],"evidence":"e"}`,
		"no changes":            body(),
		"too many changes":      body(many...),
		"a field it can't take": body(change("description", "a", "b")),
		"a field twice":         body(change("title", "a", "b"), change("title", "c", "d")),
		"no corrected value":    body(change("title", "a", " ")),
		"no change":             body(change("title", "a", "a ")),
		"a value too long":      body(change("title", "a", strings.Repeat("é", maxCorrectionValue+1))),
		"a value over lines":    body(change("title", "a", "b\nc")),
		"no evidence":           `{"work_id":"w","changes":[` + change("title", "a", "b") + `],"evidence":"  "}`,
		"evidence too long":     `{"work_id":"w","changes":[` + change("title", "a", "b") + `],"evidence":"` + strings.Repeat("e", maxEvidence+1) + `"}`,
		"evidence control":      `{"work_id":"w","changes":[` + change("title", "a", "b") + `],"evidence":"a\u001bb"}`,
		"asin too long":         `{"work_id":"w","asin":"` + strings.Repeat("A", maxCorrectIDLen+1) + `","changes":[` + change("title", "a", "b") + `],"evidence":"e"}`,
		"not an object":         `[]`,
		// What the Bridge would refuse when it files them is refused now.
		"a C1 control":            body(change("title", "a\u0085b", "c")),
		"recording id not a slug": `{"work_id":"w","recording_id":"Not A Slug","changes":[` + change("title", "a", "b") + `],"evidence":"e"}`,
		"evidence too long as filed": `{"work_id":"w","changes":[` + change("title", "a", "b") + `],"evidence":"#` +
			strings.Repeat("e", maxEvidence-1) + `"}`,
	} {
		if _, _, err := q.correct(params, ptr("Author/Other")); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	// Every field of the list is taken; the values keep their characters.
	var all []string
	for _, f := range CorrectionFields {
		all = append(all, change(f, "", "Ünïcode "+f))
	}
	if p, err := CheckCorrect(json.RawMessage(body(all...))); err != nil || len(p.Changes) != len(CorrectionFields) ||
		p.Changes[0].Corrected != "Ünïcode title" {
		t.Fatalf("every field: %+v, %v", p, err)
	}
	// Allowed: a recording slug (trimmed), and evidence at the bound as filed
	// (an indented # line loses its indent and gains a backslash).
	ok := `{"work_id":"w","recording_id":" the-final-empire-kramer ","changes":[` + change("title", "a", "b") +
		`],"evidence":"  #` + strings.Repeat("e", maxEvidence-3) + `"}`
	if p, err := CheckCorrect(json.RawMessage(ok)); err != nil || p.RecordingID != "the-final-empire-kramer" {
		t.Fatalf("a recording slug and evidence at the bound: %+v, %v", p, err)
	}
}

// issueLength counts evidence as the community's issue carries it: a line
// starting with # (after its indent) loses the indent and gains a backslash.
func TestIssueLength(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]int{
		"plain":             5,
		"#1 in series":      13,
		"a\n  # heading\n#": 1 + 1 + 10 + 1 + 2,
		"\t#x":              3,
		"é#":                2,
	} {
		if got := issueLength(in); got != want {
			t.Errorf("issueLength(%q) = %d, want %d", in, got, want)
		}
	}
}

// SharedNothing: only a finished contribute job whose result names no
// contribution and what the community already had.
func TestSharedNothing(t *testing.T) {
	t.Parallel()
	covered := json.RawMessage(`{"work_id":"w","contributed":{},"covered":["characters","recaps"]}`)
	for name, c := range map[string]struct {
		job  Job
		want bool
	}{
		"all covered":      {Job{Kind: KindContribute, State: StateDone, Result: covered}, true},
		"null contributed": {Job{Kind: KindContribute, State: StateDone, Result: json.RawMessage(`{"contributed":null,"covered":["recaps"]}`)}, true},
		"one filed": {Job{Kind: KindContribute, State: StateDone,
			Result: json.RawMessage(`{"contributed":{"recaps":"https://github.com/o/r/issues/1"},"covered":["characters"]}`)}, false},
		"no result":       {Job{Kind: KindContribute, State: StateDone}, false},
		"nothing covered": {Job{Kind: KindContribute, State: StateDone, Result: json.RawMessage(`{"contributed":{}}`)}, false},
		"not finished":    {Job{Kind: KindContribute, State: StateFailed, Result: covered}, false},
		"another kind":    {Job{Kind: KindSidecars, State: StateDone, Result: covered}, false},
		"result not JSON": {Job{Kind: KindContribute, State: StateDone, Result: json.RawMessage(`[`)}, false},
	} {
		if got := c.job.SharedNothing(); got != c.want {
			t.Errorf("%s: SharedNothing = %t, want %t", name, got, c.want)
		}
	}
}

// A queued job that needs a managed module that is off is held: claims pass
// over it (other jobs go first) until the module is on again, and with
// ModuleOn unset nothing is held.
func TestClaimHoldsJobsOfAnOffModule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := newTestQueue(t)
	q.beatSetup(t, true, true, KindVerify, KindSidecars, KindContribute, KindCorrect)
	var held []int64
	for i, queue := range []func() (*Job, bool, error){
		func() (*Job, bool, error) { return q.sidecars(`{"contribute":"issue"}`, ptr("Author/One")) },
		func() (*Job, bool, error) { return q.contribute("", ptr("Author/Two")) },
		func() (*Job, bool, error) { return q.correct(goodCorrection, ptr("Author/Three")) },
	} {
		j, created, err := queue()
		if err != nil || !created {
			t.Fatalf("community job %d: %v, %v", i, created, err)
		}
		held = append(held, j.ID)
	}
	local, _, err := q.sidecars("", ptr("Author/Four"))
	if err != nil {
		t.Fatal(err)
	}
	on := false
	q.ModuleOn = func(id string) bool { return id != ModuleContributions || on }
	all := []string{KindSidecars, KindContribute, KindCorrect}
	if j, err := q.claim(ctx, q.token, all); err != nil || j == nil || j.ID != local.ID {
		t.Fatalf("claim with contributions off = %+v, %v; want the local generate job", j, err)
	}
	if j, err := q.claim(ctx, q.token, all); err != nil || j != nil {
		t.Fatalf("a held job was claimed: %+v, %v", j, err)
	}
	on = true
	var got []int64
	for range held {
		j, err := q.claim(ctx, q.token, all)
		if err != nil || j == nil {
			t.Fatalf("claim with contributions on = %+v, %v", j, err)
		}
		got = append(got, j.ID)
	}
	slices.Sort(got)
	if !slices.Equal(got, held) {
		t.Fatalf("claimed %v, want the held jobs %v", got, held)
	}
	// Unset, nothing is held.
	q.ModuleOn = nil
	if _, created, err := q.contribute("", ptr("Author/Five")); err != nil || !created {
		t.Fatalf("contribute: %v, %v", created, err)
	}
	if j, err := q.claim(ctx, q.token, all); err != nil || j == nil {
		t.Fatalf("claim with ModuleOn unset = %+v, %v", j, err)
	}
}

// ModuleFor: the community kinds need contributions, and so does a generate
// job only when it contributes.
func TestModuleFor(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		kind, params, want string
	}{
		{KindContribute, ``, ModuleContributions},
		{KindCorrect, `{}`, ModuleContributions},
		{KindSidecars, `{"contribute":"issue"}`, ModuleContributions},
		{KindSidecars, `{"contribute":"local"}`, ""},
		{KindSidecars, ``, ""},
		{KindSidecars, `not json`, ""},
		{KindDescribe, ``, ""},
		{KindVerify, ``, ""},
		{"bogus.kind", ``, ""},
	} {
		if got := ModuleFor(c.kind, json.RawMessage(c.params)); got != c.want {
			t.Errorf("ModuleFor(%s, %s) = %q, want %q", c.kind, c.params, got, c.want)
		}
	}
}
