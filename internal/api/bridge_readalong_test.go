package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// beatReadalong sends a heartbeat offering verify, generate, detect and
// read-along (or without read-along), the env's library mapped and writable as
// given, with the sidecars estimates.
func (b *bridgeEnv) beatReadalong(t *testing.T, writable, readalong bool) {
	t.Helper()
	kinds := `"bridge.verify","sidecars.generate","chapters.detect"`
	if readalong {
		kinds += `,"readalong.align"`
	}
	body := fmt.Sprintf(`{"version":"0.5.0","kinds":[%s],
		"libraries":[{"library_id":%d,"mount_path":"/media/books","mapped":true,"writable":%t}],"estimates":%s}`,
		kinds, b.lib.ID, writable, sidecarsEstimates)
	if resp, out := b.do(t, "PUT", "/api/v1/bridge/status", b.bridgeTok, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", resp.StatusCode, out)
	}
}

func (b *bridgeEnv) queueReadalong(t *testing.T, path, params string) (*http.Response, string) {
	t.Helper()
	body := fmt.Sprintf(`{"kind":"readalong.align","library_id":%d,"path":%q`, b.lib.ID, path)
	if params != "" {
		body += `,"params":` + params
	}
	return b.do(t, "POST", "/api/v1/admin/bridge/jobs", b.adminTok, body+"}")
}

// newReadalongBridgeEnv is the Bridge env over a copy of the fixtures with an
// epub beside Mistborn's audio (Cradle has none), scanned, the files given a
// length, and a Bridge that runs read-along.
func newReadalongBridgeEnv(t *testing.T) (*bridgeEnv, string, []byte) {
	t.Helper()
	b, root := newBridgeEnvCopy(t)
	epub := testEpub(t, false)
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(mistbornBook), "Mistborn.epub"), epub, 0o644); err != nil {
		t.Fatal(err)
	}
	scanFixture(t, b.testEnv, b.lib)
	setTestDurations(t, b.testEnv)
	b.beatReadalong(t, true, true)
	return b, root, epub
}

