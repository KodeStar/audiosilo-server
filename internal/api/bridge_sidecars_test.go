package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-meta/pkg/model"
	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/names"
	"github.com/kodestar/audiosilo-server/internal/notify"
)

// Bridge Phase 1a: the sidecars.generate kind, the book's facts for a job, the
// heartbeat's estimates, the confirmation dialog's estimate and the job
// notifications.

// sidecarsEstimates is a heartbeat's estimates block: 900 s of ASR and 1.6
// dollars of agent per audio hour, 4 dollars per ebook.
const sidecarsEstimates = `{"sidecars":{"asr_seconds_per_audio_hour":900,"agent_usd_per_audio_hour":1.6,
	"agent_usd_per_ebook":4,"samples":12,"agent_backend":"claude","agent_ready":true,"subscription":true,
	"monthly_budget_usd":100,"month_spent_usd":12.3}}`

// beatWith sends a heartbeat offering both kinds, the env's library mapped as
// given, with estimates (a JSON object, "" for none).
func (b *bridgeEnv) beatWith(t *testing.T, mapped, writable bool, estimates string) {
	t.Helper()
	body := fmt.Sprintf(`{"version":"0.2.0","kinds":["bridge.verify","sidecars.generate"],
		"libraries":[{"library_id":%d,"mount_path":"/media/books","mapped":%t,"writable":%t}]`, b.lib.ID, mapped, writable)
	if estimates != "" {
		body += `,"estimates":` + estimates
	}
	resp, out := b.do(t, "PUT", "/api/v1/bridge/status", b.bridgeTok, body+"}")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", resp.StatusCode, out)
	}
}

func (b *bridgeEnv) queueSidecars(t *testing.T, libID int64, path, params string) (*http.Response, string) {
	t.Helper()
	return b.do(t, "POST", "/api/v1/admin/bridge/jobs", b.adminTok,
		fmt.Sprintf(`{"kind":"sidecars.generate","library_id":%d,"path":%q,"params":%s}`, libID, path, params))
}

func TestBridgeSidecarsQueue(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	b.beatWith(t, true, true, "")
	_, body := b.do(t, "GET", "/api/v1/server", "", "")
	var info struct {
		Bridge *bridgeInfo `json:"bridge"`
	}
	decodeInto(t, body, &info)
	if info.Bridge == nil || !slices.Equal(info.Bridge.Features, []string{"bridge", "sidecars"}) {
		t.Fatalf("features = %s", body)
	}
	resp, body := b.queueSidecars(t, b.lib.ID, mistbornBook, `{}`)
	var job bridge.Job
	decodeInto(t, body, &job)
	if resp.StatusCode != http.StatusCreated || job.Kind != bridge.KindSidecars || string(job.Params) != `{"source":"auto","contribute":"local"}` {
		t.Fatalf("queue: %d %s", resp.StatusCode, body)
	}
	for name, c := range map[string]struct {
		body string
		want int
	}{
		"no path":       {fmt.Sprintf(`{"kind":"sidecars.generate","library_id":%d}`, b.lib.ID), http.StatusBadRequest},
		"bad params":    {fmt.Sprintf(`{"kind":"sidecars.generate","library_id":%d,"path":%q,"params":{"source":"tape"}}`, b.lib.ID, cradleBook), http.StatusBadRequest},
		"unknown param": {fmt.Sprintf(`{"kind":"sidecars.generate","library_id":%d,"path":%q,"params":{"x":1}}`, b.lib.ID, cradleBook), http.StatusBadRequest},
		"no such book":  {fmt.Sprintf(`{"kind":"sidecars.generate","library_id":%d,"path":"Nobody/Nothing"}`, b.lib.ID), http.StatusNotFound},
	} {
		if resp, out := b.do(t, "POST", "/api/v1/admin/bridge/jobs", b.adminTok, c.body); resp.StatusCode != c.want {
			t.Errorf("%s: %d %s, want %d", name, resp.StatusCode, out, c.want)
		}
	}
	// Denied: a library the Bridge can't write to.
	b.beatWith(t, true, false, "")
	resp, body = b.queueSidecars(t, b.lib.ID, cradleBook, `{}`)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, `"library_not_writable"`) {
		t.Fatalf("read-only library: %d %s, want 409 library_not_writable", resp.StatusCode, body)
	}
	// bridge.verify still works there.
	if resp, body := b.do(t, "POST", "/api/v1/admin/bridge/jobs", b.adminTok,
		fmt.Sprintf(`{"kind":"bridge.verify","library_id":%d,"path":%q}`, b.lib.ID, cradleBook)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("verify on a read-only library: %d %s", resp.StatusCode, body)
	}
}

