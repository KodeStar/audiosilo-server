package metamirror

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/query"
	"github.com/kodestar/audiosilo-meta/pkg/query/querytest"
	"github.com/kodestar/audiosilo-meta/pkg/release"
	"github.com/kodestar/audiosilo-meta/pkg/release/releasetest"

	"github.com/kodestar/audiosilo-server/internal/mirrortest"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// lots of room: the real reading would make the tests depend on the machine.
func roomy(string) (uint64, error) { return 1 << 40, nil }

// newMirror is a mirror on the fake GitHub, opened as Run's start opens it.
func newMirror(t *testing.T, dir string, gh *releasetest.GitHub, opts Options) *Mirror {
	t.Helper()
	m := newUnopened(t, dir, gh, opts)
	m.open()
	return m
}

// newUnopened is a mirror as New leaves it, before Run opens its copy.
func newUnopened(t *testing.T, dir string, gh *releasetest.GitHub, opts Options) *Mirror {
	t.Helper()
	if opts.FreeBytes == nil {
		opts.FreeBytes = roomy
	}
	if opts.Logger == nil {
		opts.Logger = quiet()
	}
	opts.Repo = releasetest.Repo
	opts.Release = []release.Option{release.WithAPIBase(gh.URL), release.WithUserAgent("AudioSilo/test")}
	m, err := New(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	m.grace = 10 * time.Millisecond
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// downloads is how many times the data asset was downloaded since the release
// list last changed.
func downloads(gh *releasetest.GitHub) int { return gh.Hits(release.DataAsset) }

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

// eventually polls cond for up to five seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	mirrortest.Eventually(t, what, 5*time.Second, cond)
}

func TestMirrorDownloadsAndSwaps(t *testing.T) {
	art := mirrortest.Fixture(t, 0)
	gh := releasetest.NewGitHub(t, mirrortest.Releases(art, "data-v2026.10.08-aaaaaaa-bbbbbbb", "data-v2026.10.09-ccccccc-ddddddd")...)
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
	if m.sizeBytes() != st.SizeBytes {
		t.Fatalf("sizeBytes = %d, want %d", m.sizeBytes(), st.SizeBytes)
	}
	if ua := gh.UserAgents()[0]; ua != "AudioSilo/test" {
		t.Fatalf("User-Agent = %q", ua)
	}

	// The copy answers metaserve's API.
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/lookup?asin="+querytest.ASIN, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), querytest.ASINWork) {
		t.Fatalf("lookup = %d %s", rec.Code, rec.Body)
	}

	// A newer data release: downloaded, swapped in, and the old copy closed and
	// deleted after the grace.
	old := m.Current()
	const next = "data-v2026.10.10-eeeeeee-fffffff"
	gh.SetReleases(mirrortest.Releases(art, tag, next)...)
	m.check(context.Background())
	if st := m.Status(); st.State != StateReady || st.Tag != next || m.Current() == old {
		t.Fatalf("after an update = %+v", st)
	}
	eventually(t, "the old copy's deletion", func() bool { return len(copies(t, dir)) == 1 })
	if got := copies(t, dir); got[0] != "meta-"+next+".sqlite" {
		t.Fatalf("copies = %v", got)
	}
	if n := downloads(gh); n != 1 {
		t.Fatalf("downloads of the new release = %d, want 1", n)
	}
}

