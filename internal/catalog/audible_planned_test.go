package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/bridge"
)

func planned(f *audibleFixture, path string) *AudiblePlan {
	return &AudiblePlan{LibraryID: f.lib.ID, Path: path, File: path[strings.LastIndex(path, "/")+1:],
		Kind: AudiblePlanSeriesExisting, Siblings: 7, State: AudiblePlanJoins}
}

func (f *audibleFixture) title(t *testing.T, asin string) *AudibleTitle {
	t.Helper()
	tt, err := f.c.AudibleTitle(f.ctx, "acct0001", asin)
	if err != nil {
		t.Fatal(err)
	}
	return tt
}

// A title's planned destination is the poll's: stored, replaced by the next
// poll, dropped by a poll that plans none, never stored beside a match (the
// poll's or a backup's), and shown only while the account is mapped to the
// plan's library (a contest likewise).
func TestAudiblePlanned(t *testing.T) {
	t.Parallel()
	f := newAudibleFixture(t, "manual")
	ti := avail("B000000001", "Rivers")
	ti.Planned = planned(f, "Ben Aaronovitch/Rivers of London/2 - Moon over Soho/Moon over Soho.m4b")
	f.poll(t, "p1", ti)
	if got := f.title(t, "B000000001").Planned; got == nil || *got != *ti.Planned {
		t.Fatalf("planned = %+v, want %+v", got, ti.Planned)
	}
	// Replaced by the next poll's plan.
	ti.Planned = &AudiblePlan{LibraryID: f.lib.ID, Path: "A/Moon.m4b", File: "Moon.m4b", Kind: AudiblePlanStandalone,
		State: AudiblePlanExists, Conflict: "file", Template: "{Author}/{Title}"}
	f.poll(t, "p2", ti)
	if got := f.title(t, "B000000001").Planned; got == nil || *got != *ti.Planned {
		t.Fatalf("replaced = %+v, want %+v", got, ti.Planned)
	}
	// A poll that plans none drops it.
	ti.Planned = nil
	f.poll(t, "p3", ti)
	if got := f.title(t, "B000000001").Planned; got != nil {
		t.Fatalf("a poll without a plan kept %+v", got)
	}
	// A plan beside a match is never stored.
	if _, err := f.c.UpsertBook(f.ctx, &Book{LibraryID: f.lib.ID, RelPath: "A/Moon", Title: "Moon"}); err != nil {
		t.Fatal(err)
	}
	ti.Planned = planned(f, "A/Moon/Moon.m4b")
	ti.Match = &AudibleMatch{LibraryID: f.lib.ID, Path: "A/Moon", How: "fuzzy"}
	f.poll(t, "p4", ti)
	var raw string
	if err := f.c.db.QueryRowContext(f.ctx, `SELECT planned FROM audible_titles WHERE asin = 'B000000001'`).Scan(&raw); err != nil || raw != "" {
		t.Fatalf("stored beside a match: %q, %v", raw, err)
	}

	// A backup's match clears what the poll said of it unmatched.
	other := avail("B000000002", "")
	other.Planned, other.ContestedPath, other.Reason = planned(f, "A/Other/Other.m4b"), "A/Moon", AudibleReasonContested
	f.poll(t, "p5", other)
	if tt := f.title(t, "B000000002"); tt.Planned == nil || tt.Contested == nil {
		t.Fatalf("before the backup = %+v", tt)
	}
	if err := f.c.SetAudibleImport(f.ctx, "acct0001", "B000000002", 9); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.UpsertBook(f.ctx, &Book{LibraryID: f.lib.ID, RelPath: "A/Other", IsFolder: true, Title: "Other",
		Files: []BookFile{{RelPath: "A/Other/Other.m4b"}}}); err != nil {
		t.Fatal(err)
	}
	lib := f.lib.ID
	params, _ := json.Marshal(bridge.AudibleImportParams{Account: "acct0001", ASIN: "B000000002", LibraryID: lib})
	if err := f.c.FoldAudibleImport(f.ctx, bridge.Job{ID: 9, Kind: bridge.KindAudibleImport, State: bridge.StateDone,
		LibraryID: &lib, Params: params, Result: json.RawMessage(fmt.Sprintf(
			`{"library_id":%d,"path":"A/Other","files":[{"path":"A/Other/Other.m4b","size":1}],"bytes":1}`, lib))}); err != nil {
		t.Fatal(err)
	}
	if err := f.c.db.QueryRowContext(f.ctx, `SELECT planned || contest_path FROM audible_titles WHERE asin = 'B000000002'`).Scan(&raw); err != nil || raw != "" {
		t.Fatalf("the fold kept %q, %v", raw, err)
	}

	// Shown only while the account is mapped to the plan's (and the
	// contest's) library: a contest is never read against another library.
	third := avail("B000000003", "")
	third.Planned = planned(f, "B/Third/Third.m4b")
	third.ContestedPath = "B/Third"
	f.poll(t, "p6", third)
	lib2, err := f.c.CreateLibrary(f.ctx, Library{Name: "Two", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.PutAudibleAccount(f.ctx, "acct0001", AudibleAccountInput{Label: "x", Marketplace: "us",
		UserID: f.user, LibraryID: lib2.ID, Policy: "manual", Sync: true}); err != nil {
		t.Fatal(err)
	}
	if tt := f.title(t, "B000000003"); tt.Planned != nil || tt.Contested != nil {
		t.Fatalf("mapped elsewhere = planned %+v, contested %+v", tt.Planned, tt.Contested)
	}
	if err := f.c.DeleteLibrary(f.ctx, lib2.ID); err != nil {
		t.Fatal(err)
	}
	if tt := f.title(t, "B000000003"); tt.Planned != nil || tt.Contested != nil {
		t.Fatalf("paused = planned %+v, contested %+v", tt.Planned, tt.Contested)
	}
}

// A planned destination and a contested book are checked strictly: a
// Bridge's mistake fails the call, never stores half a plan.
func TestAudiblePlannedChecked(t *testing.T) {
	t.Parallel()
	f := newAudibleFixture(t, "manual")
	good := func() *AudiblePlan { return planned(f, "A/S/1 - B/B.m4b") }
	for _, tc := range []struct {
		name    string
		edit    func(p *AudiblePlan)
		contest string
		ok      bool
	}{
		{"good", func(*AudiblePlan) {}, "", true},
		{"exists", func(p *AudiblePlan) { p.State, p.Conflict = AudiblePlanExists, "book" }, "", true},
		{"failed, no path", func(p *AudiblePlan) {
			p.State, p.Path, p.File, p.Kind, p.Conflict = AudiblePlanError, "", "", "", "unknown variable {Nope}"
		}, "", true},
		{"template at the bound", func(p *AudiblePlan) { p.Template = strings.Repeat("a", bridge.MaxAudibleTemplate) }, "", true},
		{"contested", func(*AudiblePlan) {}, "A/S/1 - B", true},
		{"contested root book", func(*AudiblePlan) {}, ".", true},
		{"no library", func(p *AudiblePlan) { p.LibraryID = 0 }, "", false},
		{"unknown kind", func(p *AudiblePlan) { p.Kind = "elsewhere" }, "", false},
		{"no kind", func(p *AudiblePlan) { p.Kind = "" }, "", false},
		{"unknown state", func(p *AudiblePlan) { p.State = "maybe" }, "", false},
		{"no path", func(p *AudiblePlan) { p.Path = "" }, "", false},
		{"dot-dot path", func(p *AudiblePlan) { p.Path = "A/../../etc/B.m4b" }, "", false},
		{"absolute path", func(p *AudiblePlan) { p.Path = "/A/B.m4b" }, "", false},
		{"long path", func(p *AudiblePlan) { p.Path = strings.Repeat("a/", MaxPathBytes/2) + "B.m4b" }, "", false},
		{"file with a folder", func(p *AudiblePlan) { p.File = "S/B.m4b" }, "", false},
		{"dot-dot file", func(p *AudiblePlan) { p.File = ".." }, "", false},
		{"negative siblings", func(p *AudiblePlan) { p.Siblings = -1 }, "", false},
		{"too many siblings", func(p *AudiblePlan) { p.Siblings = maxAudibleSiblings + 1 }, "", false},
		{"long template", func(p *AudiblePlan) { p.Template = strings.Repeat("a", bridge.MaxAudibleTemplate+1) }, "", false},
		{"unknown conflict", func(p *AudiblePlan) { p.State, p.Conflict = AudiblePlanExists, "something" }, "", false},
		{"dot-dot contest", func(*AudiblePlan) {}, "A/../B", false},
		{"absolute contest", func(*AudiblePlan) {}, "/A/B", false},
	} {
		ti := avail("B000000001", "")
		ti.Planned, ti.ContestedPath = good(), tc.contest
		tc.edit(ti.Planned)
		err := f.c.PutAudibleTitles(f.ctx, "acct0001", "p1", false, []AudibleTitleInput{ti})
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrAudibleInvalid)) {
			t.Errorf("%s: err = %v, want ok %v", tc.name, err, tc.ok)
		}
	}
}

