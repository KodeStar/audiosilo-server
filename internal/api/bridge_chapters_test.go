package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/notify"
)

// Bridge Phase 1d: chapterless books. The chapters.detect kind, and the
// generated chapter source on the player's wire.

// beatDetect sends a heartbeat offering verify, generate and detect, the env's
// library mapped and writable as given.
func (b *bridgeEnv) beatDetect(t *testing.T, writable bool) {
	t.Helper()
	body := fmt.Sprintf(`{"version":"0.18.0","kinds":["bridge.verify","sidecars.generate","chapters.detect"],
		"libraries":[{"library_id":%d,"mount_path":"/media/books","mapped":true,"writable":%t}]}`, b.lib.ID, writable)
	if resp, out := b.do(t, "PUT", "/api/v1/bridge/status", b.bridgeTok, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", resp.StatusCode, out)
	}
}

func (b *bridgeEnv) queueDetect(t *testing.T, tok, path, params string) (*http.Response, string) {
	t.Helper()
	body := fmt.Sprintf(`{"kind":"chapters.detect","library_id":%d,"path":%q`, b.lib.ID, path)
	if params != "" {
		body += `,"params":` + params
	}
	return b.do(t, "POST", "/api/v1/admin/bridge/jobs", tok, body+"}")
}

// TestBridgeDetectQueue: chapters.detect takes no params ({} or none; any
// field is a 400), needs an indexed book (404), a Bridge that runs it (409
// kind_unavailable) and a library it can write to (409 library_not_writable);
// a second press is the open job. It is refused while a generate job is open
// for the book (409 generate_open), and a new generate job is refused while it
// is open (409 detect_open). The feature chapters appears with the kind. The
// route is the admin's: a user is 403, the bridge token 401.
func TestBridgeDetectQueue(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	// Denied: a Bridge that doesn't run the kind (an older one).
	b.beatWith(t, true, true, "")
	if resp, body := b.queueDetect(t, b.adminTok, mistbornBook, `{}`); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(body, `"code":"kind_unavailable"`) {
		t.Fatalf("an older Bridge: %d %s, want 409 kind_unavailable", resp.StatusCode, body)
	}
	b.beatDetect(t, true)
	if _, body := b.do(t, "GET", "/api/v1/server", b.userTok, ""); !strings.Contains(body, `"features":["bridge","chapters","sidecars"]`) {
		t.Fatalf("features = %s", body)
	}

	// Allowed: params absent or {}, stored as {}; a second press is the open job.
	resp, body := b.queueDetect(t, b.adminTok, mistbornBook, "")
	var job bridge.Job
	decodeInto(t, body, &job)
	if resp.StatusCode != http.StatusCreated || job.Kind != bridge.KindDetectChapters || string(job.Params) != `{}` {
		t.Fatalf("queue: %d %s", resp.StatusCode, body)
	}
	if resp, body := b.queueDetect(t, b.adminTok, mistbornBook, `{}`); resp.StatusCode != http.StatusOK || !strings.Contains(body, fmt.Sprintf(`"id":%d,`, job.ID)) {
		t.Fatalf("second press: %d %s, want the open job", resp.StatusCode, body)
	}
	for name, c := range map[string]struct {
		body string
		want int
	}{
		"a param":       {fmt.Sprintf(`{"kind":"chapters.detect","library_id":%d,"path":%q,"params":{"source":"audio"}}`, b.lib.ID, cradleBook), http.StatusBadRequest},
		"not an object": {fmt.Sprintf(`{"kind":"chapters.detect","library_id":%d,"path":%q,"params":[]}`, b.lib.ID, cradleBook), http.StatusBadRequest},
		"no path":       {fmt.Sprintf(`{"kind":"chapters.detect","library_id":%d}`, b.lib.ID), http.StatusBadRequest},
		"no such book":  {fmt.Sprintf(`{"kind":"chapters.detect","library_id":%d,"path":"Nobody/Nothing"}`, b.lib.ID), http.StatusNotFound},
	} {
		if resp, out := b.do(t, "POST", "/api/v1/admin/bridge/jobs", b.adminTok, c.body); resp.StatusCode != c.want {
			t.Errorf("%s: %d %s, want %d", name, resp.StatusCode, out, c.want)
		}
	}
	// Denied: a user, and the bridge token.
	if resp, body := b.queueDetect(t, b.userTok, cradleBook, `{}`); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a user: %d %s, want 403", resp.StatusCode, body)
	}
	if resp, body := b.queueDetect(t, b.bridgeTok, cradleBook, `{}`); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the bridge token: %d %s, want 401", resp.StatusCode, body)
	}

	// Denied: a generate job while the detect job is open.
	if resp, body := b.queueSidecars(t, b.lib.ID, mistbornBook, `{}`); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(body, `"code":"detect_open"`) {
		t.Fatalf("generate with a detect open: %d %s, want 409 detect_open", resp.StatusCode, body)
	}
	// Denied: a detect job while a generate job is open for the book.
	if resp, body := b.queueSidecars(t, b.lib.ID, cradleBook, `{}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("generate: %d %s", resp.StatusCode, body)
	}
	if resp, body := b.queueDetect(t, b.adminTok, cradleBook, `{}`); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(body, `"code":"generate_open"`) {
		t.Fatalf("detect with a generate open: %d %s, want 409 generate_open", resp.StatusCode, body)
	}
	// Allowed: the generate job once the detect job has ended.
	if _, err := b.api.bridge.Cancel(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if resp, body := b.queueSidecars(t, b.lib.ID, mistbornBook, `{}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("generate once the detect ended: %d %s", resp.StatusCode, body)
	}

	// Denied: a library the Bridge can't write to.
	b.beatDetect(t, false)
	if _, err := b.db.ExecContext(context.Background(), `UPDATE bridge_jobs SET state = 'cancelled'`); err != nil {
		t.Fatal(err)
	}
	if resp, body := b.queueDetect(t, b.adminTok, mistbornBook, `{}`); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(body, `"code":"library_not_writable"`) {
		t.Fatalf("a read-only library: %d %s, want 409 library_not_writable", resp.StatusCode, body)
	}
}

