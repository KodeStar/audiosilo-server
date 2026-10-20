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
	"unicode/utf16"

	"github.com/kodestar/audiosilo-meta/pkg/model"
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

// While the contributions module is off, a queued contribute job is held (no
// claim hands it out), so it gives way to a generate job like a parked one:
// it is no blocker, and the generate job is queued and cancels it. A claimed
// one still blocks (the Bridge may be filing), and with the module on a queued
// one blocks as before.
func TestHeldContributeGivesWayToGenerate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := newTestQueue(t)
	q.beatSetup(t, true, true, KindVerify, KindSidecars, KindContribute)
	on := true
	q.ModuleOn = func(id string) bool { return id != ModuleContributions || on }
	book, other := ptr("Author/Book"), ptr("Author/Other")

	con, _, err := q.contribute("", book)
	if err != nil {
		t.Fatal(err)
	}
	// Denied: the module on, a queued contribute refuses a generate job.
	if _, _, err := q.sidecars("", book); !errors.Is(err, ErrContributeOpen) {
		t.Fatalf("generate with a queued contribute, module on: %v, want ErrContributeOpen", err)
	}
	if b, err := q.Blocker(ctx, KindSidecars, q.lib, *book); err != nil || b == nil || b.ID != con.ID {
		t.Fatalf("blocker, module on = %+v, %v; want the contribute job", b, err)
	}

	// Allowed: the module off, the held contribute is no blocker, and a
	// generate job is queued and cancels it.
	on = false
	if b, err := q.Blocker(ctx, KindSidecars, q.lib, *book); err != nil || b != nil {
		t.Fatalf("blocker, module off = %+v, %v; want none", b, err)
	}
	if _, created, err := q.sidecars("", book); err != nil || !created {
		t.Fatalf("generate with a held contribute: %v, %v", created, err)
	}
	if got, err := q.Job(ctx, con.ID); err != nil || got.State != StateCancelled || got.Error == "" {
		t.Fatalf("the held contribute = %+v, %v; want cancelled with a reason", got, err)
	}

	// Denied: a contribute job the Bridge claimed blocks even with the module off.
	on = true
	claimed, _, err := q.contribute("", other)
	if err != nil {
		t.Fatal(err)
	}
	if j, err := q.claim(ctx, q.token, []string{KindContribute}); err != nil || j == nil || j.ID != claimed.ID {
		t.Fatalf("claim = %+v, %v", j, err)
	}
	on = false
	if _, _, err := q.sidecars("", other); !errors.Is(err, ErrContributeOpen) {
		t.Fatalf("generate with a claimed contribute, module off: %v, want ErrContributeOpen", err)
	}
	if b, err := q.Blocker(ctx, KindSidecars, q.lib, *other); err != nil || b == nil || b.ID != claimed.ID {
		t.Fatalf("blocker with a claimed contribute = %+v, %v; want it", b, err)
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

// asciiJSON is s as a JSON string an ASCII-only encoder sends: every character
// outside ASCII a \u escape, two (a surrogate pair) outside the BMP.
func asciiJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if r < 0x80 {
			b.WriteRune(r)
			continue
		}
		for _, u := range utf16.Encode([]rune{r}) {
			fmt.Fprintf(&b, "\\u%04x", u)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// fill is s repeated to exactly n characters.
func fill(s string, n int) string {
	return string([]rune(strings.Repeat(s, n))[:n])
}

// maxCorrection is a meta.correct batch at every bound: each field once, both
// values and the evidence at their most characters (s repeated), encoded by
// enc.
func maxCorrection(enc func(string) string, current, corrected, evidence string) string {
	var changes []string
	for _, f := range CorrectionFields {
		changes = append(changes, fmt.Sprintf(`{"field":%q,"current":%s,"corrected":%s}`, f,
			enc(fill(current, maxCorrectionValue)), enc(fill(corrected, maxCorrectionValue))))
	}
	return `{"work_id":"w","changes":[` + strings.Join(changes, ",") + `],"evidence":` +
		enc(fill(evidence, maxEvidence)) + `}`
}

// MaxParamsBytes, which bounds the queue route's body, is the largest cap of
// any kind: a kind given a larger one would be refused before Enqueue.
func TestMaxParamsBytesIsTheLargestCap(t *testing.T) {
	t.Parallel()
	most := 0
	for _, s := range kinds {
		most = max(most, s.paramsCap())
	}
	if most != MaxParamsBytes {
		t.Fatalf("MaxParamsBytes is %d, the largest kind's cap %d", MaxParamsBytes, most)
	}
}

// meta.correct's params cap is its own: a batch at every per-field bound is
// taken however it is encoded (multi-byte characters, each escaped as an
// ASCII-only encoder does, and characters Go escapes when it stores them),
// one past a per-field bound is refused for that, and params past the cap are
// refused as too large. Other kinds keep the default cap.
func TestCorrectParamsCap(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	q.beatSetup(t, true, true, KindVerify, KindSidecars, KindCorrect)
	utf8JSON := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	astral := maxCorrection(asciiJSON, "📚", "🎧", "📖")

	for name, params := range map[string]string{
		"multi-byte":             maxCorrection(utf8JSON, "📚", "ب", "é"),
		"multi-byte, ASCII-only": astral,
		"escaped when stored":    maxCorrection(utf8JSON, "<", "&", ">"),
		"Persian and joiners":    maxCorrection(asciiJSON, "می\u200cخ", "👩\u200d👩", "ف"),
	} {
		if len(params) <= defaultParamsBytes {
			t.Fatalf("%s: %d bytes, under the default cap; the test proves nothing", name, len(params))
		}
		job, created, err := q.correct(params, ptr("Author/"+name))
		if err != nil || !created {
			t.Fatalf("allowed (%s, %d bytes): %v, %v", name, len(params), created, err)
		}
		var p CorrectParams
		if err := json.Unmarshal(job.Params, &p); err != nil || len(p.Changes) != len(CorrectionFields) {
			t.Fatalf("%s: stored %d changes, %v", name, len(p.Changes), err)
		}
	}

	// Denied: one past a per-field bound, refused for it rather than for size.
	for name, params := range map[string]string{
		"a value too long":  strings.Replace(astral, `"corrected":"`, `"corrected":"\ud83c\udfa7`, 1),
		"evidence too long": strings.Replace(astral, `"evidence":"`, `"evidence":"\ud83d\udcd6`, 1),
	} {
		_, _, err := q.correct(params, ptr("Author/Over"))
		if !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), "too large") {
			t.Errorf("%s: %v, want ErrInvalid for the field", name, err)
		}
	}
	tooLarge := `{"work_id":"w"` + strings.Repeat(" ", maxCorrectParamsBytes) + `}`
	if _, _, err := q.correct(tooLarge, ptr("Author/Over")); err == nil || !strings.Contains(err.Error(), "params too large") {
		t.Errorf("meta.correct past its cap: %v, want params too large", err)
	}
	if _, err := CheckCorrect(json.RawMessage(tooLarge)); err == nil || !strings.Contains(err.Error(), "params too large") {
		t.Errorf("CheckCorrect past the cap: %v, want params too large", err)
	}
	// Other kinds keep the default.
	padded := `{"source":"" ` + strings.Repeat(" ", defaultParamsBytes) + `}`
	if _, _, err := q.sidecars(padded, ptr("Author/Book")); err == nil || !strings.Contains(err.Error(), "params too large") {
		t.Errorf("sidecars.generate past the default cap: %v, want params too large", err)
	}
}