// While a download runs the status says so, with its progress in compressed
// bytes, and lookups go to the remote service until the copy is ready.
func TestMirrorProgress(t *testing.T) {
	art := mirrortest.Fixture(t, 0)
	gh := releasetest.NewGitHub(t, mirrortest.Releases(art, "data-v2026.10.09-ccccccc-ddddddd")...)
	gz := len(art[release.DataAsset])
	gh.Throttle(gz/4+1, 150*time.Millisecond) // four chunks, slow enough to watch
	m := newMirror(t, t.TempDir(), gh, Options{})
	done := make(chan struct{})
	go func() { m.check(context.Background()); close(done) }()
	eventually(t, "the download's progress", func() bool {
		st := m.Status()
		return st.State == StateDownloading && st.Progress != nil && st.Progress.Done > 0
	})
	st := m.Status()
	if !st.Fallback || st.Progress.Total != int64(gz) || st.Progress.Done >= st.Progress.Total {
		t.Fatalf("while downloading = %+v (%+v)", st, st.Progress)
	}
	if !st.NextCheckAt.IsZero() {
		t.Fatalf("a running check has no next one yet, got %v", st.NextCheckAt)
	}
	m.CheckNow() // a check is running: this one is dropped, not queued
	<-done
	st = m.Status()
	if st.State != StateReady || st.Progress != nil {
		t.Fatalf("after the download = %+v", st)
	}
	if got := st.NextCheckAt.Sub(st.CheckedAt); got != checkInterval {
		t.Fatalf("once the check ends the next is %v after it, want a day", got)
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
	art := mirrortest.Fixture(t, 0)
	rels := mirrortest.Releases(art, "data-v2026.10.09-ccccccc-ddddddd")
	gh := releasetest.NewGitHub(t, rels...)
	m := newMirror(t, t.TempDir(), gh, Options{})
	m.check(context.Background())
	m.check(context.Background())
	if lists, notMod, n := gh.Lists(), gh.NotModified(), downloads(gh); lists != 1 || notMod != 1 || n != 1 {
		t.Fatalf("lists %d, 304s %d, downloads %d; want 1, 1, 1", lists, notMod, n)
	}
	gh.SetReleases(append(rels, releasetest.Rel{Tag: "v0.22.0"})...)
	m.check(context.Background())
	if n := downloads(gh); n != 0 {
		t.Fatalf("the same data release must not download again, downloads = %d", n)
	}
	if st := m.Status(); st.State != StateReady || st.Error != "" {
		t.Fatalf("status = %+v", st)
	}
}

// A download that fails its digest keeps the copy, records the failure, drops
// the ETag (so the next check lists again) and leaves no temp file; without a
// copy it is the error state, and lookups keep going to the remote service.
func TestMirrorBadDigestKeepsCopy(t *testing.T) {
	good := mirrortest.Fixture(t, 0)
	bad := maps.Clone(good)
	bad[release.DataDigestAsset] = []byte(strings.Repeat("0", 64) + "  meta.sqlite.gz\n")

	dir := t.TempDir()
	gh := releasetest.NewGitHub(t, mirrortest.Releases(bad, "data-v2026.10.09-ccccccc-ddddddd")...)
	var logs strings.Builder
	m := newMirror(t, dir, gh, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	m.check(context.Background())
	st := m.Status()
	if st.State != StateError || !st.Fallback || st.Error == "" || m.Ready() {
		t.Fatalf("a failed first download = %+v", st)
	}
	// With no copy the log can't say one stays in use.
	if !strings.Contains(logs.String(), "until a copy is downloaded") || strings.Contains(logs.String(), "stays in use") {
		t.Fatalf("log = %s", logs.String())
	}
	if got := st.NextCheckAt.Sub(st.CheckedAt); got != retryInterval {
		t.Fatalf("a failed check retries after %v, want an hour", got)
	}

	gh.SetReleases(mirrortest.Releases(good, "data-v2026.10.09-ccccccc-ddddddd")...)
	m.check(context.Background())
	if st := m.Status(); st.State != StateReady || st.Error != "" {
		t.Fatalf("after a good download = %+v", st)
	}
	gh.SetReleases(mirrortest.Releases(bad, "data-v2026.10.10-eeeeeee-fffffff")...)
	m.check(context.Background())
	st = m.Status()
	if st.State != StateReady || st.Tag != "data-v2026.10.09-ccccccc-ddddddd" || st.Error == "" || st.Fallback {
		t.Fatalf("a failed update over a copy = %+v", st)
	}
	if !strings.Contains(logs.String(), "the current copy stays in use") {
		t.Fatalf("log = %s", logs.String())
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
	gh := releasetest.NewGitHub(t, mirrortest.Releases(releasetest.DataAssets(t, []byte("not a database")), "data-v2026.10.09-ccccccc-ddddddd")...)
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
	gh := releasetest.NewGitHub(t, mirrortest.Releases(mirrortest.Fixture(t, query.MaxSchemaVersion+1), "data-v2026.10.09-ccccccc-ddddddd")...)
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
	gh := releasetest.NewGitHub(t, mirrortest.Releases(mirrortest.Fixture(t, 0), "data-v2026.10.09-ccccccc-ddddddd")...)
	m := newMirror(t, t.TempDir(), gh, Options{FreeBytes: func(string) (uint64, error) { return 1 << 20, nil }})
	m.check(context.Background())
	st := m.Status()
	// need: the headroom (512 MiB = 536.9 MB) and 4.5 x the small fixture.
	if st.State != StateError || !strings.HasPrefix(st.Error, "not enough disk space: need 53") ||
		!strings.HasSuffix(st.Error, " MB, have 1 MB") {
		t.Fatalf("status = %+v", st)
	}
	if n := downloads(gh); n != 0 {
		t.Fatalf("downloads = %d, want none", n)
	}
}

// The state outlives a restart: the copy opens at once, nothing is fetched, and
// the next check is a day after the last one, not shortly after start.
func TestMirrorStateSurvivesRestart(t *testing.T) {
	gh := releasetest.NewGitHub(t, mirrortest.Releases(mirrortest.Fixture(t, 0), "data-v2026.10.09-ccccccc-ddddddd")...)
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
	if lists, notMod, n := gh.Lists(), gh.NotModified(), downloads(gh); lists != 1 || notMod != 0 || n != 1 {
		t.Fatalf("lists %d, 304s %d, downloads %d", lists, notMod, n)
	}
	// The check it does run is conditional on the stored ETag.
	m2.check(context.Background())
	if notMod := gh.NotModified(); notMod != 1 {
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
	gh := releasetest.NewGitHub(t, mirrortest.Releases(mirrortest.Fixture(t, 0), "data-v2026.10.09-ccccccc-ddddddd")...)
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
	gh := releasetest.NewGitHub(t, mirrortest.Releases(mirrortest.Fixture(t, 0), "data-v2026.10.09-ccccccc-ddddddd")...)

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
	if gh.Lists()+gh.NotModified() != 0 || m.Ready() {
		t.Fatal("nothing may be checked while metadata is off")
	}

	mu.Lock()
	on = true
	mu.Unlock()
	m.CheckNow()
	// Done once the check has ended too (a running one has no next check): a
	// Check now during it is dropped.
	eventually(t, "the first download", func() bool { return m.Ready() && !m.Status().NextCheckAt.IsZero() })
	m.CheckNow()
	eventually(t, "a check on request", func() bool { return gh.NotModified() == 1 })
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
	gh := releasetest.NewGitHub(t, mirrortest.Releases(mirrortest.Fixture(t, 0), "../escape")...)
	m := newMirror(t, t.TempDir(), gh, Options{})
	m.check(context.Background())
	if st := m.Status(); st.State != StateError || !strings.Contains(st.Error, "can't name a file") {
		t.Fatalf("status = %+v", st)
	}
	if n := downloads(gh); n != 0 {
		t.Fatalf("downloads = %d", n)
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

// The copy is opened by Run, not New: until then the status says it is opening
// (not "no copy yet"), names no copy and lookups go to the remote service,
// though the next check is the copy's (a day after the last), not a first
// download's.
func TestMirrorOpensInRun(t *testing.T) {
	gh := releasetest.NewGitHub(t, mirrortest.Releases(mirrortest.Fixture(t, 0), "data-v2026.10.09-ccccccc-ddddddd")...)
	dir := t.TempDir()
	m := newMirror(t, dir, gh, Options{})
	m.check(context.Background())
	checked := m.Status().CheckedAt
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	m2 := newUnopened(t, dir, gh, Options{})
	st := m2.Status()
	if m2.Ready() || st.State != StateOpening || !st.Fallback || st.Tag != "" || st.Error != "" ||
		!st.NextCheckAt.Equal(checked.Add(checkInterval)) {
		t.Fatalf("before Run opens the copy = %+v", st)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m2.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	eventually(t, "the copy to open", m2.Ready)
	if st := m2.Status(); st.State != StateReady || st.Tag != "data-v2026.10.09-ccccccc-ddddddd" || st.SizeBytes == 0 || st.BuiltAt.IsZero() {
		t.Fatalf("once opened = %+v", st)
	}
}

// Check now makes the next check due now in the status it answers with.
func TestMirrorCheckNowIsDueNow(t *testing.T) {
	gh := releasetest.NewGitHub(t, mirrortest.Releases(mirrortest.Fixture(t, 0), "data-v2026.10.09-ccccccc-ddddddd")...)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	m := newMirror(t, t.TempDir(), gh, Options{Now: func() time.Time { return now }})
	if st := m.Status(); !st.NextCheckAt.Equal(now.Add(startDelay)) {
		t.Fatalf("before = %v", st.NextCheckAt)
	}
	m.CheckNow()
	if st := m.Status(); !st.NextCheckAt.Equal(now) {
		t.Fatalf("after Check now = %v, want now", st.NextCheckAt)
	}
	m.check(context.Background())
	if st := m.Status(); !st.NextCheckAt.Equal(now.Add(checkInterval)) {
		t.Fatalf("after the check = %v", st.NextCheckAt)
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[uint64]string{
		0: "0 B", 999: "999 B", 1000: "1 kB", 1 << 20: "1 MB", 1_066_192_076: "1.1 GB",
		2_534_030_000: "2.5 GB", 15_300_000: "15 MB", 1_760_000_000_000: "1.8 TB",
	} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// Metadata turned off while a download runs stops it: no copy, nothing left in
// the folder, and nothing recorded (the check is due again once it is back on).
func TestMirrorStopsWhenDisabled(t *testing.T) {
	art := mirrortest.Fixture(t, 0)
	gh := releasetest.NewGitHub(t, mirrortest.Releases(art, "data-v2026.10.09-ccccccc-ddddddd")...)
	gh.Throttle(len(art[release.DataAsset])/20+1, 200*time.Millisecond) // about four seconds
	var on atomic.Bool
	on.Store(true)
	dir := t.TempDir()
	m := newMirror(t, dir, gh, Options{Enabled: on.Load})
	m.enabledPoll = 10 * time.Millisecond
	done := make(chan struct{})
	go func() { m.checkWhileEnabled(context.Background()); close(done) }()
	eventually(t, "the download to start", func() bool {
		st := m.Status()
		return st.State == StateDownloading && st.Progress != nil && st.Progress.Done > 0
	})
	on.Store(false)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("turning metadata off must stop the download")
	}
	st := m.Status()
	if m.Ready() || st.State != StateEmpty || !st.CheckedAt.IsZero() || st.Error != "" {
		t.Fatalf("after a stopped download = %+v", st)
	}
	eventually(t, "no copy or temp file", func() bool {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.Name() != stateFile {
				return false
			}
		}
		return true
	})
}

// A release swapped back in while the copy it replaced is still in its grace
// (the newer release withdrawn) has the same file name: retiring the old handle
// must not delete the file the current copy is.
func TestMirrorSwapBackKeepsFile(t *testing.T) {
	art := mirrortest.Fixture(t, 0)
	const a, b = "data-v2026.10.08-aaaaaaa-bbbbbbb", "data-v2026.10.09-ccccccc-ddddddd"
	gh := releasetest.NewGitHub(t, mirrortest.Releases(art, a)...)
	dir := t.TempDir()
	m := newMirror(t, dir, gh, Options{})
	m.grace = time.Hour // the replaced copies stay in their grace until Close
	m.check(context.Background())
	first := m.Current()
	gh.SetReleases(mirrortest.Releases(art, a, b)...)
	m.check(context.Background())
	gh.SetReleases(mirrortest.Releases(art, a)...)
	m.check(context.Background())
	if st := m.Status(); st.Tag != a || m.Current() == first {
		t.Fatalf("after the swap back = %+v", st)
	}
	m.retire(first)
	if got := copies(t, dir); len(got) != 2 || !slices.Contains(got, "meta-"+a+".sqlite") {
		t.Fatalf("retiring the old handle deleted the current copy: %v", got)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if got := copies(t, dir); len(got) != 1 || got[0] != "meta-"+a+".sqlite" {
		t.Fatalf("after Close = %v, want the current copy only", got)
	}
}

// A release swapped back in while the copy it replaced is still in its grace,
// with that copy's retire timer firing after the new download took its name but
// before the swap (cur is still the newer release then): the file being opened
// must not be deleted under the install.
func TestMirrorSwapBackRetireDuringInstall(t *testing.T) {
	art := mirrortest.Fixture(t, 0)
	const a, b = "data-v2026.10.08-aaaaaaa-bbbbbbb", "data-v2026.10.09-ccccccc-ddddddd"
	gh := releasetest.NewGitHub(t, mirrortest.Releases(art, a)...)
	dir := t.TempDir()
	m := newMirror(t, dir, gh, Options{})
	m.grace = time.Hour // the timer is fired by hand below
	m.check(context.Background())
	first := m.Current()
	gh.SetReleases(mirrortest.Releases(art, a, b)...)
	m.check(context.Background())

	gh.SetReleases(mirrortest.Releases(art, a)...)
	opening, release := gatedOpen(m)
	done := make(chan struct{})
	go func() { m.check(context.Background()); close(done) }()
	<-opening // A is downloaded to meta-A.sqlite again; B still answers
	m.retire(first)
	if _, err := os.Stat(filepath.Join(dir, "meta-"+a+".sqlite")); err != nil {
		t.Fatalf("retiring the old handle deleted the copy being installed: %v", err)
	}
	close(release)
	<-done
	if st := m.Status(); st.State != StateReady || st.Tag != a || st.Error != "" || m.Current() == first {
		t.Fatalf("after the swap back = %+v", st)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if got := copies(t, dir); len(got) != 1 || got[0] != "meta-"+a+".sqlite" {
		t.Fatalf("after Close = %v, want the current copy only", got)
	}
}

// gatedOpen makes m's copies open only once release is closed, and reports on
// opening when an open is waiting: the seconds query.Open takes over a real copy.
func gatedOpen(m *Mirror) (opening <-chan struct{}, release chan<- struct{}) {
	waiting, gate := make(chan struct{}, 4), make(chan struct{})
	m.openDB = func(path, tag string) (*query.DB, error) {
		waiting <- struct{}{}
		<-gate
		return query.Open(path, tag)
	}
	return waiting, gate
}

// While Run opens the copy on disk the status says opening, with lookups going
// to the remote service; then ready. The console polls while it does.
func TestMirrorStatusOpeningAtStart(t *testing.T) {
	gh := releasetest.NewGitHub(t, mirrortest.Releases(mirrortest.Fixture(t, 0), "data-v2026.10.09-ccccccc-ddddddd")...)
	dir := t.TempDir()
	m := newMirror(t, dir, gh, Options{})
	m.check(context.Background())
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	m2 := newUnopened(t, dir, gh, Options{})
	opening, release := gatedOpen(m2)
	done := make(chan struct{})
	go func() { m2.open(); close(done) }()
	<-opening
	if st := m2.Status(); st.State != StateOpening || !st.Fallback || st.Progress != nil || st.Error != "" {
		t.Fatalf("while opening = %+v", st)
	}
	close(release)
	<-done
	if st := m2.Status(); st.State != StateReady || st.Fallback {
		t.Fatalf("once open = %+v", st)
	}
}

// Once the copy on disk is open it answers, and the status says ready while
// open still sweeps the folder: not opening, and never the "over a copy"
// wording of an update (opening without fallback).
func TestMirrorStatusReadyWhileSweeping(t *testing.T) {
	gh := releasetest.NewGitHub(t, mirrortest.Releases(mirrortest.Fixture(t, 0), "data-v2026.10.09-ccccccc-ddddddd")...)
	dir := t.TempDir()
	m := newMirror(t, dir, gh, Options{})
	m.check(context.Background())
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	m2 := newUnopened(t, dir, gh, Options{})
	m2.openCopy() // open's first half: the sweep (and opened) still to come
	if st := m2.Status(); !m2.Ready() || st.State != StateReady || st.Fallback || st.Tag == "" {
		t.Fatalf("an open copy before the sweep = %+v", st)
	}
}

// A finished download is opened before it answers: the status says opening
// (no progress bar stuck at 100%), with lookups where they were: the remote
// service for a first copy, the current copy over an update.
func TestMirrorStatusOpeningAfterDownload(t *testing.T) {
	art := mirrortest.Fixture(t, 0)
	const first, next = "data-v2026.10.09-ccccccc-ddddddd", "data-v2026.10.10-eeeeeee-fffffff"
	gh := releasetest.NewGitHub(t, mirrortest.Releases(art, first)...)
	m := newMirror(t, t.TempDir(), gh, Options{})
	opening, release := gatedOpen(m)

	done := make(chan struct{})
	go func() { m.check(context.Background()); close(done) }()
	<-opening
	if st := m.Status(); st.State != StateOpening || !st.Fallback || st.Progress != nil || st.Tag != "" {
		t.Fatalf("opening a first copy = %+v", st)
	}
	release <- struct{}{}
	<-done
	if st := m.Status(); st.State != StateReady || st.Tag != first {
		t.Fatalf("after the first copy = %+v", st)
	}

	gh.SetReleases(mirrortest.Releases(art, first, next)...)
	done = make(chan struct{})
	go func() { m.check(context.Background()); close(done) }()
	<-opening
	if st := m.Status(); st.State != StateOpening || st.Fallback || st.Tag != first || st.Progress != nil {
		t.Fatalf("opening an update over a copy = %+v", st)
	}
	close(release)
	<-done
	if st := m.Status(); st.State != StateReady || st.Tag != next {
		t.Fatalf("after the update = %+v", st)
	}
}

// Metadata turned back on wakes Run (Wake): a check that came due while it was
// off runs at once, not after the disabled poll's minute; a wake with nothing
// due checks nothing.
func TestMirrorWake(t *testing.T) {
	gh := releasetest.NewGitHub(t, mirrortest.Releases(mirrortest.Fixture(t, 0), "data-v2026.10.09-ccccccc-ddddddd")...)
	var on atomic.Bool
	m := newMirror(t, t.TempDir(), gh, Options{Enabled: on.Load})
	m.startAfter = 0
	m.first = time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	time.Sleep(50 * time.Millisecond) // Run is in its disabled poll
	on.Store(true)
	m.Wake()
	eventually(t, "the due check after a wake", func() bool { return m.Ready() && !m.Status().NextCheckAt.IsZero() })

	// The copy is fresh: the next check is a day away, and waking checks nothing.
	lists := gh.Lists() + gh.NotModified()
	m.Wake()
	time.Sleep(100 * time.Millisecond)
	if got := gh.Lists() + gh.NotModified(); got != lists {
		t.Fatalf("a wake with no check due listed the releases (%d -> %d)", lists, got)
	}
}
