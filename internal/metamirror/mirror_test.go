package metamirror

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/query"
	"github.com/kodestar/audiosilo-meta/pkg/query/querytest"
	"github.com/kodestar/audiosilo-meta/pkg/release"
	_ "modernc.org/sqlite"
)

// artifact is one data release's assets: the gzipped artifact and its digest.
type artifact struct {
	gz     []byte
	digest string // the sha256sum line, as release.yml writes it
}

// fixtureArtifact gzips the querytest artifact; schema, when non-zero, first
// rewrites the copy's meta(schema_version) (an artifact newer than this code).
func fixtureArtifact(t *testing.T, schema int) artifact {
	t.Helper()
	path := querytest.Build(t, t.TempDir())
	if schema != 0 {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE meta SET value=? WHERE key='schema_version'`, schema); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return gzArtifact(t, raw)
}

func gzArtifact(t *testing.T, raw []byte) artifact {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return artifact{gz: buf.Bytes(), digest: hex.EncodeToString(sum[:]) + "  meta.sqlite.gz\n"}
}

// fakeRelease is one release on the fake: a data release when art is set, a
// code release (no data assets, like the repo's v* releases) otherwise.
type fakeRelease struct {
	tag       string
	published time.Time
	art       *artifact
}

// fakeGitHub serves the releases list (with ETag/304) and the data assets. It
// counts the full list answers, the 304s and the artifact downloads.
type fakeGitHub struct {
	srv *httptest.Server

	mu        sync.Mutex
	rels      []fakeRelease
	etag      string
	lists     int
	notMod    int
	downloads int
	agents    []string
	// hold, when set, stops each artifact download halfway until it is closed.
	hold chan struct{}
}

func newFakeGitHub(t *testing.T, rels ...fakeRelease) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{}
	f.publish(rels...)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/"+release.DefaultRepo+"/releases", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.agents = append(f.agents, r.Header.Get("User-Agent"))
		if inm := r.Header.Get("If-None-Match"); inm != "" && inm == f.etag {
			f.notMod++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		f.lists++
		type asset struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
			URL  string `json:"browser_download_url"`
		}
		type rel struct {
			Tag       string    `json:"tag_name"`
			Published time.Time `json:"published_at"`
			Assets    []asset   `json:"assets"`
		}
		list := []rel{}
		for _, fr := range f.rels {
			r := rel{Tag: fr.tag, Published: fr.published, Assets: []asset{}}
			if fr.art != nil {
				base := f.srv.URL + "/dl/" + fr.tag + "/"
				r.Assets = append(r.Assets,
					asset{Name: release.DataAsset, Size: int64(len(fr.art.gz)), URL: base + release.DataAsset},
					asset{Name: release.DataDigestAsset, Size: int64(len(fr.art.digest)), URL: base + release.DataDigestAsset})
			}
			list = append(list, r)
		}
		w.Header().Set("ETag", f.etag)
		_ = json.NewEncoder(w).Encode(list)
	})
	mux.HandleFunc("GET /dl/{tag}/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		var art *artifact
		for _, fr := range f.rels {
			if fr.tag == r.PathValue("tag") {
				art = fr.art
			}
		}
		if art != nil && r.PathValue("name") == release.DataAsset {
			f.downloads++
		}
		hold := f.hold
		f.mu.Unlock()
		switch {
		case art == nil:
			http.NotFound(w, r)
		case r.PathValue("name") == release.DataAsset && hold != nil:
			half := len(art.gz) / 2
			_, _ = w.Write(art.gz[:half])
			w.(http.Flusher).Flush()
			<-hold
			_, _ = w.Write(art.gz[half:])
		case r.PathValue("name") == release.DataAsset:
			_, _ = w.Write(art.gz)
		default:
			_, _ = io.WriteString(w, art.digest)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// publish replaces the release list (a new ETag, so the next request is a 200).
func (f *fakeGitHub) publish(rels ...fakeRelease) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rels = rels
	tags := make([]string, len(rels))
	for i, r := range rels {
		tags[i] = r.tag
	}
	f.etag = `"` + strings.Join(tags, "+") + `"`
}

func (f *fakeGitHub) counts() (lists, notMod, downloads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists, f.notMod, f.downloads
}

// releases is a realistic list: a data release between code releases (v*),
// listed out of publish order, the newest data release not first.
func releases(art artifact, tags ...string) []fakeRelease {
	t0 := time.Date(2026, 10, 9, 6, 0, 0, 0, time.UTC)
	out := []fakeRelease{{tag: "v0.21.0", published: t0.Add(5 * time.Hour)}}
	for i, tag := range tags {
		out = append(out, fakeRelease{tag: tag, published: t0.Add(time.Duration(i) * time.Hour), art: &art})
	}
	return append(out, fakeRelease{tag: "v0.20.0", published: t0.Add(-time.Hour)})
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// lots of room: the real reading would make the tests depend on the machine.
func roomy(string) (uint64, error) { return 1 << 40, nil }

func (f *fakeGitHub) client() *release.Client {
	return release.New("", "", release.WithAPIBase(f.srv.URL), release.WithUserAgent("AudioSilo/test"))
}

func newMirror(t *testing.T, dir string, gh *fakeGitHub, opts Options) *Mirror {
	t.Helper()
	if opts.FreeBytes == nil {
		opts.FreeBytes = roomy
	}
	if opts.Logger == nil {
		opts.Logger = quiet()
	}
	m, err := New(dir, gh.client(), opts)
	if err != nil {
		t.Fatal(err)
	}
	m.grace = 10 * time.Millisecond
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// copies lists the copies in the mirror's folder.
func copies(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "meta-*.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range matches {
		matches[i] = filepath.Base(matches[i])
	}
	return matches
}

// eventually polls cond for up to two seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMirrorDownloadsAndSwaps(t *testing.T) {
	art := fixtureArtifact(t, 0)
	gh := newFakeGitHub(t, releases(art, "data-v2026.10.08-aaaaaaa-bbbbbbb", "data-v2026.10.09-ccccccc-ddddddd")...)
	dir := filepath.Join(t.TempDir(), DirName)
	m := newMirror(t, dir, gh, Options{})

	st := m.Status()
	if st.State != StateEmpty || !st.Fallback || m.Ready() || st.NextCheckAt.IsZero() {
		t.Fatalf("a new mirror = %+v", st)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("the folder must be created 0700: %v %v", fi.Mode(), err)
	}

	m.check(context.Background())
	st = m.Status()
	const tag = "data-v2026.10.09-ccccccc-ddddddd" // the newest data release, not the first listed
	if st.State != StateReady || st.Fallback || st.Tag != tag || st.Error != "" {
		t.Fatalf("after a download = %+v", st)
	}
	if st.SchemaVersion != query.MaxSchemaVersion || st.SchemaNewer || st.SizeBytes == 0 || st.BuiltAt.IsZero() ||
		st.DownloadedAt.IsZero() || st.CheckedAt.IsZero() || st.Progress != nil {
		t.Fatalf("the copy's facts = %+v", st)
	}
	if got := st.NextCheckAt.Sub(st.CheckedAt); got != checkInterval {
		t.Fatalf("the next check is %v after this one, want a day", got)
	}
	if got := copies(t, dir); len(got) != 1 || got[0] != "meta-"+tag+".sqlite" {
		t.Fatalf("copies = %v", got)
	}
	if fi, err := os.Stat(filepath.Join(dir, "meta-"+tag+".sqlite")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the copy must be owner-only: %v %v", fi.Mode(), err)
	}
	if m.SizeBytes() != st.SizeBytes {
		t.Fatalf("SizeBytes = %d, want %d", m.SizeBytes(), st.SizeBytes)
	}
	gh.mu.Lock()
	ua := gh.agents[0]
	gh.mu.Unlock()
	if ua != "AudioSilo/test" {
		t.Fatalf("User-Agent = %q", ua)
	}

	// The copy answers metaserve's API.
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/lookup?asin="+querytest.ASIN, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), querytest.ASINWork) {
		t.Fatalf("lookup = %d %s", rec.Code, rec.Body)
	}

	// A newer data release: downloaded, swapped in, and the old copy closed and
	// deleted after the grace.
	old := m.Current()
	const next = "data-v2026.10.10-eeeeeee-fffffff"
	gh.publish(releases(art, tag, next)...)
	m.check(context.Background())
	if st := m.Status(); st.State != StateReady || st.Tag != next || m.Current() == old {
		t.Fatalf("after an update = %+v", st)
	}
	eventually(t, "the old copy's deletion", func() bool { return len(copies(t, dir)) == 1 })
	if got := copies(t, dir); got[0] != "meta-"+next+".sqlite" {
		t.Fatalf("copies = %v", got)
	}
	if _, _, downloads := gh.counts(); downloads != 2 {
		t.Fatalf("downloads = %d, want 2", downloads)
	}
}

// While a download runs the status says so, with its progress in compressed
// bytes, and lookups go to the remote service until the copy is ready.
func TestMirrorProgress(t *testing.T) {
	art := fixtureArtifact(t, 0)
	gh := newFakeGitHub(t, releases(art, "data-v2026.10.09-ccccccc-ddddddd")...)
	gh.hold = make(chan struct{})
	m := newMirror(t, t.TempDir(), gh, Options{})
	done := make(chan struct{})
	go func() { m.check(context.Background()); close(done) }()
	eventually(t, "the download's progress", func() bool {
		st := m.Status()
		return st.State == StateDownloading && st.Progress != nil && st.Progress.Done > 0
	})
	st := m.Status()
	if !st.Fallback || st.Progress.Total != int64(len(art.gz)) || st.Progress.Done >= st.Progress.Total {
		t.Fatalf("while downloading = %+v (%+v)", st, st.Progress)
	}
	m.CheckNow() // a check is running: this one is dropped, not queued
	close(gh.hold)
	<-done
	if st := m.Status(); st.State != StateReady || st.Progress != nil {
		t.Fatalf("after the download = %+v", st)
	}
	select {
	case <-m.wake:
		t.Fatal("Check now during a check must not queue another")
	default:
	}
}

// An unchanged release list is a 304 and downloads nothing; the same release
// listed again (a new ETag) downloads nothing either.
func TestMirrorNotModified(t *testing.T) {
	art := fixtureArtifact(t, 0)
	rels := releases(art, "data-v2026.10.09-ccccccc-ddddddd")
	gh := newFakeGitHub(t, rels...)
	m := newMirror(t, t.TempDir(), gh, Options{})
	m.check(context.Background())
	m.check(context.Background())
	if lists, notMod, downloads := gh.counts(); lists != 1 || notMod != 1 || downloads != 1 {
		t.Fatalf("lists %d, 304s %d, downloads %d; want 1, 1, 1", lists, notMod, downloads)
	}
	gh.publish(append(rels, fakeRelease{tag: "v0.22.0"})...)
	m.check(context.Background())
	if _, _, downloads := gh.counts(); downloads != 1 {
		t.Fatalf("the same data release must not download again, downloads = %d", downloads)
	}
	if st := m.Status(); st.State != StateReady || st.Error != "" {
		t.Fatalf("status = %+v", st)
	}
}

// A download that fails its digest keeps the copy, records the failure, drops
// the ETag (so the next check lists again) and leaves no temp file; without a
// copy it is the error state, and lookups keep going to the remote service.
func TestMirrorBadDigestKeepsCopy(t *testing.T) {
	good := fixtureArtifact(t, 0)
	bad := good
	bad.digest = strings.Repeat("0", 64) + "  meta.sqlite.gz\n"

	dir := t.TempDir()
	gh := newFakeGitHub(t, releases(bad, "data-v2026.10.09-ccccccc-ddddddd")...)
	m := newMirror(t, dir, gh, Options{})
	m.check(context.Background())
	st := m.Status()
	if st.State != StateError || !st.Fallback || st.Error == "" || m.Ready() {
		t.Fatalf("a failed first download = %+v", st)
	}
	if got := st.NextCheckAt.Sub(st.CheckedAt); got != retryInterval {
		t.Fatalf("a failed check retries after %v, want an hour", got)
	}

	gh.publish(releases(good, "data-v2026.10.09-ccccccc-ddddddd")...)
	m.check(context.Background())
	if st := m.Status(); st.State != StateReady || st.Error != "" {
		t.Fatalf("after a good download = %+v", st)
	}
	gh.publish(releases(bad, "data-v2026.10.10-eeeeeee-fffffff")...)
	m.check(context.Background())
	st = m.Status()
	if st.State != StateReady || st.Tag != "data-v2026.10.09-ccccccc-ddddddd" || st.Error == "" || st.Fallback {
		t.Fatalf("a failed update over a copy = %+v", st)
	}
	if m.st.ETag != "" {
		t.Fatalf("a failed download must drop the ETag, got %q", m.st.ETag)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if release.TempFile(e.Name()) {
			t.Fatalf("a temp file was left: %s", e.Name())
		}
	}
	if got := copies(t, dir); len(got) != 1 {
		t.Fatalf("copies = %v", got)
	}
}

// A downloaded file that is not an artifact (right digest, wrong content) is
// deleted, and the failure recorded.
func TestMirrorCopyThatDoesNotOpen(t *testing.T) {
	gh := newFakeGitHub(t, releases(gzArtifact(t, []byte("not a database")), "data-v2026.10.09-ccccccc-ddddddd")...)
	dir := t.TempDir()
	m := newMirror(t, dir, gh, Options{})
	m.check(context.Background())
	if st := m.Status(); st.State != StateError || !strings.Contains(st.Error, "doesn't open") {
		t.Fatalf("status = %+v", st)
	}
	if got := copies(t, dir); len(got) != 0 {
		t.Fatalf("a copy that doesn't open must be deleted, copies = %v", got)
	}
}

// An artifact schema newer than this code still opens and answers, flagged.
func TestMirrorSchemaNewer(t *testing.T) {
	gh := newFakeGitHub(t, releases(fixtureArtifact(t, query.MaxSchemaVersion+1), "data-v2026.10.09-ccccccc-ddddddd")...)
	m := newMirror(t, t.TempDir(), gh, Options{})
	m.check(context.Background())
	st := m.Status()
	if st.State != StateReady || !st.SchemaNewer || st.SchemaVersion != query.MaxSchemaVersion+1 {
		t.Fatalf("status = %+v", st)
	}
	b, _ := json.Marshal(st)
	if !strings.Contains(string(b), `"schema_newer":true`) {
		t.Fatalf("wire = %s", b)
	}
}

// The disk guard refuses a download the volume has no room for, before any
// byte is fetched.
func TestMirrorDiskGuard(t *testing.T) {
	gh := newFakeGitHub(t, releases(fixtureArtifact(t, 0), "data-v2026.10.09-ccccccc-ddddddd")...)
	m := newMirror(t, t.TempDir(), gh, Options{FreeBytes: func(string) (uint64, error) { return 1 << 20, nil }})
	m.check(context.Background())
	st := m.Status()
	if st.State != StateError || !strings.HasPrefix(st.Error, "not enough disk space: need ") || !strings.HasSuffix(st.Error, "have 1.0 MB") {
		t.Fatalf("status = %+v", st)
	}
	if _, _, downloads := gh.counts(); downloads != 0 {
		t.Fatalf("downloads = %d, want none", downloads)
	}
}

// The state outlives a restart: the copy opens at once, nothing is fetched, and
// the next check is a day after the last one, not shortly after start.
func TestMirrorStateSurvivesRestart(t *testing.T) {
	gh := newFakeGitHub(t, releases(fixtureArtifact(t, 0), "data-v2026.10.09-ccccccc-ddddddd")...)
	dir := t.TempDir()
	m := newMirror(t, dir, gh, Options{})
	m.check(context.Background())
	tag := m.Status().Tag
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	m2 := newMirror(t, dir, gh, Options{})
	st := m2.Status()
	if st.State != StateReady || st.Tag != tag || !m2.Ready() || st.DownloadedAt.IsZero() {
		t.Fatalf("after a restart = %+v", st)
	}
	if due := m2.untilDue(); due < checkInterval-time.Minute {
		t.Fatalf("a restart must not check again within the day, due in %v", due)
	}
	if lists, notMod, downloads := gh.counts(); lists != 1 || notMod != 0 || downloads != 1 {
		t.Fatalf("lists %d, 304s %d, downloads %d", lists, notMod, downloads)
	}
	// The check it does run is conditional on the stored ETag.
	m2.check(context.Background())
	if _, notMod, _ := gh.counts(); notMod != 1 {
		t.Fatalf("the restarted mirror's check must be a 304, got %d", notMod)
	}

	// A copy whose file is gone is forgotten: the next check is soon, and lists.
	if err := m2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "meta-"+tag+".sqlite")); err != nil {
		t.Fatal(err)
	}
	m3 := newMirror(t, dir, gh, Options{})
	if st := m3.Status(); st.State != StateEmpty || m3.Ready() || m3.st.ETag != "" {
		t.Fatalf("a missing copy = %+v (etag %q)", st, m3.st.ETag)
	}
	if due := m3.untilDue(); due > startDelay {
		t.Fatalf("without a copy the first check is soon, due in %v", due)
	}
}

// Leftovers of a stopped process are deleted at start: download temp files, a
// half-written state, a copy that isn't the current one.
func TestMirrorCleansLeftovers(t *testing.T) {
	gh := newFakeGitHub(t, releases(fixtureArtifact(t, 0), "data-v2026.10.09-ccccccc-ddddddd")...)
	dir := t.TempDir()
	m := newMirror(t, dir, gh, Options{})
	m.check(context.Background())
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	leftovers := []string{".meta-123.tmp", stateTemp, "meta-data-v2026.10.01-0000000-0000000.sqlite"}
	for _, name := range leftovers {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m2 := newMirror(t, dir, gh, Options{})
	if !m2.Ready() {
		t.Fatal("the current copy must survive the sweep")
	}
	for _, name := range leftovers {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s must be deleted, got %v", name, err)
		}
	}
}

// Run checks shortly after start without a copy and when woken; it checks
// nothing while metadata is off.
func TestMirrorRun(t *testing.T) {
	gh := newFakeGitHub(t, releases(fixtureArtifact(t, 0), "data-v2026.10.09-ccccccc-ddddddd")...)

	var mu sync.Mutex
	on := false
	enabled := func() bool { mu.Lock(); defer mu.Unlock(); return on }
	m := newMirror(t, t.TempDir(), gh, Options{Enabled: enabled})
	m.startAfter = 0
	m.first = time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	m.CheckNow()
	time.Sleep(50 * time.Millisecond)
	if lists, notMod, _ := gh.counts(); lists+notMod != 0 || m.Ready() {
		t.Fatal("nothing may be checked while metadata is off")
	}

	mu.Lock()
	on = true
	mu.Unlock()
	m.CheckNow()
	eventually(t, "the first download", m.Ready)
	m.CheckNow()
	eventually(t, "a check on request", func() bool { _, notMod, _ := gh.counts(); return notMod == 1 })
}

// The status on the wire, as the console reads it: no empty facts, fallback
// always present.
func TestStatusJSON(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	b, err := json.Marshal(Status{State: StateEmpty, NextCheckAt: now, Fallback: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"state":"empty","next_check_at":"2026-10-09T12:00:00Z","fallback":true}`; string(b) != want {
		t.Fatalf("wire = %s, want %s", b, want)
	}
	b, _ = json.Marshal(Status{State: StateDownloading, Progress: &Progress{Done: 5, Total: 0}})
	if want := `{"state":"downloading","progress":{"done":5,"total":0},"fallback":false}`; string(b) != want {
		t.Fatalf("wire = %s, want %s", b, want)
	}
}

// A tag that can't name a file is refused before anything is downloaded.
func TestMirrorRefusesBadTag(t *testing.T) {
	gh := newFakeGitHub(t, releases(fixtureArtifact(t, 0), "../escape")...)
	m := newMirror(t, t.TempDir(), gh, Options{})
	m.check(context.Background())
	if st := m.Status(); st.State != StateError || !strings.Contains(st.Error, "can't name a file") {
		t.Fatalf("status = %+v", st)
	}
	if _, _, downloads := gh.counts(); downloads != 0 {
		t.Fatalf("downloads = %d", downloads)
	}
}

func TestRemove(t *testing.T) {
	data := t.TempDir()
	if had, err := Remove(data); had || err != nil {
		t.Fatalf("Remove without a folder = %v, %v", had, err)
	}
	if err := os.MkdirAll(filepath.Join(Dir(data), "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if had, err := Remove(data); !had || err != nil {
		t.Fatalf("Remove = %v, %v", had, err)
	}
	if _, err := os.Stat(Dir(data)); !os.IsNotExist(err) {
		t.Fatalf("the folder must be gone: %v", err)
	}
}