// TestGeneratedChaptersOnTheWire: once the Bridge has written a chapterless
// book's chapters.json and asked for the attachments rescan (which answers
// chapters: true), the player's /chapters and item say chapters_source
// "generated" with the found chapters, and the admin's book page does too.
func TestGeneratedChaptersOnTheWire(t *testing.T) {
	t.Parallel()
	b, root := newBridgeEnvCopy(t)
	ctx := context.Background()
	// The fixture's book as a chapterless hour (the test scanner runs without
	// ffprobe, so it reads no length).
	book, err := b.cat.GetBookByPath(ctx, b.lib.ID, mistbornBook)
	if err != nil {
		t.Fatal(err)
	}
	book.Duration, book.Chapters = 3600, nil
	for i := range book.Files {
		book.Files[i].Duration = 3600
	}
	if _, err := b.cat.UpsertBook(ctx, book); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, filepath.FromSlash(mistbornBook), ".audiosilo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	list := fmt.Sprintf(`{"version":1,"generator":"audiosilo-bridge","from":"narration",
		"files":[{"path":%q,"size":%d,"duration_ms":3600000}],
		"chapters":[{"title":"Prologue","start_ms":0,"length_ms":1200000},{"title":"Chapter 1","start_ms":1200000,"length_ms":2400000}]}`,
		strings.TrimPrefix(book.Files[0].RelPath, mistbornBook+"/"), book.Files[0].Size)
	if err := os.WriteFile(filepath.Join(dir, metadata.AttachmentChapters), []byte(list), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body := b.rescan(t, fmt.Sprintf(`{"library_id":%d,"path":%q,"mode":"attachments"}`, b.lib.ID, mistbornBook))
	if want := `"attachments":{"characters":false,"recaps":false,"description":false,"chapters":true,"bridge":false,"readalong":false}`; code != http.StatusOK || !strings.Contains(body, want) {
		t.Fatalf("attachments rescan = %d %s, want %s", code, body, want)
	}
	q := fmt.Sprintf("?path=%s", url.QueryEscape(mistbornBook))
	resp, body := b.do(t, "GET", fmt.Sprintf("/api/v1/libraries/%d/chapters%s", b.lib.ID, q), b.adminTok, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"chapters_source":"generated"`) || !strings.Contains(body, `"title":"Chapter 1"`) {
		t.Fatalf("chapters: %d %s", resp.StatusCode, body)
	}
	resp, body = b.do(t, "GET", fmt.Sprintf("/api/v1/libraries/%d/item%s", b.lib.ID, q), b.adminTok, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"chapters_source":"generated"`) || strings.Contains(body, `"generated":true`) {
		t.Fatalf("item: %d %s", resp.StatusCode, body)
	}
	resp, body = b.do(t, "GET", fmt.Sprintf("/api/v1/admin/libraries/%d/book%s", b.lib.ID, q), b.adminTok, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"chapter_source":"generated"`) ||
		!strings.Contains(body, `"generated_chapters_fit":true`) {
		t.Fatalf("admin book: %d %s", resp.StatusCode, body)
	}
	// A files choice keeps them out, and the page still says they fit (so it
	// can offer automatic again); automatic brings them back.
	adminBook := fmt.Sprintf("/api/v1/admin/libraries/%d/book%s", b.lib.ID, q)
	resp, body = b.do(t, "PATCH", adminBook, b.adminTok, `{"chapter_source":"files"}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"chapter_source":"files"`) ||
		!strings.Contains(body, `"chapter_choice":"files"`) || !strings.Contains(body, `"generated_chapters_fit":true`) {
		t.Fatalf("files chosen: %d %s", resp.StatusCode, body)
	}
	resp, body = b.do(t, "PATCH", adminBook, b.adminTok, `{"chapter_source":"auto"}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"chapter_source":"generated"`) {
		t.Fatalf("back to automatic: %d %s", resp.StatusCode, body)
	}
	// The file going (the Bridge's own delete, then its rescan) takes them away.
	if err := os.Remove(filepath.Join(dir, metadata.AttachmentChapters)); err != nil {
		t.Fatal(err)
	}
	if code, body := b.rescan(t, fmt.Sprintf(`{"library_id":%d,"path":%q,"mode":"attachments"}`, b.lib.ID, mistbornBook)); code != http.StatusOK {
		t.Fatalf("rescan: %d %s", code, body)
	}
	if _, body := b.do(t, "GET", fmt.Sprintf("/api/v1/libraries/%d/chapters%s", b.lib.ID, q), b.adminTok, ""); strings.Contains(body, "chapters_source") {
		t.Fatalf("chapters after the file went: %s", body)
	}
	if _, body := b.do(t, "PATCH", adminBook, b.adminTok, `{"chapter_source":"files"}`); strings.Contains(body, "generated_chapters_fit") {
		t.Fatalf("files chosen with no chapters.json: %s", body)
	}
}