// The book's facts ride on GET /bridge/libraries/{id}/book only when a path was
// asked for.
func TestBridgeBookFacts(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	resp, body := b.do(t, "GET", fmt.Sprintf("/api/v1/bridge/libraries/%d/book?path=%s", b.lib.ID, url.QueryEscape(mistbornBook)), b.bridgeTok, "")
	var out struct {
		Book *bridgeBook `json:"book"`
	}
	decodeInto(t, body, &out)
	book, err := b.cat.GetBookByPath(context.Background(), b.lib.ID, mistbornBook)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || out.Book == nil || out.Book.Title != book.Title || out.Book.Duration != book.Duration ||
		!slices.Equal(out.Book.Authors, orEmpty(names.Split(book.Author))) || out.Book.Narrators == nil || out.Book.WorkID != "" {
		t.Fatalf("book facts: %d %s", resp.StatusCode, body)
	}
	resp, body = b.do(t, "GET", fmt.Sprintf("/api/v1/bridge/libraries/%d/book", b.lib.ID), b.bridgeTok, "")
	if resp.StatusCode != http.StatusOK || strings.Contains(body, `"book"`) {
		t.Fatalf("the sample carries a book block: %d %s", resp.StatusCode, body)
	}
}

// The work id is the metadata cache's: none before the book is looked up (no
// upstream call from the Bridge's route), then the cached one.
func TestBridgeBookFactsWorkID(t *testing.T) {
	t.Parallel()
	b := newBridgeEnvOn(t, newMetaEnv(t, true, 0))
	ctx := context.Background()
	libID := b.lib.ID
	book := &catalog.Book{LibraryID: libID, RelPath: "Andy Weir/The Martian", Title: "The Martian", Author: "Andy Weir; Someone Else",
		Narrator: "R. C. Bray", Series: "Mars", SeriesIndex: 1, SeriesIndexEnd: 3, ASIN: "B00FLIJJSY", Duration: 38040.5, AddedAt: "2020-01-01"}
	if _, err := b.cat.UpsertBook(ctx, book); err != nil {
		t.Fatal(err)
	}
	facts := func() bridgeBook {
		t.Helper()
		resp, body := b.do(t, "GET", fmt.Sprintf("/api/v1/bridge/libraries/%d/book?path=%s", libID, url.QueryEscape(book.RelPath)), b.bridgeTok, "")
		var out struct{ Book bridgeBook }
		decodeInto(t, body, &out)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("book: %d %s", resp.StatusCode, body)
		}
		return out.Book
	}
	got := facts()
	if got.WorkID != "" || got.SeriesIndex != "1-3" || got.Series != "Mars" || got.ASIN != "B00FLIJJSY" || got.Duration != 38040.5 ||
		!slices.Equal(got.Authors, []string{"Andy Weir", "Someone Else"}) || !slices.Equal(got.Narrators, []string{"R. C. Bray"}) {
		t.Fatalf("before a lookup = %+v", got)
	}
	if resp, body := b.do(t, "GET", bookURL(libID, "meta", book.RelPath), b.adminTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("meta: %d %s", resp.StatusCode, body)
	}
	if got := facts(); got.WorkID != "the-martian" {
		t.Fatalf("after a lookup = %+v", got)
	}
}

