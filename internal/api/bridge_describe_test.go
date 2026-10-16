package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// Bridge Phase 1c: own-words descriptions. The sidecars.describe kind, the
// estimate's can_describe and has.*.description, the console's book detail and
// /meta's generated_description.

// testDescriptionText is a description the server takes (200 to 1500 code
// points, no control character but a line break).
var testDescriptionText = strings.Repeat("A harbour town waits out a long winter. ", 6)

// beatDescribe sends a heartbeat offering verify, generate and describe, the
// env's library mapped and writable as given.
func (b *bridgeEnv) beatDescribe(t *testing.T, writable bool) {
	t.Helper()
	body := fmt.Sprintf(`{"version":"0.17.0","kinds":["bridge.verify","sidecars.generate","sidecars.describe"],
		"libraries":[{"library_id":%d,"mount_path":"/media/books","mapped":true,"writable":%t}]}`, b.lib.ID, writable)
	if resp, out := b.do(t, "PUT", "/api/v1/bridge/status", b.bridgeTok, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", resp.StatusCode, out)
	}
}

func (b *bridgeEnv) queueDescribe(t *testing.T, tok, path, params string) (*http.Response, string) {
	t.Helper()
	body := fmt.Sprintf(`{"kind":"sidecars.describe","library_id":%d,"path":%q`, b.lib.ID, path)
	if params != "" {
		body += `,"params":` + params
	}
	return b.do(t, "POST", "/api/v1/admin/bridge/jobs", tok, body+"}")
}