// TestBridgeReadalongQueue: readalong.align takes no params, needs a book with
// an ebook (409 no_ebook), a Bridge that runs it (409 kind_unavailable) and a
// library it can write to (409 library_not_writable); it and a generate job
// refuse each other (409 readalong_open, generate_open), as it and a detect job
// do. The feature readalong appears with the kind.
func TestBridgeReadalongQueue(t *testing.T) {
	t.Parallel()
	b, _, _ := newReadalongBridgeEnv(t)
	// Denied: no ebook.
	if resp, body := b.queueReadalong(t, cradleBook, ""); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(body, `"code":"no_ebook"`) {
		t.Fatalf("a book with no ebook: %d %s, want 409 no_ebook", resp.StatusCode, body)
	}
	// Denied: a Bridge that doesn't run it; a library it can't write to.
	b.beatReadalong(t, true, false)
	if resp, body := b.queueReadalong(t, mistbornBook, ""); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(body, `"code":"kind_unavailable"`) {
		t.Fatalf("no kind: %d %s, want 409 kind_unavailable", resp.StatusCode, body)
	}
	b.beatReadalong(t, false, true)
	if resp, body := b.queueReadalong(t, mistbornBook, ""); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(body, `"code":"library_not_writable"`) {
		t.Fatalf("read-only: %d %s, want 409 library_not_writable", resp.StatusCode, body)
	}
	b.beatReadalong(t, true, true)
	if resp, body := b.queueReadalong(t, mistbornBook, `{"source":"ebook"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a param: %d %s, want 400", resp.StatusCode, body)
	}
	// Allowed: queued once, a second press is the open job.
	resp, body := b.queueReadalong(t, mistbornBook, `{}`)
	var job bridge.Job
	decodeInto(t, body, &job)
	if resp.StatusCode != http.StatusCreated || job.Kind != bridge.KindReadalong || *job.Path != mistbornBook || string(job.Params) != `{}` {
		t.Fatalf("queue: %d %s", resp.StatusCode, body)
	}
	if resp, body := b.queueReadalong(t, mistbornBook, ""); resp.StatusCode != http.StatusOK || !strings.Contains(body, fmt.Sprintf(`"id":%d,`, job.ID)) {
		t.Fatalf("second press: %d %s", resp.StatusCode, body)
	}
	// Denied both ways: generate and detect behind it.
	if resp, body := b.queueSidecars(t, b.lib.ID, mistbornBook, `{}`); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(body, `"code":"readalong_open"`) {
		t.Fatalf("generate with a read-along open: %d %s, want 409 readalong_open", resp.StatusCode, body)
	}
	if resp, body := b.queueDetect(t, b.adminTok, mistbornBook, ""); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(body, `"code":"readalong_open"`) {
		t.Fatalf("detect with a read-along open: %d %s, want 409 readalong_open", resp.StatusCode, body)
	}
	if resp, body := b.do(t, "POST", fmt.Sprintf("/api/v1/admin/bridge/jobs/%d/cancel", job.ID), b.adminTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel: %d %s", resp.StatusCode, body)
	}
	if resp, body := b.queueSidecars(t, b.lib.ID, mistbornBook, `{}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("generate once the read-along ended: %d %s", resp.StatusCode, body)
	}
	if resp, body := b.queueReadalong(t, mistbornBook, ""); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(body, `"code":"generate_open"`) {
		t.Fatalf("read-along with a generate open: %d %s, want 409 generate_open", resp.StatusCode, body)
	}
	// Denied: a user.
	_, userTok := newUserWithSession(t, b.testEnv, "sam", false)
	if resp, _ := b.do(t, "POST", "/api/v1/admin/bridge/jobs", userTok,
		fmt.Sprintf(`{"kind":"readalong.align","library_id":%d,"path":%q}`, b.lib.ID, mistbornBook)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a user: %d, want 403", resp.StatusCode)
	}
	_, body = b.do(t, "GET", "/api/v1/server", b.adminTok, "")
	if !strings.Contains(body, `"readalong"`) || !strings.Contains(body, `"features":["bridge","chapters","readalong","sidecars"]`) {
		t.Errorf("server = %s", body)
	}
}