// The community refuses a batch holding a bidirectional embedding, override
// or isolate control (U+202A-U+202E, U+2066-U+2069), so a value or the
// evidence holding one is refused at queue time; the joiners, the marks and
// right-to-left text are taken.
func TestCorrectRefusesBidiControls(t *testing.T) {
	t.Parallel()
	params := func(current, corrected, evidence string) string {
		return fmt.Sprintf(`{"work_id":"w","changes":[{"field":"title","current":%q,"corrected":%q}],"evidence":%q}`,
			current, corrected, evidence)
	}
	for name, p := range map[string]string{
		"ZWJ":     params("a", "👩\u200d👩", "e"),
		"ZWNJ":    params("a", "می\u200cخواهم", "e"),
		"Persian": params("شازده کوچولو", "شازده\u200cی کوچولو", "روی جلد کتاب نوشته شده است."),
		"marks":   params("a", "\u200fעברית\u200e", "e"),
	} {
		if _, err := CheckCorrect(json.RawMessage(p)); err != nil {
			t.Errorf("allowed (%s): %v", name, err)
		}
	}
	for _, r := range []rune{0x202A, 0x202B, 0x202C, 0x202D, 0x202E, 0x2066, 0x2067, 0x2068, 0x2069} {
		c := string(r)
		for where, p := range map[string]string{
			"current":   params("a"+c+"b", "c", "e"),
			"corrected": params("a", "b"+c+"c", "e"),
			"evidence":  params("a", "b", "see"+c+" here"),
		} {
			_, err := CheckCorrect(json.RawMessage(p))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "bidirectional") {
				t.Errorf("U+%04X in %s: %v, want ErrInvalid", r, where, err)
			}
		}
	}
}