// adminBook is the admin's book page URL for path.
func (b *bridgeEnv) adminBook(path string) string {
	return fmt.Sprintf("/api/v1/admin/libraries/%d/book?path=%s", b.lib.ID, url.QueryEscape(path))
}

// TestBridgeDetectRefusedForChosenChapters: the chapters a detect job finds
// stand in only when no chapter source was chosen, so a book whose admin chose
// the files' (or the community's) is refused (409 chapters_chosen), and admin
// rows carry the choice (chapter_choice) for Health to say so; back to
// automatic, it is queued.
func TestBridgeDetectRefusedForChosenChapters(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	b.beatDetect(t, true)
	for _, source := range []string{"files", "community"} {
		if resp, body := b.do(t, "PATCH", b.adminBook(mistbornBook), b.adminTok, `{"chapter_source":"`+source+`"}`); resp.StatusCode != http.StatusOK {
			t.Fatalf("choose %s: %d %s", source, resp.StatusCode, body)
		}
		// Denied: a chosen source.
		if resp, body := b.queueDetect(t, b.adminTok, mistbornBook, `{}`); resp.StatusCode != http.StatusConflict ||
			!strings.Contains(body, `"code":"chapters_chosen"`) {
			t.Fatalf("detect with %s chosen: %d %s, want 409 chapters_chosen", source, resp.StatusCode, body)
		}
		_, body := b.do(t, "GET", fmt.Sprintf("/api/v1/admin/books?library_id=%d", b.lib.ID), b.adminTok, "")
		if !strings.Contains(body, `"chapter_choice":"`+source+`"`) {
			t.Fatalf("admin rows with %s chosen: %s", source, body)
		}
	}
	if resp, body := b.do(t, "PATCH", b.adminBook(mistbornBook), b.adminTok, `{"chapter_source":"auto"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("back to automatic: %d %s", resp.StatusCode, body)
	}
	if _, body := b.do(t, "GET", fmt.Sprintf("/api/v1/admin/books?library_id=%d", b.lib.ID), b.adminTok, ""); strings.Contains(body, `"chapter_choice"`) {
		t.Fatalf("admin rows with no choice carry one: %s", body)
	}
	// Allowed: no choice.
	if resp, body := b.queueDetect(t, b.adminTok, mistbornBook, `{}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("detect with no choice: %d %s, want 201", resp.StatusCode, body)
	}
}