// The heartbeat's estimates are bounded (an unknown key ignored) and shown on
// GET /admin/bridge.
func TestBridgeHeartbeatEstimates(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	b.beatWith(t, true, true, `{"sidecars":{"asr_seconds_per_audio_hour":900,"agent_usd_per_audio_hour":null,"later_field":1},"podcasts":{}}`)
	_, body := b.do(t, "GET", "/api/v1/admin/bridge", b.adminTok, "")
	var st adminBridge
	decodeInto(t, body, &st)
	if st.Estimates == nil || st.Estimates.Sidecars == nil || st.Estimates.Sidecars.ASRSecondsPerAudioHour != 900 ||
		st.Estimates.Sidecars.AgentUSDPerAudioHour != nil || !strings.Contains(body, `"agent_usd_per_audio_hour":null`) {
		t.Fatalf("admin bridge estimates: %s", body)
	}
	resp, out := b.do(t, "PUT", "/api/v1/bridge/status", b.bridgeTok, `{"version":"0.2.0","estimates":{"sidecars":{"samples":-1}}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a negative figure: %d %s, want 400", resp.StatusCode, out)
	}
	// None sent: none shown.
	b.beat(t, bridge.KindVerify)
	if _, body := b.do(t, "GET", "/api/v1/admin/bridge", b.adminTok, ""); strings.Contains(body, `"estimates"`) {
		t.Fatalf("estimates without any sent: %s", body)
	}
}

func (b *bridgeEnv) estimate(t *testing.T, tok string, libID int64, path string) (*http.Response, bridgeEstimate, string) {
	t.Helper()
	resp, body := b.do(t, "GET", fmt.Sprintf("/api/v1/admin/bridge/estimate?library_id=%d&path=%s&kind=sidecars.generate", libID, url.QueryEscape(path)), tok, "")
	var out bridgeEstimate
	if resp.StatusCode == http.StatusOK {
		decodeInto(t, body, &out)
	}
	return resp, out, body
}

// An ebook beside the audio makes the run read it (no ASR, the per-ebook cost);
// the open job is part of the answer; the route is the admin's.
func TestBridgeEstimateEbookAndAccess(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	root := copyFixtures(t, "Brandon Sanderson/Mistborn/01 - The Final Empire.m4b", cradlePart)
	if err := os.WriteFile(filepath.Join(root, "Brandon Sanderson", "Mistborn", "Mistborn.epub"), []byte("epub"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.ExecContext(context.Background(), `UPDATE libraries SET root = ? WHERE id = ?`, root, b.lib.ID); err != nil {
		t.Fatal(err)
	}
	b.beatWith(t, true, true, sidecarsEstimates)

	resp, est, body := b.estimate(t, b.adminTok, b.lib.ID, mistbornBook)
	if resp.StatusCode != http.StatusOK || est.Source != bridge.SourceEbook || est.Ebook != mistbornBook+"/Mistborn.epub" ||
		est.ASRHours != 0 || est.AgentCostUSD == nil || *est.AgentCostUSD != 4 || !est.Writable || !est.Subscription || !est.AgentReady ||
		est.Has.Community.Recaps || est.Has.Local.Recaps || est.Earlier == nil || len(est.Earlier) != 0 || est.OpenJob != nil ||
		strings.Contains(body, `"open_job"`) {
		t.Fatalf("ebook estimate: %d %s", resp.StatusCode, body)
	}
	// Audio: the ASR hours and the per-hour cost from the book's duration.
	resp, est, body = b.estimate(t, b.adminTok, b.lib.ID, cradleBook)
	book, _ := b.cat.GetBookByPath(context.Background(), b.lib.ID, cradleBook)
	want := bridge.EstimateSidecars(book.Duration, false, &bridge.SidecarsEstimates{ASRSecondsPerAudioHour: 900, AgentUSDPerAudioHour: ptrFloat(1.6)})
	if resp.StatusCode != http.StatusOK || est.Source != bridge.SourceAudio || est.Ebook != "" || strings.Contains(body, `"ebook"`) ||
		est.DurationSeconds != book.Duration || est.ASRHours != want.ASRHours || *est.AgentCostUSD != *want.AgentCostUSD {
		t.Fatalf("audio estimate: %d %s", resp.StatusCode, body)
	}

	if resp, body := b.queueSidecars(t, b.lib.ID, mistbornBook, `{"source":"ebook"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("queue: %d %s", resp.StatusCode, body)
	}
	if _, est, body := b.estimate(t, b.adminTok, b.lib.ID, mistbornBook); est.OpenJob == nil || est.OpenJob.Kind != bridge.KindSidecars {
		t.Fatalf("open job: %s", body)
	}
	// Allowed: an admin's API key. Denied: a user (403), the bridge token (401).
	if resp, _, body := b.estimate(t, b.adminKey, b.lib.ID, mistbornBook); resp.StatusCode != http.StatusOK {
		t.Fatalf("api key: %d %s", resp.StatusCode, body)
	}
	if resp, _, _ := b.estimate(t, b.userTok, b.lib.ID, mistbornBook); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("user: %d, want 403", resp.StatusCode)
	}
	if resp, _, _ := b.estimate(t, b.bridgeTok, b.lib.ID, mistbornBook); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bridge token: %d, want 401", resp.StatusCode)
	}
	for name, c := range map[string]struct {
		query string
		want  int
	}{
		"unknown book":    {fmt.Sprintf("library_id=%d&path=Nobody", b.lib.ID), http.StatusNotFound},
		"hidden path":     {fmt.Sprintf("library_id=%d&path=%s", b.lib.ID, url.QueryEscape(mistbornBook+"/.audiosilo")), http.StatusNotFound},
		"unknown library": {fmt.Sprintf("library_id=9%d&path=x", b.lib.ID), http.StatusNotFound},
		"no path":         {fmt.Sprintf("library_id=%d", b.lib.ID), http.StatusBadRequest},
		"no library":      {"path=x", http.StatusBadRequest},
		"other kind":      {fmt.Sprintf("library_id=%d&path=%s&kind=bridge.verify", b.lib.ID, url.QueryEscape(mistbornBook)), http.StatusBadRequest},
	} {
		if resp, body := b.do(t, "GET", "/api/v1/admin/bridge/estimate?"+c.query, b.adminTok, ""); resp.StatusCode != c.want {
			t.Errorf("%s: %d %s, want %d", name, resp.StatusCode, body, c.want)
		}
	}
	// A Bridge that doesn't run the kind.
	b.beat(t, bridge.KindVerify)
	if resp, _, body := b.estimate(t, b.adminTok, b.lib.ID, mistbornBook); resp.StatusCode != http.StatusConflict || !strings.Contains(body, "kind_unavailable") {
		t.Fatalf("kind unavailable: %d %s", resp.StatusCode, body)
	}
}

