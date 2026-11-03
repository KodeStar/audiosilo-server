package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
)

// The policy evaluator: what a poll backs up by itself.
func TestAudibleAutoImportWants(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	newer, older := since.Add(time.Second), since.Add(-time.Hour)
	base := autoImport{Policy: "all", PolicySince: since, FirstSeen: newer, State: AudibleAvailable}
	cases := []struct {
		name string
		edit func(*autoImport)
		want bool
	}{
		{"all, a new purchase", func(*autoImport) {}, true},
		{"all, the library it had", func(a *autoImport) { a.FirstSeen = older }, false},
		{"all, first seen at policy_since itself", func(a *autoImport) { a.FirstSeen = since }, false},
		{"series, a new purchase in a series the person has", func(a *autoImport) { a.Policy, a.SeriesOwned = "series", true }, true},
		{"series, a new purchase in no series they have", func(a *autoImport) { a.Policy = "series" }, false},
		{"series, an old title in a series they have", func(a *autoImport) { a.Policy, a.SeriesOwned, a.FirstSeen = "series", true, older }, false},
		{"manual, a new purchase", func(a *autoImport) { a.Policy = "manual" }, false},
		{"manual, a pending request", func(a *autoImport) { a.Policy, a.Pending = "manual", true }, true},
		{"a pending request for an old title", func(a *autoImport) { a.Pending, a.FirstSeen = true, older }, true},
		{"a pending request, still a preorder", func(a *autoImport) { a.Pending, a.State = true, AudiblePreorder }, false},
		{"a preorder under all", func(a *autoImport) { a.State = AudiblePreorder }, false},
		{"unavailable", func(a *autoImport) { a.State = AudibleUnavailable }, false},
		{"gone", func(a *autoImport) { a.State = AudibleGone }, false},
		{"already on this server", func(a *autoImport) { a.Matched = true }, false},
		{"a pending request, now on this server", func(a *autoImport) { a.Pending, a.Matched = true, true }, false},
		{"Not this book", func(a *autoImport) { a.Rejected = true }, false},
		{"ignored", func(a *autoImport) { a.Ignored = true }, false},
		{"ignored with a request", func(a *autoImport) { a.Ignored, a.Pending = true, true }, false},
		{"tried before", func(a *autoImport) { a.Attempted = true }, false},
		{"a backup open", func(a *autoImport) { a.Open = true }, false},
		{"a request with a backup open", func(a *autoImport) { a.Open, a.Pending = true, true }, false},
	}
	for _, tc := range cases {
		a := base
		tc.edit(&a)
		if got := a.wants(); got != tc.want {
			t.Errorf("%s: wants = %t, want %t", tc.name, got, tc.want)
		}
	}
}

// audibleFixture is a catalog with a books library, a person and an account on
// a controllable clock.
type audibleFixture struct {
	c     *Catalog
	ctx   context.Context
	lib   *Library
	user  int64
	clock time.Time
}