// The community reads each value and the evidence as GitHub shows them: an
// HTML character reference ending in ';' is the character it names (a
// whitespace one a space), and evidence left as "_No response_" is none. So a
// reference to a control or a bidirectional override is refused as the
// character is, a corrected value that reads empty or as the current one (the
// record's, as it spells it) changes nothing, and evidence that reads as none
// is missing; any other reference, and an ampersand that starts none, is taken
// and kept as sent. A name html.UnescapeString leaves as written (&nLt; and
// &nGt;, which a browser decodes; the console's namedRef mirrors Go) is none.
func TestCorrectReadsValuesAsTheCommunity(t *testing.T) {
	t.Parallel()
	params := func(current, corrected, evidence string) string {
		return fmt.Sprintf(`{"work_id":"w","changes":[{"field":"title","current":%q,"corrected":%q}],"evidence":%q}`,
			current, corrected, evidence)
	}
	for name, c := range map[string]struct{ params, want string }{
		"an override by reference":     {params("a&#x202E;b", "c", "e"), "bidirectional"},
		"a decimal override":           {params("a", "b&#8238;c", "e"), "bidirectional"},
		"an isolate in the evidence":   {params("a", "b", "see &#x2066;here"), "bidirectional"},
		"an upper-case hex reference":  {params("a", "b", "see &#X202E;here"), "bidirectional"},
		"a control by reference":       {params("a", "A&#1;B", "e"), "control character"},
		"a control in the evidence":    {params("a", "b", "see\fhere"), "control character"},
		"a C1 space before a ref":      {params("a", "b\u0085&#32;", "e"), "control character"},
		"a C1 space in the current":    {params("a\u0085&#x20;", "b", "e"), "control character"},
		"a C1 space ending evidence":   {params("a", "b", "see\u0085&nbsp;"), "control character"},
		"a no-break space for a value": {params("a", "&#160;", "e"), "must be set"},
		"a named no-break space":       {params("a", "&nbsp;", "e"), "must be set"},
		"the current value escaped":    {params("Tom & Jerry", "Tom &amp; Jerry", "e"), "differ"},
		"evidence a no-break space":    {params("a", "b", "&nbsp;"), "evidence is required"},
		"evidence as the form's blank": {params("a", "b", " _No response_ "), "evidence is required"},
	} {
		_, err := CheckCorrect(json.RawMessage(c.params))
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want ErrInvalid (%s)", name, err, c.want)
		}
	}
	for name, c := range map[string]struct{ current, corrected, evidence string }{
		"a mark by reference":     {"a", "&lrm;עברית", "e"},
		"an ampersand":            {"a", "AT&T;", "e"},
		"a name it doesn't know":  {"a", "&copyright;", "e"},
		"accents by reference":    {"Resume", "R&#233;sum&#233;", "e"},
		"a record that spells it": {"Tom &amp; Jerry", "Tom & Jerry", "e"},
		"markup in the evidence":  {"a", "b", "The cover reads &lt;Mistborn&gt;."},
		"a space before a ref":    {"a", "b\u00a0&#32;", "e"},
		"evidence ending a ref":   {"a", "b", "see\u00a0&nbsp;"},
		"a name html keeps":       {"\u226a\u20d2", "&nLt;", "e"},
	} {
		p, err := CheckCorrect(json.RawMessage(params(c.current, c.corrected, c.evidence)))
		if err != nil || p.Changes[0].Corrected != c.corrected || p.Evidence != c.evidence {
			t.Errorf("allowed (%s): %+v, %v; want it kept as sent", name, p, err)
		}
	}
}

// bridgeParamsBytes is the Bridge's own bound on meta.correct's params as it
// claims them (audiosilo-bridge's internal/correct ParseParams).
const bridgeParamsBytes = 64 << 10

// meta.correct's params as stored, which the Bridge claims, fit the Bridge's
// bound with every bound here reached in the characters the store escapes
// most (json.Marshal sends <, > and & as six bytes each): a job the server
// took and the Bridge refused could never be filed.
func TestCorrectStoredParamsFitTheBridge(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	q.beatSetup(t, true, true, KindVerify, KindCorrect)
	var changes []string
	for _, f := range CorrectionFields {
		changes = append(changes, fmt.Sprintf(`{"field":%q,"current":%q,"corrected":%q}`, f,
			strings.Repeat("<", maxCorrectionValue), strings.Repeat(">", maxCorrectionValue)))
	}
	id := strings.Repeat("<", maxCorrectIDLen)
	params := fmt.Sprintf(`{"work_id":%q,"recording_id":%q,"asin":%q,"isbn":%q,"region":%q,"changes":[%s],"evidence":%q,"source":%q}`,
		strings.Repeat("<", maxWorkIDLen), strings.Repeat("r", model.MaxSlugLen), id, id, id,
		strings.Join(changes, ","), strings.Repeat("&", maxEvidence), strings.Repeat("<", maxSourceLen))
	job, created, err := q.correct(params, ptr("Author/Book"))
	if err != nil || !created {
		t.Fatalf("a batch at every bound: %v, %v", created, err)
	}
	if len(job.Params) > bridgeParamsBytes {
		t.Fatalf("stored %d bytes, past the Bridge's %d", len(job.Params), bridgeParamsBytes)
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