func ptrFloat(v float64) *float64 { return &v }

// What a book already has (community from its /meta enrichment, local from its
// attachments) and the earlier books of its series without recaps, in reading
// order: a book with community recaps (cached) or generated ones is left out.
func TestBridgeEstimateHasAndEarlier(t *testing.T) {
	t.Parallel()
	e := newMetaEnv(t, true, 0)
	local := map[string]*catalog.Attachments{
		"Mars/2": {Recaps: &model.Recaps{Recaps: []model.Recap{{Text: "So far."}}}},
		"Mars/4": {Characters: &model.Characters{Characters: []model.Character{{ID: "mark", Name: "Mark"}}}},
		// Characters alone are no recaps: still offered.
		"Mars/3": {Characters: &model.Characters{Characters: []model.Character{{ID: "mark", Name: "Mark"}}}},
	}
	// Set before the first request (the handlers read it).
	e.api.attachments = func(_ context.Context, _ int64, path string) (*catalog.Attachments, error) {
		return local[path], nil
	}
	b := newBridgeEnvOn(t, e)
	ctx := context.Background()
	lib, err := b.cat.CreateLibrary(ctx, catalog.Library{Name: "Series", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, bk := range []catalog.Book{
		{RelPath: "Mars/1", Series: "Mars", SeriesIndex: 1, ASIN: "B00FLIJJSY"}, // community recaps, once cached
		{RelPath: "Mars/0.5", Series: "mars!", SeriesIndex: 0.5, ASIN: "B0UNCACHED"},
		{RelPath: "Mars/2", Series: "Mars", SeriesIndex: 2},
		{RelPath: "Mars/3", Series: "Mars", SeriesIndex: 3},
		{RelPath: "Mars/4", Series: "Mars", SeriesIndex: 4, ASIN: "B00FLIJJSY", Duration: 36000},
		{RelPath: "Mars/5", Series: "Mars", SeriesIndex: 5},
	} {
		bk.LibraryID, bk.Title, bk.AddedAt = lib.ID, "Book "+bk.RelPath, "2020-01-01"
		if _, err := b.cat.UpsertBook(ctx, &bk); err != nil {
			t.Fatal(err)
		}
	}
	b.beatWith(t, true, true, sidecarsEstimates)
	earlier := func(est bridgeEstimate) []string {
		var out []string
		for _, e := range est.Earlier {
			out = append(out, e.Path+"@"+e.SeriesIndex)
		}
		return out
	}
	// Mars/4's own lookup (Enrich) finds its work, which then has characters
	// and recaps.
	resp, est, body := b.estimate(t, b.adminTok, lib.ID, "Mars/4")
	if resp.StatusCode != http.StatusOK || !est.Has.Community.Characters || !est.Has.Community.Recaps ||
		!est.Has.Local.Characters || est.Has.Local.Recaps || est.Writable || est.Source != bridge.SourceAudio || est.ASRHours != 2.5 ||
		est.AgentCostUSD == nil || *est.AgentCostUSD != 16 {
		t.Fatalf("estimate: %d %s", resp.StatusCode, body)
	}
	// That lookup cached the work under the ASIN Mars/1 shares, so Mars/1 has
	// community recaps and is left out; Mars/0.5's ASIN was never looked up, so
	// it is offered (the earlier books are never looked up one by one). Mars/2
	// has generated recaps; Mars/3 generated characters only.
	if got, want := earlier(est), []string{"Mars/0.5@0.5", "Mars/3@3"}; !slices.Equal(got, want) {
		t.Fatalf("earlier = %v, want %v (%s)", got, want, body)
	}
	if _, est, body := b.estimate(t, b.adminTok, lib.ID, "Mars/0.5"); len(est.Earlier) != 0 {
		t.Fatalf("the first book: %s", body)
	}
}

// Metadata off: nothing is community, nothing is looked up.
func TestBridgeEstimateMetadataOff(t *testing.T) {
	t.Parallel()
	b := newBridgeEnvOn(t, newMetaEnv(t, false, 0))
	ctx := context.Background()
	if _, err := b.cat.UpsertBook(ctx, &catalog.Book{LibraryID: b.lib.ID, RelPath: "Andy Weir/The Martian", Title: "The Martian",
		ASIN: "B00FLIJJSY", AddedAt: "2020-01-01"}); err != nil {
		t.Fatal(err)
	}
	b.beatWith(t, true, true, "")
	resp, est, body := b.estimate(t, b.adminTok, b.lib.ID, "Andy Weir/The Martian")
	if resp.StatusCode != http.StatusOK || est.Has.Community.Characters || est.Has.Community.Recaps || est.AgentCostUSD != nil || est.ASRHours != 0 {
		t.Fatalf("metadata off: %d %s", resp.StatusCode, body)
	}
}

// A job's move into done, failed or needs_attention is announced once; a
// keepalive (running -> running, parked -> parked) is not.
func TestBridgeJobNotifications(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	b.beatWith(t, true, true, "")
	ctx := context.Background()
	resp, body := b.queueSidecars(t, b.lib.ID, mistbornBook, `{}`)
	var job bridge.Job
	decodeInto(t, body, &job)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("queue: %d %s", resp.StatusCode, body)
	}
	if resp, body := b.do(t, "GET", "/api/v1/bridge/jobs/next", b.bridgeTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("claim: %d %s", resp.StatusCode, body)
	}
	events := func(kind string) []catalog.ServerEvent {
		t.Helper()
		evs, _, err := b.cat.ListServerEvents(ctx, 0, 50, kind)
		if err != nil {
			t.Fatal(err)
		}
		return evs
	}
	for i, u := range []string{
		`{"attempt":1,"state":"running","progress":{"step":"asr"}}`,
		`{"attempt":1,"state":"running","progress":{"step":"asr","done":3,"total":40}}`,
		`{"attempt":1,"progress":{"step":"asr","done":4,"total":40}}`,
		`{"attempt":1,"state":"needs_attention","error":"no agent","progress":{"step":"waiting","park":{"code":"agent_unavailable","hint":"later"}}}`,
		`{"attempt":1,"state":"needs_attention","progress":{"step":"waiting","park":{"code":"agent_unavailable"}}}`,
		`{"attempt":1,"state":"running","progress":{"step":"fact_pass"}}`,
		`{"attempt":1,"state":"done","result":{"characters":3}}`,
	} {
		if resp, body := b.do(t, "PATCH", fmt.Sprintf("/api/v1/bridge/jobs/%d", job.ID), b.bridgeTok, u); resp.StatusCode != http.StatusOK {
			t.Fatalf("update %d: %d %s", i, resp.StatusCode, body)
		}
		if i < 3 && len(events(notify.KindBridgeJobFailed))+len(events(notify.KindBridgeJobFinished)) != 0 {
			t.Fatalf("a running report (%d) was announced", i)
		}
	}
	failed, finished := events(notify.KindBridgeJobFailed), events(notify.KindBridgeJobFinished)
	if len(failed) != 1 || len(finished) != 1 {
		t.Fatalf("announced %d failed and %d finished, want one each", len(failed), len(finished))
	}
	book, _ := b.cat.GetBookByPath(ctx, b.lib.ID, mistbornBook)
	if f := failed[0].Data; f["code"] != "agent_unavailable" || f["state"] != "needs_attention" || f["title"] != book.Title ||
		f["path"] != mistbornBook || f["library"] != b.lib.Name || f["job_kind"] != bridge.KindSidecars || f["detail"] != "no agent" {
		t.Fatalf("failed event = %v", f)
	}
	if f := finished[0].Data; f["state"] != "done" || f["title"] != book.Title {
		t.Fatalf("finished event = %v", f)
	}
}

// The estimate needs the admin: a user signed in with a password session is
// refused even with a library they can read (allowed/denied pair beside the
// route table's).
func TestBridgeEstimateNeedsAnAdmin(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	b.beatWith(t, true, true, "")
	uid, err := b.auth.CreateUser(context.Background(), "lee", "lee-password", auth.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := b.auth.IssueToken(context.Background(), uid.ID, auth.KindAPI, "script", 0)
	if resp, _, _ := b.estimate(t, tok, b.lib.ID, mistbornBook); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied: a user's api key: %d, want 403", resp.StatusCode)
	}
	if resp, _, body := b.estimate(t, b.adminTok, b.lib.ID, mistbornBook); resp.StatusCode != http.StatusOK {
		t.Fatalf("allowed: the admin: %d %s", resp.StatusCode, body)
	}
}