// TestBridgeReadalongEstimate: the read-along estimate quotes the book's whole
// audio at the Bridge's ASR figure, says whether it can write, names the ebook
// (null for none) and the book's read-along now (null for none), and the open
// job a new one would be or wait for.
func TestBridgeReadalongEstimate(t *testing.T) {
	t.Parallel()
	b, _, epub := newReadalongBridgeEnv(t)
	estimate := func(path string) map[string]json.RawMessage {
		t.Helper()
		resp, body := b.do(t, "GET", fmt.Sprintf("/api/v1/admin/bridge/estimate?kind=readalong.align&library_id=%d&path=%s",
			b.lib.ID, url.QueryEscape(path)), b.adminTok, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("estimate %s: %d %s", path, resp.StatusCode, body)
		}
		var out map[string]json.RawMessage
		decodeInto(t, body, &out)
		return out
	}
	got := estimate(mistbornBook)
	// 3600.25 s of audio at 900 s per audio hour: 0.25 h.
	want := map[string]string{"kind": `"readalong.align"`, "asr_hours": `0.25`, "figures": `true`, "writable": `true`,
		"ebook": fmt.Sprintf(`{"path":%q,"size":%d}`, mistbornBook+"/Mistborn.epub", len(epub)), "readalong": `null`}
	for k, v := range want {
		if string(got[k]) != v {
			t.Errorf("%s = %s, want %s", k, got[k], v)
		}
	}
	if _, has := got["open_job"]; has || len(got) != len(want) {
		t.Errorf("estimate = %v", got)
	}
	if got := estimate(cradleBook); string(got["ebook"]) != `null` {
		t.Errorf("no ebook: %s", got["ebook"])
	}
	// The book's open job, else the one it would wait for.
	if resp, body := b.queueSidecars(t, b.lib.ID, cradleBook, `{}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("generate: %d %s", resp.StatusCode, body)
	}
	if got := estimate(cradleBook); !strings.Contains(string(got["open_job"]), `"kind":"sidecars.generate"`) {
		t.Errorf("open_job behind a generate = %s", got["open_job"])
	}
	if resp, body := b.queueReadalong(t, mistbornBook, ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("read-along: %d %s", resp.StatusCode, body)
	}
	if got := estimate(mistbornBook); !strings.Contains(string(got["open_job"]), `"kind":"readalong.align"`) {
		t.Errorf("open_job = %s", got["open_job"])
	}
	// Denied: a Bridge that doesn't run it.
	b.beatReadalong(t, true, false)
	resp, body := b.do(t, "GET", fmt.Sprintf("/api/v1/admin/bridge/estimate?kind=readalong.align&library_id=%d&path=%s",
		b.lib.ID, url.QueryEscape(mistbornBook)), b.adminTok, "")
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "kind_unavailable") {
		t.Errorf("no kind: %d %s", resp.StatusCode, body)
	}
}

// The Bridge's book route carries each file's indexed length and the book's
// ebook as the index holds it (omitted when none).
func TestBridgeBookReadalongFacts(t *testing.T) {
	t.Parallel()
	b, _, epub := newReadalongBridgeEnv(t)
	book := func(path string) (files []bridgeFile, ebook *catalog.Ebook, raw string) {
		t.Helper()
		resp, body := b.do(t, "GET", fmt.Sprintf("/api/v1/bridge/libraries/%d/book?path=%s", b.lib.ID, url.QueryEscape(path)), b.bridgeTok, "")
		var out struct {
			Files []bridgeFile
			Book  struct {
				Ebook *catalog.Ebook `json:"ebook"`
			} `json:"book"`
		}
		decodeInto(t, body, &out)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("book %s: %d %s", path, resp.StatusCode, body)
		}
		return out.Files, out.Book.Ebook, body
	}
	files, ebook, _ := book(mistbornBook)
	if len(files) != 1 || files[0].Duration != 1800.125 {
		t.Errorf("files = %+v, want the indexed length", files)
	}
	if ebook == nil || ebook.Path != mistbornBook+"/Mistborn.epub" || ebook.Size != int64(len(epub)) {
		t.Errorf("ebook = %+v", ebook)
	}
	if _, ebook, raw := book(cradleBook); ebook != nil || strings.Contains(raw, `"ebook"`) {
		t.Errorf("no ebook: %s", raw)
	}
}

// A finished read-along job is announced with its coverage.
func TestBridgeReadalongNotified(t *testing.T) {
	t.Parallel()
	b, _, _ := newReadalongBridgeEnv(t)
	if resp, body := b.queueReadalong(t, mistbornBook, ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("queue: %d %s", resp.StatusCode, body)
	}
	resp, body := b.do(t, "GET", "/api/v1/bridge/jobs/next?kind=readalong.align", b.bridgeTok, "")
	var job bridge.Job
	decodeInto(t, body, &job)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim: %d %s", resp.StatusCode, body)
	}
	result := fmt.Sprintf(`{"attempt":%d,"state":"done","result":{"library_id":%d,"path":%q,"file":"readalong.json",
		"coverage":{"text":0.873,"audio":0.9},"sections":3,"spans":40,"audio_gaps":0,"heal_windows":1,"transcribed":false,
		"ebook_sha256":"%s"}}`, job.Attempts, b.lib.ID, mistbornBook, strings.Repeat("a", 64))
	if resp, body := b.do(t, "PATCH", fmt.Sprintf("/api/v1/bridge/jobs/%d", job.ID), b.bridgeTok, result); resp.StatusCode != http.StatusOK {
		t.Fatalf("done: %d %s", resp.StatusCode, body)
	}
	events, _, err := b.cat.ListServerEvents(context.Background(), 0, 10, "bridge_job_finished")
	if err != nil || len(events) != 1 || events[0].Data["coverage"] != 0.873 {
		t.Fatalf("events = %+v, %v", events, err)
	}
}