func newAudibleFixture(t *testing.T, policy string) *audibleFixture {
	t.Helper()
	c, ctx := newTestCatalog(t)
	f := &audibleFixture{c: c, ctx: ctx, clock: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)}
	c.now = func() time.Time { return f.clock }
	lib, err := c.CreateLibrary(ctx, Library{Name: "Books", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	f.lib, f.user = lib, seedUser(t, c, ctx)
	if _, err := c.PutAudibleAccount(ctx, "acct0001", AudibleAccountInput{Label: "x", Marketplace: "us",
		UserID: f.user, LibraryID: lib.ID, Policy: policy, Sync: true}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *audibleFixture) tick(d time.Duration) { f.clock = f.clock.Add(d) }

// poll stores one complete poll of titles.
func (f *audibleFixture) poll(t *testing.T, id string, titles ...AudibleTitleInput) {
	t.Helper()
	f.tick(time.Second)
	if err := f.c.PutAudibleTitles(f.ctx, "acct0001", id, true, titles); err != nil {
		t.Fatal(err)
	}
}

func avail(asin, series string) AudibleTitleInput {
	t := AudibleTitleInput{ASIN: asin, Title: "Title " + asin, State: AudibleAvailable}
	if series != "" {
		t.Series = []AudibleSeries{{Name: series, Position: "2"}}
	}
	return t
}

func asins(ps []bridge.AudibleImportParams) []string {
	out := []string{}
	for _, p := range ps {
		out = append(out, p.ASIN)
	}
	return out
}

func (f *audibleFixture) policyImports(t *testing.T) []string {
	t.Helper()
	ps, err := f.c.AudiblePolicyImports(f.ctx, "acct0001")
	if err != nil {
		t.Fatal(err)
	}
	return asins(ps)
}

// policy_since: the account's first complete poll is the library it already
// had (never backed up by a policy); a later poll's new title is a new
// purchase. Turning a policy on starts it again from now.
func TestAudiblePolicySince(t *testing.T) {
	t.Parallel()
	f := newAudibleFixture(t, "all")
	acct, _ := f.c.AudibleAccount(f.ctx, "acct0001")
	if acct.PolicySince != f.clock.Format(time.RFC3339Nano) {
		t.Fatalf("policy_since at create = %q", acct.PolicySince)
	}
	f.tick(time.Hour)
	f.poll(t, "p1", avail("B000000001", ""), avail("B000000002", ""))
	if got := f.policyImports(t); len(got) != 0 {
		t.Fatalf("first poll queues %v: the library the account had is never a policy's", got)
	}
	f.tick(time.Hour)
	f.poll(t, "p2", avail("B000000001", ""), avail("B000000002", ""), avail("B000000003", ""))
	if got := f.policyImports(t); !slices.Equal(got, []string{"B000000003"}) {
		t.Fatalf("second poll queues %v, want the new purchase", got)
	}
	// manual: none. Back to all: from now, so the purchase seen before isn't.
	if _, err := f.c.PatchAudibleAccount(f.ctx, "acct0001", AudibleAccountPatch{Policy: new("manual")}); err != nil {
		t.Fatal(err)
	}
	if got := f.policyImports(t); len(got) != 0 {
		t.Fatalf("manual queues %v", got)
	}
	f.tick(time.Minute)
	acct, err := f.c.PatchAudibleAccount(f.ctx, "acct0001", AudibleAccountPatch{Policy: new("all"), Template: new("{Author}/{Title}")})
	if err != nil || acct.PolicySince != f.clock.Format(time.RFC3339Nano) || acct.Template != "{Author}/{Title}" {
		t.Fatalf("all again = %+v, %v", acct, err)
	}
	if got := f.policyImports(t); len(got) != 0 {
		t.Fatalf("all, turned on again, queues %v from before it", got)
	}
	f.tick(time.Hour)
	f.poll(t, "p3", avail("B000000001", ""), avail("B000000002", ""), avail("B000000003", ""), avail("B000000004", ""))
	ps, _ := f.c.AudiblePolicyImports(f.ctx, "acct0001")
	if len(ps) != 1 || ps[0].ASIN != "B000000004" || ps[0].Template != "{Author}/{Title}" || ps[0].LibraryID != f.lib.ID {
		t.Fatalf("p3 queues %+v, want B000000004 with the template", ps)
	}
	// Once queued it is not asked for again.
	if err := f.c.SetAudibleImport(f.ctx, "acct0001", "B000000004", 1); err != nil {
		t.Fatal(err)
	}
	if got := f.policyImports(t); len(got) != 0 {
		t.Fatalf("after queueing: %v", got)
	}
	// Bad patches.
	if _, err := f.c.PatchAudibleAccount(f.ctx, "acct0001", AudibleAccountPatch{Policy: new("everything")}); !errors.Is(err, ErrAudiblePolicy) {
		t.Fatalf("bad policy: %v", err)
	}
	if _, err := f.c.PatchAudibleAccount(f.ctx, "acct0001", AudibleAccountPatch{Template: new("{Nope}")}); !errors.Is(err, bridge.ErrAudibleTemplate) {
		t.Fatalf("bad template: %v", err)
	}
	if _, err := f.c.PatchAudibleAccount(f.ctx, "acct0009", AudibleAccountPatch{}); !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("unknown account: %v", err)
	}
	// The Bridge mapping the account again leaves the console's policy alone.
	if _, err := f.c.PatchAudibleAccount(f.ctx, "acct0001", AudibleAccountPatch{Policy: new("series")}); err != nil {
		t.Fatal(err)
	}
	acct, err = f.c.PutAudibleAccount(f.ctx, "acct0001", AudibleAccountInput{Label: "y", Marketplace: "uk",
		UserID: f.user, LibraryID: f.lib.ID, Policy: "manual", Sync: true})
	if err != nil || acct.Policy != "series" || acct.Template != "{Author}/{Title}" || acct.Label != "y" {
		t.Fatalf("re-mapped = %+v, %v (policy and template are the console's)", acct, err)
	}
}

// series: a new purchase is backed up only when the person has a book of one
// of its series, in any spelling, in a library they can see.
func TestAudiblePolicySeries(t *testing.T) {
	t.Parallel()
	f := newAudibleFixture(t, "series")
	if err := f.c.GrantWholeLibrary(f.ctx, f.user, f.lib.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.UpsertBook(f.ctx, &Book{LibraryID: f.lib.ID, RelPath: "Sanderson/Mistborn/1", Title: "Mistborn",
		Series: "Mistborn", SeriesIndex: 1}); err != nil {
		t.Fatal(err)
	}
	f.poll(t, "p1", avail("B000000001", "Mistborn"))
	f.tick(time.Hour)
	f.poll(t, "p2", avail("B000000001", "Mistborn"), avail("B000000002", "MISTBORN"),
		avail("B000000003", "Stormlight"), avail("B000000004", ""))
	if got := f.policyImports(t); !slices.Equal(got, []string{"B000000002"}) {
		t.Fatalf("series queues %v, want the new title of a series they have", got)
	}
	// A person who can't see the library has no book of it.
	other := seedUserNamed(t, f.c, f.ctx, "kim")
	if _, err := f.c.PutAudibleAccount(f.ctx, "acct0001", AudibleAccountInput{Label: "x", Marketplace: "us",
		UserID: other, LibraryID: f.lib.ID, Policy: "manual", Sync: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.policyImports(t); len(got) != 0 {
		t.Fatalf("series for a person without access queues %v", got)
	}
}

// A console Back up on a preorder waits (survives in the row) and is queued by
// the poll that finds it available, whatever the policy; "Not this book"
// drops it.
func TestAudiblePendingBackup(t *testing.T) {
	t.Parallel()
	f := newAudibleFixture(t, "manual")
	pre := AudibleTitleInput{ASIN: "B000000001", Title: "Soon", State: AudiblePreorder, ReleaseDate: "2026-11-10"}
	f.poll(t, "p1", pre, AudibleTitleInput{ASIN: "B000000002", Title: "Later", State: AudiblePreorder, ReleaseDate: "2026-12-01"})
	p, pending, err := f.c.RequestAudibleBackup(f.ctx, "acct0001", "B000000001")
	if err != nil || !pending || p != nil {
		t.Fatalf("back up a preorder = %+v, %t, %v", p, pending, err)
	}
	if _, _, err := f.c.RequestAudibleBackup(f.ctx, "acct0001", "B000000001"); !errors.Is(err, ErrAudibleImportOpen) {
		t.Fatalf("again: %v, want import_open", err)
	}
	if tt, _ := f.c.AudibleTitle(f.ctx, "acct0001", "B000000001"); tt.Import == nil || tt.Import.State != AudibleImportPending || tt.Import.JobID != nil {
		t.Fatalf("pending title = %+v", tt.Import)
	}
	if _, pending, _ := f.c.RequestAudibleBackup(f.ctx, "acct0001", "B000000002"); !pending {
		t.Fatal("second preorder not pending")
	}
	if _, err := f.c.PatchAudibleTitle(f.ctx, "acct0001", "B000000002", AudibleTitlePatch{Rejected: new(true)}); err != nil {
		t.Fatal(err)
	}
	if got := f.policyImports(t); len(got) != 0 {
		t.Fatalf("still preorders: %v", got)
	}
	pre.State = AudibleAvailable
	f.poll(t, "p2", pre, AudibleTitleInput{ASIN: "B000000002", Title: "Later", State: AudibleAvailable})
	if got := f.policyImports(t); !slices.Equal(got, []string{"B000000001"}) {
		t.Fatalf("available now: %v, want the pending one only", got)
	}
	if err := f.c.SetAudibleImport(f.ctx, "acct0001", "B000000001", 7); err != nil {
		t.Fatal(err)
	}
	if tt, _ := f.c.AudibleTitle(f.ctx, "acct0001", "B000000001"); tt.Import != nil || *tt.ImportJobID != 7 {
		t.Fatalf("queued title = %+v (no such job row: no import shown)", tt)
	}
	if got := f.policyImports(t); len(got) != 0 {
		t.Fatalf("once queued: %v", got)
	}
}

// Preorder clock: a preorder released by the UTC date and not yet available
// makes its account due hourly instead of every poll_hours, for 14 days; then
// it needs attention and the account goes back to its interval.
func TestAudiblePreorderClock(t *testing.T) {
	t.Parallel()
	f := newAudibleFixture(t, "manual")
	f.clock = time.Date(2026, 11, 9, 22, 0, 0, 0, time.UTC)
	f.poll(t, "p1", AudibleTitleInput{ASIN: "B000000001", Title: "Soon", State: AudiblePreorder, ReleaseDate: "2026-11-10"})
	if err := f.c.MarkAudibleQueued(f.ctx, "acct0001"); err != nil {
		t.Fatal(err)
	}
	queued := f.clock
	due := func(at time.Time) bool {
		t.Helper()
		ids, err := f.c.DueAudiblePolls(f.ctx, at, 10)
		if err != nil {
			t.Fatal(err)
		}
		return len(ids) == 1
	}
	// Before the release date (UTC): every 6 hours only.
	if due(queued.Add(90 * time.Minute)) {
		t.Fatal("due hourly before its release date")
	}
	// The release day (2026-11-10 00:30 UTC): hourly.
	if !due(queued.Add(150 * time.Minute)) {
		t.Fatal("not due an hour later on its release date")
	}
	if due(queued.Add(30 * time.Minute)) {
		t.Fatal("due within the hour")
	}
	// Day 13 after release: still hourly; not yet needing attention.
	day13 := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	f.clock = day13
	if err := f.c.MarkAudibleQueued(f.ctx, "acct0001"); err != nil {
		t.Fatal(err)
	}
	if !due(day13.Add(61 * time.Minute)) {
		t.Fatal("not hourly on day 13")
	}
	if tt, _ := f.c.AudibleTitle(f.ctx, "acct0001", "B000000001"); tt.NeedsAttention {
		t.Fatal("needs attention on day 13")
	}
	// Day 14: needs attention, back to poll_hours.
	day14 := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	f.clock = day14
	if err := f.c.MarkAudibleQueued(f.ctx, "acct0001"); err != nil {
		t.Fatal(err)
	}
	if due(day14.Add(61*time.Minute)) || !due(day14.Add(6*time.Hour)) {
		t.Fatal("still hourly after 14 days")
	}
	if tt, _ := f.c.AudibleTitle(f.ctx, "acct0001", "B000000001"); !tt.NeedsAttention {
		t.Fatal("no attention after 14 days")
	}
	// Available at last: no attention, no hourly polls.
	f.poll(t, "p2", AudibleTitleInput{ASIN: "B000000001", Title: "Soon", State: AudibleAvailable, ReleaseDate: "2026-11-10"})
	if tt, _ := f.c.AudibleTitle(f.ctx, "acct0001", "B000000001"); tt.NeedsAttention {
		t.Fatal("an available title needs attention")
	}
	// Marked "Not this book", a released preorder doesn't speed polls up.
	f.poll(t, "p3", AudibleTitleInput{ASIN: "B000000002", Title: "Other", State: AudiblePreorder, ReleaseDate: "2026-11-23"})
	if !due(f.clock.Add(61 * time.Minute)) {
		t.Fatal("a released preorder doesn't make the account hourly")
	}
	if _, err := f.c.PatchAudibleTitle(f.ctx, "acct0001", "B000000002", AudibleTitlePatch{Rejected: new(true)}); err != nil {
		t.Fatal(err)
	}
	if due(f.clock.Add(61 * time.Minute)) {
		t.Fatal("a rejected preorder makes the account hourly")
	}
}

// A done backup becomes its title's match (how import, Not this book cleared);
// a result naming another library or no indexed book changes nothing; the next
// poll keeps the match while the book is indexed and the poll names no other.
func TestFoldAudibleImport(t *testing.T) {
	t.Parallel()
	f := newAudibleFixture(t, "manual")
	f.poll(t, "p1", avail("B000000001", ""))
	if _, err := f.c.PatchAudibleTitle(f.ctx, "acct0001", "B000000001", AudibleTitlePatch{Rejected: new(true)}); err != nil {
		t.Fatal(err)
	}
	if err := f.c.SetAudibleImport(f.ctx, "acct0001", "B000000001", 5); err != nil {
		t.Fatal(err)
	}
	lib := f.lib.ID
	params, _ := json.Marshal(bridge.AudibleImportParams{Account: "acct0001", ASIN: "B000000001", LibraryID: lib})
	job := func(id int64, libID int64, path string) bridge.Job {
		return bridge.Job{ID: id, Kind: bridge.KindAudibleImport, State: bridge.StateDone, LibraryID: &lib, Params: params,
			Result: json.RawMessage(fmt.Sprintf(`{"library_id":%d,"path":%q,"files":[],"bytes":1,"enriched":true}`, libID, path))}
	}
	if err := f.c.FoldAudibleImport(f.ctx, job(5, lib, "Author/Book")); !errors.Is(err, ErrImportNotIndexed) {
		t.Fatalf("no book there: %v", err)
	}
	if _, err := f.c.UpsertBook(f.ctx, &Book{LibraryID: lib, RelPath: "Author/Book", Title: "Book"}); err != nil {
		t.Fatal(err)
	}
	if err := f.c.FoldAudibleImport(f.ctx, job(5, lib+1, "Author/Book")); !errors.Is(err, ErrImportNotIndexed) {
		t.Fatalf("another library: %v", err)
	}
	// Another backup's job leaves the title alone.
	if err := f.c.FoldAudibleImport(f.ctx, job(6, lib, "Author/Book")); err != nil {
		t.Fatal(err)
	}
	if tt, _ := f.c.AudibleTitle(f.ctx, "acct0001", "B000000001"); tt.Match != nil {
		t.Fatalf("another job matched it: %+v", tt.Match)
	}
	if err := f.c.FoldAudibleImport(f.ctx, job(5, lib, "Author/Book")); err != nil {
		t.Fatal(err)
	}
	tt, _ := f.c.AudibleTitle(f.ctx, "acct0001", "B000000001")
	if tt.Match == nil || tt.Match.How != AudibleMatchImport || tt.Match.Path != "Author/Book" || tt.Rejected {
		t.Fatalf("after the fold = %+v", tt)
	}
	// A poll that names no match keeps it; one naming the same book keeps "import".
	f.poll(t, "p2", avail("B000000001", ""))
	if tt, _ := f.c.AudibleTitle(f.ctx, "acct0001", "B000000001"); tt.Match == nil || tt.Match.How != AudibleMatchImport {
		t.Fatalf("a poll without a match dropped it: %+v", tt.Match)
	}
	same := avail("B000000001", "")
	same.Match = &AudibleMatch{LibraryID: lib, Path: "Author/Book", How: "asin"}
	f.poll(t, "p3", same)
	if tt, _ := f.c.AudibleTitle(f.ctx, "acct0001", "B000000001"); tt.Match == nil || tt.Match.How != AudibleMatchImport {
		t.Fatalf("the same match by ASIN: %+v", tt.Match)
	}
	// Another book named: the poll wins.
	if _, err := f.c.UpsertBook(f.ctx, &Book{LibraryID: lib, RelPath: "Author/Other", Title: "Other"}); err != nil {
		t.Fatal(err)
	}
	same.Match = &AudibleMatch{LibraryID: lib, Path: "Author/Other", How: "fuzzy"}
	f.poll(t, "p4", same)
	if tt, _ := f.c.AudibleTitle(f.ctx, "acct0001", "B000000001"); tt.Match == nil || tt.Match.Path != "Author/Other" {
		t.Fatalf("the poll's other match: %+v", tt.Match)
	}
}

// Back up's refusals, and Back up everything's set and estimate.
func TestAudibleBackupRequests(t *testing.T) {
	t.Parallel()
	f := newAudibleFixture(t, "manual")
	matched := avail("B000000002", "")
	matched.RuntimeS = 3600
	if _, err := f.c.UpsertBook(f.ctx, &Book{LibraryID: f.lib.ID, RelPath: "A/B", Title: "B"}); err != nil {
		t.Fatal(err)
	}
	matched.Match = &AudibleMatch{LibraryID: f.lib.ID, Path: "A/B", How: "fuzzy"}
	one := avail("B000000001", "")
	one.RuntimeS = 36000
	f.poll(t, "p1", one, matched, avail("B000000003", ""),
		AudibleTitleInput{ASIN: "B000000004", Title: "Gone soon", State: AudibleUnavailable})
	if _, err := f.c.PatchAudibleTitle(f.ctx, "acct0001", "B000000003", AudibleTitlePatch{Rejected: new(true),
		DestPath: new("Author/Series/Book.m4b")}); err != nil {
		t.Fatal(err)
	}
	for asin, want := range map[string]error{
		"B000000002": ErrAudibleMatched, "B000000004": ErrAudibleNotAvailable, "B000000009": ErrUnknownTitle,
	} {
		if _, _, err := f.c.RequestAudibleBackup(f.ctx, "acct0001", asin); !errors.Is(err, want) {
			t.Errorf("back up %s: %v, want %v", asin, err, want)
		}
	}
	// "Not this book" on a match: the title may be backed up one by one, with its destination.
	p, pending, err := f.c.RequestAudibleBackup(f.ctx, "acct0001", "B000000003")
	if err != nil || pending || p.DestPath != "Author/Series/Book.m4b" {
		t.Fatalf("back up a rejected title = %+v, %v", p, err)
	}
	all, err := f.c.AudibleBackupAll(f.ctx, "acct0001")
	if err != nil || !slices.Equal(asins(all), []string{"B000000001"}) {
		t.Fatalf("everything = %v, %v", asins(all), err)
	}
	accts, err := f.c.AudibleAccounts(f.ctx)
	if err != nil || accts[0].BackupEstimate == nil || *accts[0].BackupEstimate != (AudibleEstimate{Count: 1, Bytes: 36000 * audibleBytesPerSecond}) {
		t.Fatalf("estimate = %+v, %v", accts[0].BackupEstimate, err)
	}
	for _, bad := range []string{"../x.m4b", "/abs.m4b", "a//b.m4b", "a/./b.m4b", ".hidden/b.m4b", "a/.audiosilo/x", `a\b.m4b`, "a/b\x00.m4b", "a/"} {
		if _, err := f.c.PatchAudibleTitle(f.ctx, "acct0001", "B000000001", AudibleTitlePatch{DestPath: new(bad)}); !errors.Is(err, bridge.ErrAudibleDestPath) {
			t.Errorf("dest %q: %v", bad, err)
		}
	}
	if _, err := f.c.PatchAudibleTitle(f.ctx, "acct0001", "B000000009", AudibleTitlePatch{}); !errors.Is(err, ErrUnknownTitle) {
		t.Fatalf("patch unknown: %v", err)
	}
	// Paused: nothing.
	if err := f.c.DeleteLibrary(f.ctx, f.lib.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.AudibleBackupAll(f.ctx, "acct0001"); !errors.Is(err, ErrAudiblePaused) {
		t.Fatalf("paused everything: %v", err)
	}
	if _, _, err := f.c.RequestAudibleBackup(f.ctx, "acct0001", "B000000001"); !errors.Is(err, ErrAudiblePaused) {
		t.Fatalf("paused backup: %v", err)
	}
	if got := f.policyImports(t); len(got) != 0 {
		t.Fatalf("paused policy: %v", got)
	}
}