// A contested title names its book (in the account's library) and the
// account's other titles on it: matched to it, or contested over it too; not
// another account's, nor itself. The book's move carries the contest.
func TestAudibleContestedWith(t *testing.T) {
	t.Parallel()
	f := newAudibleFixture(t, "manual")
	if _, err := f.c.PutAudibleAccount(f.ctx, "acct0002", AudibleAccountInput{Label: "y", Marketplace: "us",
		UserID: f.user, LibraryID: f.lib.ID, Policy: "manual", Sync: true}); err != nil {
		t.Fatal(err)
	}
	const book = "Ben Aaronovitch/PG02 - Moon over Soho"
	if _, err := f.c.UpsertBook(f.ctx, &Book{LibraryID: f.lib.ID, RelPath: book, Title: "Moon over Soho"}); err != nil {
		t.Fatal(err)
	}
	a := contested("B000000001", "")
	a.Title, a.ContestedPath = "Rivers of London", book
	b := contested("B000000002", "")
	b.Title, b.ContestedPath = "Moon over Soho", book
	m := avail("B000000003", "")
	m.Title, m.Match = "Another Edition", &AudibleMatch{LibraryID: f.lib.ID, Path: book, How: "fuzzy"}
	lone := contested("B000000004", "")
	lone.ContestedPath = "Elsewhere/Book"
	f.poll(t, "p1", a, b, m, lone)
	if err := f.c.PutAudibleTitles(f.ctx, "acct0002", "q1", true, []AudibleTitleInput{b}); err != nil {
		t.Fatal(err)
	}
	with := func(c *AudibleContest) []string {
		out := []string{}
		for _, w := range c.With {
			out = append(out, w.ASIN+" "+w.Title)
		}
		return out
	}
	page, err := f.c.AudibleTitles(f.ctx, AudibleTitleFilter{Account: "acct0001"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*AudibleTitle{}
	for i := range page.Titles {
		got[page.Titles[i].ASIN] = &page.Titles[i]
	}
	if c := got["B000000001"].Contested; c == nil || c.LibraryID != f.lib.ID || c.Path != book ||
		!slices.Equal(with(c), []string{"B000000003 Another Edition", "B000000002 Moon over Soho"}) {
		t.Fatalf("contested = %+v", c)
	}
	if c := got["B000000002"].Contested; c == nil || !slices.Equal(with(c), []string{"B000000003 Another Edition", "B000000001 Rivers of London"}) {
		t.Fatalf("its partner's = %+v", c)
	}
	if c := got["B000000004"].Contested; c == nil || len(c.With) != 0 || c.With == nil {
		t.Fatalf("a contest with no other title here = %+v", c)
	}
	if got["B000000003"].Contested != nil || got["B000000003"].ContestedPath != "" {
		t.Fatalf("the matched title = %+v", got["B000000003"])
	}
	// One title reads the same (the console's PATCH answer).
	if c := f.title(t, "B000000001").Contested; c == nil || len(c.With) != 2 {
		t.Fatalf("one title = %+v", c)
	}
	// The book moves: the contest follows it.
	if err := f.c.MoveDurableState(f.ctx, f.lib.ID, book, "Ben Aaronovitch/Rivers/2 - Moon over Soho"); err != nil {
		t.Fatal(err)
	}
	if c := f.title(t, "B000000002").Contested; c == nil || c.Path != "Ben Aaronovitch/Rivers/2 - Moon over Soho" ||
		len(c.With) != 2 {
		t.Fatalf("after the move = %+v", c)
	}
	// The library goes: so does every contest over its books.
	if err := f.c.DeleteLibrary(f.ctx, f.lib.ID); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := f.c.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM audible_titles
		WHERE contest_library_id IS NOT NULL OR contest_path <> ''`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("contests left after the library's delete = %d, %v", left, err)
	}
}