// TestBridgeDescribeQueue: sidecars.describe takes no params ({} or none; any
// field is a 400), needs a book, a Bridge that runs it (409 kind_unavailable)
// and a library it can write to (409 library_not_writable), and is refused while
// a sidecars.generate job is open for the book (409 generate_open), which writes
// the description itself; a generate job is never refused for an open describe.
// The route is the admin's: a user is 403, the bridge token 401.
func TestBridgeDescribeQueue(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	// Denied: a Bridge that doesn't run the kind (an older one).
	b.beatWith(t, true, true, "")
	if resp, body := b.queueDescribe(t, b.adminTok, mistbornBook, `{}`); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(body, `"code":"kind_unavailable"`) {
		t.Fatalf("an older Bridge: %d %s, want 409 kind_unavailable", resp.StatusCode, body)
	}
	b.beatDescribe(t, true)
	_, body := b.do(t, "GET", "/api/v1/server", "", "")
	if !strings.Contains(body, `"features":["bridge","sidecars"]`) {
		t.Fatalf("features = %s", body)
	}

	// Allowed: params absent or {}, stored as {}; a second press is the open job.
	resp, body := b.queueDescribe(t, b.adminTok, mistbornBook, "")
	var job bridge.Job
	decodeInto(t, body, &job)
	if resp.StatusCode != http.StatusCreated || job.Kind != bridge.KindDescribe || string(job.Params) != `{}` {
		t.Fatalf("queue: %d %s", resp.StatusCode, body)
	}
	if resp, body := b.queueDescribe(t, b.adminTok, mistbornBook, `{}`); resp.StatusCode != http.StatusOK || !strings.Contains(body, fmt.Sprintf(`"id":%d,`, job.ID)) {
		t.Fatalf("second press: %d %s, want the open job", resp.StatusCode, body)
	}
	for name, c := range map[string]struct {
		body string
		want int
	}{
		"a param":       {fmt.Sprintf(`{"kind":"sidecars.describe","library_id":%d,"path":%q,"params":{"source":"audio"}}`, b.lib.ID, cradleBook), http.StatusBadRequest},
		"not an object": {fmt.Sprintf(`{"kind":"sidecars.describe","library_id":%d,"path":%q,"params":[]}`, b.lib.ID, cradleBook), http.StatusBadRequest},
		"no path":       {fmt.Sprintf(`{"kind":"sidecars.describe","library_id":%d}`, b.lib.ID), http.StatusBadRequest},
		"no such book":  {fmt.Sprintf(`{"kind":"sidecars.describe","library_id":%d,"path":"Nobody/Nothing"}`, b.lib.ID), http.StatusNotFound},
	} {
		if resp, out := b.do(t, "POST", "/api/v1/admin/bridge/jobs", b.adminTok, c.body); resp.StatusCode != c.want {
			t.Errorf("%s: %d %s, want %d", name, resp.StatusCode, out, c.want)
		}
	}
	// Denied: a user, and the bridge token.
	if resp, body := b.queueDescribe(t, b.userTok, cradleBook, `{}`); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a user: %d %s, want 403", resp.StatusCode, body)
	}
	if resp, body := b.queueDescribe(t, b.bridgeTok, cradleBook, `{}`); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the bridge token: %d %s, want 401", resp.StatusCode, body)
	}

	// A generate job is never refused for the open describe job (it writes a
	// description of its own; the describe job stays queued).
	if resp, body := b.queueSidecars(t, b.lib.ID, mistbornBook, `{}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("generate beside an open describe: %d %s", resp.StatusCode, body)
	}
	// Denied: a describe for a book whose generate job is open.
	if resp, body := b.queueSidecars(t, b.lib.ID, cradleBook, `{}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("generate: %d %s", resp.StatusCode, body)
	}
	if resp, body := b.queueDescribe(t, b.adminTok, cradleBook, `{}`); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(body, `"code":"generate_open"`) {
		t.Fatalf("describe with a generate open: %d %s, want 409 generate_open", resp.StatusCode, body)
	}
	// Denied: a library the Bridge can't write to (the open job still answers).
	b.beatDescribe(t, false)
	if resp, body := b.queueDescribe(t, b.adminTok, mistbornBook, `{}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("the open job on a read-only library: %d %s", resp.StatusCode, body)
	}
	if _, err := b.api.bridge.Cancel(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if resp, body := b.queueDescribe(t, b.adminTok, mistbornBook, `{}`); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(body, `"code":"library_not_writable"`) {
		t.Fatalf("a read-only library: %d %s, want 409 library_not_writable", resp.StatusCode, body)
	}
}

// TestBridgeEstimateDescribe: the estimate (brief or full) says whether the
// Bridge can write a description (online and running sidecars.describe) and
// whether the book has one, generated (its attachments) or from the community
// (community_description; the core description doesn't count).
func TestBridgeEstimateDescribe(t *testing.T) {
	t.Parallel()
	b := newBridgeEnvOn(t, generatedMetaEnv(t, `{"id":"w","title":"Community Title","authors":[],"language":"en",
		"description":"The publisher's blurb.","community_description":{"text":"A community description.","license":"CC-BY-SA-4.0"}}`))
	ctx := context.Background()
	for _, bk := range []catalog.Book{
		{RelPath: "A/Matched", ASIN: "B0MATCHED"},
		{RelPath: "A/Local"},
	} {
		bk.LibraryID, bk.Title, bk.AddedAt = b.lib.ID, bk.RelPath, "2020-01-01"
		if _, err := b.cat.UpsertBook(ctx, &bk); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.cat.SetAttachments(ctx, b.lib.ID, map[string]catalog.AttachmentsReading{
		"A/Local": {Sig: "description.json:1:1", Files: metadata.AttachmentFiles{
			Description: &model.Description{Work: "w", Text: testDescriptionText, License: "CC-BY-SA-4.0"}}},
	}); err != nil {
		t.Fatal(err)
	}

	// An older Bridge: no description offered.
	b.beatWith(t, true, true, "")
	if est := b.briefEstimate(t, b.lib.ID, "A/Local"); est.CanDescribe || !est.Has.Local.Description {
		t.Fatalf("older Bridge = %+v", est)
	}
	b.beatDescribe(t, true)
	for name, extra := range map[string]string{"brief": "&brief=1", "full": ""} {
		resp, est, body := b.estimateQuery(t, b.adminTok, b.lib.ID, "A/Local", extra)
		if resp.StatusCode != http.StatusOK || !est.CanDescribe || !est.Has.Local.Description || est.Has.Community.Description ||
			!strings.Contains(body, `"can_describe":true`) {
			t.Fatalf("%s, local: %d %s", name, resp.StatusCode, body)
		}
		resp, est, body = b.estimateQuery(t, b.adminTok, b.lib.ID, "A/Matched", extra)
		if resp.StatusCode != http.StatusOK || !est.Has.Community.Description || est.Has.Local.Description || est.Has.CommunityUnknown {
			t.Fatalf("%s, matched: %d %s", name, resp.StatusCode, body)
		}
	}
	// Offline: the kind is still advertised by the last heartbeat, but the
	// Bridge couldn't run it now.
	old := time.Now().Add(-bridge.OnlineWindow - time.Second).UTC().Format(time.RFC3339)
	if _, err := b.db.ExecContext(ctx, `UPDATE bridge_status SET heartbeat_at = ?`, old); err != nil {
		t.Fatal(err)
	}
	b.api.bridge.Forget()
	if est := b.briefEstimate(t, b.lib.ID, "A/Local"); est.CanDescribe {
		t.Fatalf("offline = %+v, want can_describe false", est)
	}
}

// TestBridgeEstimateNoCommunityDescription: a community work with only the core
// description has no community description (the console offers one).
func TestBridgeEstimateNoCommunityDescription(t *testing.T) {
	t.Parallel()
	b := newBridgeEnvOn(t, generatedMetaEnv(t, `{"id":"w","title":"Community Title","authors":[],"language":"en",
		"description":"The publisher's blurb."}`))
	bk := catalog.Book{LibraryID: b.lib.ID, RelPath: "A/Matched", Title: "Matched", ASIN: "B0MATCHED", AddedAt: "2020-01-01"}
	if _, err := b.cat.UpsertBook(context.Background(), &bk); err != nil {
		t.Fatal(err)
	}
	b.beatDescribe(t, true)
	if est := b.briefEstimate(t, b.lib.ID, "A/Matched"); est.Has.Community.Description || est.Has.CommunityUnknown {
		t.Fatalf("core description only = %+v", est.Has)
	}
}

// TestAdminBookGeneratedDescription: the console's book detail carries the
// book's generated description with its own time, only when it has one; the
// route is the admin's.
func TestAdminBookGeneratedDescription(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	ctx := context.Background()
	if err := b.cat.SetAttachments(ctx, b.lib.ID, map[string]catalog.AttachmentsReading{
		mistbornBook: {Sig: "description.json:1:1,bridge.json:1:1", Files: metadata.AttachmentFiles{
			Description: &model.Description{Work: "w", Text: testDescriptionText, License: "CC-BY-SA-4.0"},
			Provenance:  &metadata.AttachmentProvenance{GeneratedAt: "2026-10-15T10:00:00Z", DescriptionAt: "2026-10-16T08:00:00Z"}}},
	}); err != nil {
		t.Fatal(err)
	}
	detail := func(tok, path string) (*http.Response, string) {
		return b.do(t, "GET", fmt.Sprintf("/api/v1/admin/libraries/%d/book?path=%s", b.lib.ID, url.QueryEscape(path)), tok, "")
	}
	resp, body := detail(b.adminTok, mistbornBook)
	var d struct {
		GeneratedDescription *struct {
			Text        string `json:"text"`
			GeneratedAt string `json:"generated_at"`
		} `json:"generated_description"`
	}
	decodeInto(t, body, &d)
	if resp.StatusCode != http.StatusOK || d.GeneratedDescription == nil || d.GeneratedDescription.Text != testDescriptionText ||
		d.GeneratedDescription.GeneratedAt != "2026-10-16T08:00:00Z" {
		t.Fatalf("detail: %d %s", resp.StatusCode, body)
	}
	if resp, body := detail(b.adminTok, cradleBook); resp.StatusCode != http.StatusOK || strings.Contains(body, "generated_description") {
		t.Fatalf("detail without one: %d %s", resp.StatusCode, body)
	}
	if resp, _ := detail(b.userTok, mistbornBook); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a user: %d, want 403", resp.StatusCode)
	}
}

// setDescription stores a generated description (alone, or beside what is
// stored: files) for the book at path.
func setDescription(t *testing.T, e *testEnv, libID int64, path, sig string, files metadata.AttachmentFiles) {
	t.Helper()
	files.Description = &model.Description{Work: "w", Text: testDescriptionText, License: "CC-BY-SA-4.0"}
	if err := e.cat.SetAttachments(context.Background(), libID, map[string]catalog.AttachmentsReading{path: {Sig: sig, Files: files}}); err != nil {
		t.Fatal(err)
	}
}

// descriptionBody is the slice of /meta the description tests read.
type descriptionBody struct {
	Matched bool `json:"matched"`
	Work    *struct {
		Title                string           `json:"title"`
		Description          string           `json:"description"`
		CommunityDescription *json.RawMessage `json:"community_description"`
		GeneratedDescription *struct {
			Text string `json:"text"`
		} `json:"generated_description"`
		Attribution *json.RawMessage `json:"attribution"`
		Generated   *struct {
			Characters  bool   `json:"characters"`
			Recaps      bool   `json:"recaps"`
			Description bool   `json:"description"`
			GeneratedAt string `json:"generated_at"`
		} `json:"generated"`
	} `json:"work"`
}

func getDescription(t *testing.T, e *testEnv, libID int64, path, query, tok string) (descriptionBody, string) {
	t.Helper()
	resp, body := e.do(t, "GET", "/api/v1/libraries/"+strconv.FormatInt(libID, 10)+"/meta?path="+escape(path)+query, tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("meta %s: %d %s", path, resp.StatusCode, body)
	}
	var g descriptionBody
	decodeInto(t, body, &g)
	return g, body
}

// TestMetaGeneratedDescriptionMatched: a matched work with no community
// description takes the generated one in generated_description (never
// community_description), beside its core description, marked generated, with
// no attribution of its own; spoilers=hide keeps it. A work with a community
// description ignores it.
func TestMetaGeneratedDescriptionMatched(t *testing.T) {
	t.Parallel()
	e := generatedMetaEnv(t, `{"id":"w","title":"Community Title","authors":[],"language":"en","description":"The publisher's blurb."}`)
	lib := seedChapteredBook(t, e, "A/Two", "B0TWO")
	setDescription(t, e, lib.ID, "A/Two", "description.json:1:1", metadata.AttachmentFiles{})
	tok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)

	for _, q := range []string{"", "&spoilers=hide"} {
		g, body := getDescription(t, e, lib.ID, "A/Two", q, tok)
		if !g.Matched || g.Work == nil || g.Work.GeneratedDescription == nil || g.Work.GeneratedDescription.Text != testDescriptionText ||
			g.Work.CommunityDescription != nil || g.Work.Description != "The publisher's blurb." {
			t.Fatalf("matched (%q) = %s", q, body)
		}
		if gen := g.Work.Generated; gen == nil || !gen.Description || gen.Characters || gen.Recaps || g.Work.Attribution != nil {
			t.Errorf("matched (%q): generated = %+v, attribution = %v", q, gen, g.Work.Attribution != nil)
		}
	}

	c := generatedMetaEnv(t, `{"id":"w","title":"Community Title","authors":[],"language":"en",
		"community_description":{"text":"A community description.","license":"CC-BY-SA-4.0"}}`)
	clib := seedChapteredBook(t, c, "A/Two", "B0TWO")
	setDescription(t, c, clib.ID, "A/Two", "description.json:1:1", metadata.AttachmentFiles{})
	ctok, _ := c.auth.IssueToken(context.Background(), c.adminID, auth.KindSession, "t", 0)
	if g, body := getDescription(t, c, clib.ID, "A/Two", "", ctok); g.Work == nil || g.Work.GeneratedDescription != nil ||
		g.Work.CommunityDescription == nil || g.Work.Generated != nil || g.Work.Attribution == nil {
		t.Fatalf("community description = %s", body)
	}
}

// TestMetaGeneratedDescriptionBesideCommunityContent: a matched work with
// community characters and recaps but no community description takes only the
// generated description (generatedFor reads just that part, Fillable): the
// community's characters and recaps stay, and the mark names the description
// alone, dated by the description's own time (a description-only job keeps the
// earlier run's in bridge.json's generated_at).
func TestMetaGeneratedDescriptionBesideCommunityContent(t *testing.T) {
	t.Parallel()
	e := generatedMetaEnv(t, `{"id":"w","title":"Community Title","authors":[],"language":"en",
		"characters":[{"id":"community-hero","name":"Hero","reveal":{"chapter":1}}],
		"recaps":[{"through":{"chapter":1},"text":"Community recap."}]}`)
	lib := seedChapteredBook(t, e, "A/Two", "B0TWO")
	setDescription(t, e, lib.ID, "A/Two", "characters.json:1:1,recaps.json:1:1,description.json:1:1,bridge.json:1:1", metadata.AttachmentFiles{
		Characters: &model.Characters{Work: "w", Characters: []model.Character{{ID: "gen", Name: "Gen", Reveal: model.Position{Chapter: 1}}}},
		Recaps:     &model.Recaps{Work: "w", Recaps: []model.Recap{{Through: model.Position{Chapter: 1}, Text: "Gen recap."}}},
		Provenance: &metadata.AttachmentProvenance{GeneratedAt: "2026-07-01T10:00:00Z", DescriptionAt: "2026-10-16T08:00:00Z"}})
	tok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)

	g, body := getDescription(t, e, lib.ID, "A/Two", "", tok)
	if g.Work == nil || g.Work.GeneratedDescription == nil || g.Work.GeneratedDescription.Text != testDescriptionText {
		t.Fatalf("work = %s", body)
	}
	if gen := g.Work.Generated; gen == nil || !gen.Description || gen.Characters || gen.Recaps || gen.GeneratedAt != "2026-10-16T08:00:00Z" {
		t.Errorf("generated = %+v", gen)
	}
	if !strings.Contains(body, `"community-hero"`) || !strings.Contains(body, "Community recap.") ||
		strings.Contains(body, `"gen"`) || strings.Contains(body, "Gen recap.") {
		t.Errorf("community content not kept: %s", body)
	}
}

// TestMetaGeneratedDescriptionUnmatched: a book with no match and only a
// generated description answers matched false with a work of the book's title
// and the description, no attribution, kept under spoilers=hide; beside
// generated characters the mark names both.
func TestMetaGeneratedDescriptionUnmatched(t *testing.T) {
	t.Parallel()
	e := newMetaEnv(t, true, 0)
	lib := seedChapteredBook(t, e, "A/Two", "")
	setDescription(t, e, lib.ID, "A/Two", "description.json:1:1", metadata.AttachmentFiles{})
	tok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	for _, q := range []string{"", "&spoilers=hide"} {
		g, body := getDescription(t, e, lib.ID, "A/Two", q, tok)
		if g.Matched || g.Work == nil || g.Work.Title != "Two" || g.Work.GeneratedDescription == nil || g.Work.Attribution != nil ||
			g.Work.Generated == nil || !g.Work.Generated.Description || g.Work.Generated.Characters {
			t.Fatalf("unmatched (%q) = %s", q, body)
		}
		if !strings.HasPrefix(body, `{"matched":false,"work":{`) {
			t.Errorf("unmatched (%q) shape = %s", q, body)
		}
	}
	setDescription(t, e, lib.ID, "A/Two", "characters.json:1:1,description.json:1:1", metadata.AttachmentFiles{
		Characters: &model.Characters{Work: "w", Characters: []model.Character{{ID: "c", Name: "C", Reveal: model.Position{Chapter: 1}}}}})
	if g, body := getDescription(t, e, lib.ID, "A/Two", "", tok); g.Work == nil || !g.Work.Generated.Characters || !g.Work.Generated.Description {
		t.Fatalf("with characters = %s", body)
	}
}
