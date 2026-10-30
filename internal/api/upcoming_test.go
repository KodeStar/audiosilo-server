package api

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/upcoming"
	"github.com/kodestar/audiosilo-server/pkg/match"
)

// GET /me/upcoming (Track U1): what the job stored, read for one caller. The
// job itself is internal/upcoming's tests; these seed its tables and check the
// transport and the access rules.

const upcomingPath = "/api/v1/me/upcoming"

// upcomingEnv is a test env with a books library holding two series in two
// folders ("Expanse/" and "Other/"), each with the job's results stored.
type upcomingEnv struct {
	*testEnv
	lib *catalog.Library
}

func folded(name string) string { return cmp.Or(match.SeriesKey(name), name) }

func newUpcomingEnv(t *testing.T) *upcomingEnv {
	t.Helper()
	e := newTestEnvWith(t, func(c *config.Config) { c.ServerID = "srv-test" }) // the calendar's UIDs
	ctx := context.Background()
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	u := &upcomingEnv{testEnv: e, lib: lib}
	u.book(t, lib.ID, "Expanse/1", "The Expanse", 1)
	u.book(t, lib.ID, "Other/1", "Other Series", 1)
	u.series(t, lib.ID, "The Expanse", "expanse", []catalog.MetaSeriesMember{
		{WorkID: "e1", Position: "1", Title: "Leviathan Wakes"},
		{WorkID: "e2", Position: "2", Title: "Caliban's War", ReleaseDate: "2012", Authors: []catalog.MetaSeriesPerson{{ID: "jsac", Name: "James S. A. Corey"}}},
		{WorkID: "e3", Position: "3", Title: "Abaddon's Gate", ReleaseDate: "2026-10-01", CoverURL: "https://c/e3.jpg"},
		{WorkID: "e4", Position: "4", Title: "Cibola Burn", ReleaseDate: "2099-11-12"},
		{WorkID: "e5", Position: "5", Title: "Nemesis Games", ReleaseDate: "2098-01"},
	}, []catalog.UpcomingItem{
		{WorkID: "e2", Kind: catalog.UpcomingMissing, ReleaseDate: "2012", Position: "2"},
		{WorkID: "e3", Kind: catalog.UpcomingReleased, ReleaseDate: "2026-10-01", Position: "3"},
		{WorkID: "e4", Kind: catalog.UpcomingPreorder, ReleaseDate: "2099-11-12", Position: "4"},
		{WorkID: "e5", Kind: catalog.UpcomingPreorder, ReleaseDate: "2098-01", Position: "5"},
	})
	u.series(t, lib.ID, "Other Series", "other", []catalog.MetaSeriesMember{
		{WorkID: "o1", Position: "1", Title: "One"}, {WorkID: "o2", Position: "2", Title: "Secret Two"},
	}, []catalog.UpcomingItem{{WorkID: "o2", Kind: catalog.UpcomingMissing, Position: "2"}})
	if err := e.cat.SaveUpcomingRunState(ctx, catalog.UpcomingRun{LastSuccessAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z")}); err != nil {
		t.Fatal(err)
	}
	return u
}

func (u *upcomingEnv) book(t *testing.T, libID int64, path, series string, pos float64) {
	t.Helper()
	if _, err := u.cat.UpsertBook(context.Background(), &catalog.Book{LibraryID: libID, RelPath: path, Title: path,
		Author: "A", Series: series, SeriesIndex: pos, AddedAt: "2020-01-01"}); err != nil {
		t.Fatal(err)
	}
}

func (u *upcomingEnv) series(t *testing.T, libID int64, name, id string, members []catalog.MetaSeriesMember, items []catalog.UpcomingItem) {
	t.Helper()
	ctx := context.Background()
	at := "2026-10-20T04:00:00.000Z"
	if err := u.cat.SetUpcomingSeries(ctx, libID, folded(name), name, 1, []string{id}, at); err != nil {
		t.Fatal(err)
	}
	if err := u.cat.SaveMetaSeries(ctx, catalog.MetaSeriesRecord{ID: id, Name: name, Members: members, FetchedAt: at}); err != nil {
		t.Fatal(err)
	}
	// Each item carries its work as the series lists it, as the job stores it.
	for i := range items {
		items[i].SeriesID = id
		for _, m := range members {
			if m.WorkID == items[i].WorkID {
				items[i].Title, items[i].Authors, items[i].CoverURL = m.Title, m.Authors, m.CoverURL
			}
		}
	}
	if err := u.cat.ReplaceUpcomingItems(ctx, libID, folded(name), items, nil, []string{id}, at); err != nil {
		t.Fatal(err)
	}
}

func (u *upcomingEnv) get(t *testing.T, tok string) upcoming.Answer {
	t.Helper()
	resp, body := u.do(t, "GET", upcomingPath, tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /me/upcoming = %d %s", resp.StatusCode, body)
	}
	var ans upcoming.Answer
	if err := json.Unmarshal([]byte(body), &ans); err != nil {
		t.Fatal(err)
	}
	return ans
}

func workIDs(ans upcoming.Answer) []string {
	var out []string
	for _, it := range ans.Items {
		out = append(out, it.Work.ID+"="+it.Kind)
	}
	return out
}

// The wire as the plan freezes it, ordered preorders soonest first, releases
// newest first, then missing volumes by series name ("Other Series" before
// "The Expanse") and position.
func TestUpcomingWire(t *testing.T) {
	t.Parallel()
	u := newUpcomingEnv(t)
	tok, _ := u.auth.IssueToken(context.Background(), u.adminID, auth.KindSession, "t", 0)
	resp, body := u.do(t, "GET", upcomingPath, tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d %s", resp.StatusCode, body)
	}
	for _, want := range []string{`"status":"ok"`, `"generated_at":"`, `"window_days":90`,
		`{"kind":"missing","release_date":"2012","first_seen_at":"2026-10-20T04:00:00.000Z","work":{"id":"e2","title":"Caliban's War","authors":[{"id":"jsac","name":"James S. A. Corey"}],"web_url":"https://meta.audiosilo.app/work?id=e2"},"series":{"id":"expanse","name":"The Expanse","position":"2"},"local":{"library_id":`,
		`"cover_url":"https://c/e3.jpg"`, `"local":{"library_id":` + strconv.FormatInt(u.lib.ID, 10) + `,"series":"The Expanse"}`} {
		if !strings.Contains(body, want) {
			t.Fatalf("answer lacks %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, `"release_date":""`) {
		t.Fatalf("an unknown date is omitted: %s", body)
	}
	ans := u.get(t, tok)
	if got, want := workIDs(ans), []string{"e5=preorder", "e4=preorder", "e3=released", "o2=missing", "e2=missing"}; !slices.Equal(got, want) {
		t.Fatalf("items = %v, want %v", got, want)
	}
}

// Allowed and denied: a listener sees a series' items only through a book of
// it they can see.
func TestUpcomingScoped(t *testing.T) {
	t.Parallel()
	u := newUpcomingEnv(t)
	ctx := context.Background()
	reader, _ := u.auth.CreateUser(ctx, "reader", "reader-password", auth.RoleUser)
	share, err := u.cat.CreateShare(ctx, catalog.Share{Name: "Expanse only"})
	if err != nil {
		t.Fatal(err)
	}
	if err := u.cat.AddSharePath(ctx, share.ID, catalog.PathRule{LibraryID: u.lib.ID, Path: "Expanse"}); err != nil {
		t.Fatal(err)
	}
	if err := u.cat.GrantShare(ctx, reader.ID, share.ID); err != nil {
		t.Fatal(err)
	}
	tok, _ := u.auth.IssueToken(ctx, reader.ID, auth.KindSession, "t", 0)
	got := workIDs(u.get(t, tok))
	if !slices.Contains(got, "e2=missing") {
		t.Fatalf("a series under the share must be listed: %v", got)
	}
	if slices.Contains(got, "o2=missing") {
		t.Fatalf("a series held only outside the share must not be: %v", got)
	}
	// No access at all: nothing.
	stranger, _ := u.auth.CreateUser(ctx, "stranger", "stranger-password", auth.RoleUser)
	stok, _ := u.auth.IssueToken(ctx, stranger.ID, auth.KindSession, "t", 0)
	if ans := u.get(t, stok); len(ans.Items) != 0 {
		t.Fatalf("a caller with no access sees %v", workIDs(ans))
	}
}

// A copy of the work the caller can see anywhere hides the item; a copy they
// can't see doesn't. Each work is listed once.
func TestUpcomingOwnCopyElsewhere(t *testing.T) {
	t.Parallel()
	u := newUpcomingEnv(t)
	ctx := context.Background()
	other, err := u.cat.CreateLibrary(ctx, catalog.Library{Name: "Other", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	u.book(t, other.ID, "Loose/Caliban", "", 0)
	if err := u.cat.SaveUpcomingOwned(ctx, []catalog.UpcomingOwned{{Ref: catalog.Ref{LibraryID: other.ID, Path: "Loose/Caliban"},
		IDKey: "a:B0", WorkID: "e2", CheckedAt: "2026-10-20T04:00:00.000Z"}}, nil); err != nil {
		t.Fatal(err)
	}
	// The same series held in the other library too: e3 is listed once.
	u.book(t, other.ID, "Expanse/1", "The Expanse", 1)
	u.series(t, other.ID, "The Expanse", "expanse", []catalog.MetaSeriesMember{
		{WorkID: "e2", Position: "2", ReleaseDate: "2012"}, {WorkID: "e3", Position: "3", ReleaseDate: "2026-10-01"},
	},
		[]catalog.UpcomingItem{{WorkID: "e3", Kind: catalog.UpcomingReleased, ReleaseDate: "2026-10-01", Position: "3"}})

	adminTok, _ := u.auth.IssueToken(ctx, u.adminID, auth.KindSession, "t", 0)
	got := workIDs(u.get(t, adminTok))
	if slices.Contains(got, "e2=missing") {
		t.Fatalf("a work the caller holds elsewhere must be hidden: %v", got)
	}
	n := 0
	for _, g := range got {
		if g == "e3=released" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("a work in two of the caller's series is listed once: %v", got)
	}

	// A listener granted only the first library can't see the other copy.
	reader, _ := u.auth.CreateUser(ctx, "reader", "reader-password", auth.RoleUser)
	if err := u.cat.GrantWholeLibrary(ctx, reader.ID, u.lib.ID); err != nil {
		t.Fatal(err)
	}
	tok, _ := u.auth.IssueToken(ctx, reader.ID, auth.KindSession, "t", 0)
	if got := workIDs(u.get(t, tok)); !slices.Contains(got, "e2=missing") {
		t.Fatalf("a copy outside the caller's access must not hide the item: %v", got)
	}
}

func TestUpcomingPodcastLibrariesExcluded(t *testing.T) {
	t.Parallel()
	u := newUpcomingEnv(t)
	ctx := context.Background()
	pods, err := u.cat.CreateLibrary(ctx, catalog.Library{Name: "Pods", Root: t.TempDir(), Kind: catalog.KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	u.book(t, pods.ID, "Show/1", "Show Series", 1)
	u.series(t, pods.ID, "Show Series", "show", []catalog.MetaSeriesMember{{WorkID: "p2", Position: "2"}},
		[]catalog.UpcomingItem{{WorkID: "p2", Kind: catalog.UpcomingMissing, Position: "2"}})
	tok, _ := u.auth.IssueToken(ctx, u.adminID, auth.KindSession, "t", 0)
	if got := workIDs(u.get(t, tok)); slices.Contains(got, "p2=missing") {
		t.Fatalf("a podcast library's rows must never be listed: %v", got)
	}
}

func TestUpcomingStatusAndPreorderDay(t *testing.T) {
	t.Parallel()
	u := newUpcomingEnv(t)
	ctx := context.Background()
	tok, _ := u.auth.IssueToken(ctx, u.adminID, auth.KindSession, "t", 0)
	// Never run: pending.
	if err := u.cat.SaveUpcomingRunState(ctx, catalog.UpcomingRun{}); err != nil {
		t.Fatal(err)
	}
	if ans := u.get(t, tok); ans.Status != upcoming.StatusPending || ans.GeneratedAt != "" {
		t.Fatalf("status = %q %q", ans.Status, ans.GeneratedAt)
	}
	// No success for 48 hours: stale.
	old := time.Now().Add(-49 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	if err := u.cat.SaveUpcomingRunState(ctx, catalog.UpcomingRun{LastSuccessAt: old}); err != nil {
		t.Fatal(err)
	}
	if ans := u.get(t, tok); ans.Status != upcoming.StatusStale || ans.GeneratedAt != old {
		t.Fatalf("status = %q %q", ans.Status, ans.GeneratedAt)
	}
	// A preorder whose day came since the run reads as released.
	u.series(t, u.lib.ID, "The Expanse", "expanse", []catalog.MetaSeriesMember{{WorkID: "e9", Position: "9", ReleaseDate: "2020-01-01"}},
		[]catalog.UpcomingItem{{WorkID: "e9", Kind: catalog.UpcomingPreorder, ReleaseDate: "2020-01-01", Position: "9"}})
	if got := workIDs(u.get(t, tok)); !slices.Contains(got, "e9=released") {
		t.Fatalf("items = %v", got)
	}
}

// Metadata off: 404 metadata_off; the job switched off: 404 upcoming_off and
// the capability false; back on: listed again.
func TestUpcomingOffAndCapability(t *testing.T) {
	t.Parallel()
	u := newUpcomingEnv(t)
	tok, _ := u.auth.IssueToken(context.Background(), u.adminID, auth.KindSession, "t", 0)
	// upcoming_calendar and upcoming_covers (Track U3) follow upcoming exactly,
	// false (never absent) while it is off.
	capability := func() string {
		_, si := u.do(t, "GET", "/api/v1/server", "", "")
		switch {
		case strings.Contains(si, `"upcoming":true`) && strings.Contains(si, `"upcoming_calendar":true`) && strings.Contains(si, `"upcoming_covers":true`):
			return "on"
		case strings.Contains(si, `"upcoming":false`) && strings.Contains(si, `"upcoming_calendar":false`) && strings.Contains(si, `"upcoming_covers":false`):
			return "off"
		}
		return "absent or mixed: " + si
	}
	if c := capability(); c != "on" {
		t.Fatalf("capability with metadata on = %s", c)
	}
	resp, body := u.do(t, "GET", "/api/v1/admin/settings", tok, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"upcoming":true`) {
		t.Fatalf("settings = %d %s", resp.StatusCode, body)
	}

	if r, b := u.do(t, "PATCH", "/api/v1/admin/settings", tok, `{"metadata":{"upcoming":false}}`); r.StatusCode != http.StatusOK || !strings.Contains(b, `"upcoming":false`) {
		t.Fatalf("switch off = %d %s", r.StatusCode, b)
	}
	if c := capability(); c != "off" {
		t.Fatalf("capability switched off = %s", c)
	}
	if r, b := u.do(t, "GET", upcomingPath, tok, ""); r.StatusCode != http.StatusNotFound || !strings.Contains(b, `"upcoming_off"`) {
		t.Fatalf("GET while off = %d %s", r.StatusCode, b)
	}

	if r, b := u.do(t, "PATCH", "/api/v1/admin/settings", tok, `{"metadata":{"upcoming":true,"enabled":false}}`); r.StatusCode != http.StatusOK {
		t.Fatalf("metadata off = %d %s", r.StatusCode, b)
	}
	if c := capability(); c != "off" {
		t.Fatalf("capability with metadata off = %s", c)
	}
	if r, b := u.do(t, "GET", upcomingPath, tok, ""); r.StatusCode != http.StatusNotFound || !strings.Contains(b, `"metadata_off"`) {
		t.Fatalf("GET with metadata off = %d %s", r.StatusCode, b)
	}

	if r, b := u.do(t, "PATCH", "/api/v1/admin/settings", tok, `{"metadata":{"enabled":true}}`); r.StatusCode != http.StatusOK {
		t.Fatalf("metadata on = %d %s", r.StatusCode, b)
	}
	if ans := u.get(t, tok); len(ans.Items) == 0 {
		t.Fatal("items must be listed again")
	}
}

func TestUpcomingRefusals(t *testing.T) {
	t.Parallel()
	if r, _ := newTestEnv(t).do(t, "GET", upcomingPath, "", ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d, want 401", r.StatusCode)
	}
	m := newMemberEnv(t)
	_, kidTok := m.kid(t, m.adamTok, "Evie")
	if r, b := m.do(t, "GET", upcomingPath, kidTok, ""); r.StatusCode != http.StatusForbidden || !strings.Contains(b, `"kid_account"`) {
		t.Fatalf("kid = %d %s, want 403 kid_account", r.StatusCode, b)
	}
	// The kid's grown-up is answered.
	if r, b := m.do(t, "GET", upcomingPath, m.adamTok, ""); r.StatusCode != http.StatusOK || !strings.Contains(b, `"status":"pending"`) {
		t.Fatalf("grown-up = %d %s", r.StatusCode, b)
	}
}