// TestBridgeDescribeAndDetectExclude: a describe job is refused while a detect
// job is open for the book (409 detect_open) and a detect job while a describe
// job is (409 describe_open): both rewrite its bridge.json on the Bridge. The
// estimate's open_job names a queued detect job, which refuses a generate.
func TestBridgeDescribeAndDetectExclude(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	body := fmt.Sprintf(`{"version":"0.18.0","kinds":["bridge.verify","sidecars.generate","sidecars.describe","chapters.detect"],
		"libraries":[{"library_id":%d,"mount_path":"/media/books","mapped":true,"writable":true}]}`, b.lib.ID)
	if resp, out := b.do(t, "PUT", "/api/v1/bridge/status", b.bridgeTok, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", resp.StatusCode, out)
	}
	// The estimate names no open job, then the detect job once queued.
	if est := b.briefEstimate(t, b.lib.ID, mistbornBook); est.OpenJob != nil {
		t.Fatalf("open_job before = %+v", est.OpenJob)
	}
	resp, out := b.queueDetect(t, b.adminTok, mistbornBook, `{}`)
	var det bridge.Job
	decodeInto(t, out, &det)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("detect: %d %s", resp.StatusCode, out)
	}
	if est := b.briefEstimate(t, b.lib.ID, mistbornBook); est.OpenJob == nil || est.OpenJob.ID != det.ID || est.OpenJob.Kind != bridge.KindDetectChapters {
		t.Fatalf("open_job with a detect queued = %+v", est.OpenJob)
	}
	// Denied: describe while detect is open.
	if resp, out := b.queueDescribe(t, b.adminTok, mistbornBook, `{}`); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(out, `"code":"detect_open"`) {
		t.Fatalf("describe with a detect open: %d %s, want 409 detect_open", resp.StatusCode, out)
	}
	// Allowed: describe for another book; denied: detect while it is open.
	if resp, out := b.queueDescribe(t, b.adminTok, cradleBook, `{}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("describe: %d %s", resp.StatusCode, out)
	}
	if resp, out := b.queueDetect(t, b.adminTok, cradleBook, `{}`); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(out, `"code":"describe_open"`) {
		t.Fatalf("detect with a describe open: %d %s, want 409 describe_open", resp.StatusCode, out)
	}
	// Allowed: describe once the detect job ended.
	if _, err := b.api.bridge.Cancel(context.Background(), det.ID); err != nil {
		t.Fatal(err)
	}
	if est := b.briefEstimate(t, b.lib.ID, mistbornBook); est.OpenJob != nil {
		t.Fatalf("open_job after = %+v", est.OpenJob)
	}
	if resp, out := b.queueDescribe(t, b.adminTok, mistbornBook, `{}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("describe once the detect ended: %d %s", resp.StatusCode, out)
	}
}

// TestBridgeDetectParkGivesWayToGenerate: a parked detect job (no chapters
// heard) refuses no generate job: the generate job is queued and cancels it,
// and the estimate's open_job no longer names it.
func TestBridgeDetectParkGivesWayToGenerate(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	b.beatDetect(t, true)
	resp, out := b.queueDetect(t, b.adminTok, mistbornBook, `{}`)
	var det bridge.Job
	decodeInto(t, out, &det)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("detect: %d %s", resp.StatusCode, out)
	}
	if resp, out := b.do(t, "GET", "/api/v1/bridge/jobs/next", b.bridgeTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("claim: %d %s", resp.StatusCode, out)
	}
	if resp, out := b.do(t, "PATCH", fmt.Sprintf("/api/v1/bridge/jobs/%d", det.ID), b.bridgeTok,
		`{"attempt":1,"state":"needs_attention","error":"no headings","progress":{"park":{"code":"chapters_not_detected"}}}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("park: %d %s", resp.StatusCode, out)
	}
	if est := b.briefEstimate(t, b.lib.ID, mistbornBook); est.OpenJob != nil {
		t.Fatalf("open_job with a parked detect = %+v", est.OpenJob)
	}
	if resp, out := b.queueSidecars(t, b.lib.ID, mistbornBook, `{}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("generate with a parked detect: %d %s, want 201", resp.StatusCode, out)
	}
	if j, err := b.api.bridge.Job(context.Background(), det.ID); err != nil || j.State != bridge.StateCancelled {
		t.Fatalf("the parked detect = %+v, %v; want cancelled", j, err)
	}
}

// TestBridgeDetectFinishedSaysWhereTheChaptersGo: a finished detect job's
// event carries the book's chapter source now, so the message promises the
// chapters to listeners only when they are the book's (generated), and says
// they were saved but unused otherwise.
func TestBridgeDetectFinishedSaysWhereTheChaptersGo(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	b.beatDetect(t, true)
	ctx := context.Background()
	resp, out := b.queueDetect(t, b.adminTok, mistbornBook, `{}`)
	var det bridge.Job
	decodeInto(t, out, &det)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("detect: %d %s", resp.StatusCode, out)
	}
	if resp, out := b.do(t, "GET", "/api/v1/bridge/jobs/next", b.bridgeTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("claim: %d %s", resp.StatusCode, out)
	}
	// The book's chapters stay its files' (no chapters.json was read).
	if resp, out := b.do(t, "PATCH", fmt.Sprintf("/api/v1/bridge/jobs/%d", det.ID), b.bridgeTok,
		`{"attempt":1,"state":"done","result":{"chapters":12}}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("done: %d %s", resp.StatusCode, out)
	}
	evs, _, err := b.cat.ListServerEvents(ctx, 0, 10, notify.KindBridgeJobFinished)
	if err != nil || len(evs) != 1 {
		t.Fatalf("events = %v, %v", evs, err)
	}
	if got := evs[0].Data["chapters_source"]; got != catalog.ChaptersFromFiles {
		t.Fatalf("chapters_source = %v, want files", got)
	}
}
