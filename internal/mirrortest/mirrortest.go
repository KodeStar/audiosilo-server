// Package mirrortest is test support for metadata mirror mode, shared by the
// mirror's own tests, internal/meta's, the api's and the launcher's: the data
// releases a fake GitHub publishes (audiosilo-meta's releasetest is the fake;
// this package adds the server's fixture on it), a remote metadata service that
// counts what it is asked, and a poll for a condition. Tests only.
package mirrortest

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/query/querytest"
	"github.com/kodestar/audiosilo-meta/pkg/release/releasetest"

	_ "modernc.org/sqlite" // FixtureRewritten
)

// Fixture is a data release's assets over querytest's real artifact. schema,
// when non-zero, first rewrites the copy's meta(schema_version): an artifact
// newer than the code reading it.
func Fixture(tb testing.TB, schema int) map[string][]byte {
	tb.Helper()
	if schema == 0 {
		return FixtureRewritten(tb)
	}
	return FixtureRewritten(tb, "UPDATE meta SET value="+strconv.Itoa(schema)+" WHERE key='schema_version'")
}

// FixtureRewritten is Fixture with these statements run on the built artifact first:
// the fixture's catalogue is fixed, and a test can want another shape of it.
func FixtureRewritten(tb testing.TB, stmts ...string) map[string][]byte {
	tb.Helper()
	path := querytest.Build(tb, tb.TempDir())
	if len(stmts) > 0 {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			tb.Fatal(err)
		}
		for _, stmt := range stmts {
			if _, err := db.ExecContext(context.Background(), stmt); err != nil {
				tb.Fatalf("%s: %v", stmt, err)
			}
		}
		if err := db.Close(); err != nil {
			tb.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		tb.Fatal(err)
	}
	return releasetest.DataAssets(tb, raw)
}

// HamletScale is FixtureRewritten's statements for querytest's community work as a
// sidecar on 5 acts: a character (id "act-N") and a recap (text "Act N.") at each
// position N.
func HamletScale() []string {
	const work = "'" + querytest.CommunityWork + "'"
	stmts := []string{
		"UPDATE position_scales SET length=5, unit='act', partial=0 WHERE work_id=" + work,
		"DELETE FROM character_aliases WHERE work_id=" + work,
		"DELETE FROM characters WHERE work_id=" + work,
		"DELETE FROM recaps WHERE work_id=" + work,
	}
	for act := 1; act <= 5; act++ {
		n := strconv.Itoa(act)
		stmts = append(stmts,
			"INSERT INTO characters(work_id, id, name, reveal_chapter, ord, license) VALUES("+work+", 'act-"+n+"', 'Act "+n+"', "+n+", "+n+", 'CC-BY-SA-4.0')",
			"INSERT INTO recaps(work_id, through_chapter, text, license) VALUES("+work+", "+n+", 'Act "+n+".', 'CC-BY-SA-4.0')",
		)
	}
	return stmts
}

// Releases is a realistic release list: data releases carrying assets for tags
// (published an hour apart, in order) between two code releases with no data
// assets, the newest data release listed after older ones and a code release
// published later than all of them - so neither "the first listed" nor
// "GitHub's latest" is the answer.
func Releases(assets map[string][]byte, tags ...string) []releasetest.Rel {
	t0 := time.Date(2026, 10, 9, 6, 0, 0, 0, time.UTC)
	out := []releasetest.Rel{{Tag: "v0.21.0", Published: t0.Add(time.Duration(len(tags)+5) * time.Hour)}}
	for i, tag := range tags {
		out = append(out, releasetest.Rel{Tag: tag, Published: t0.Add(time.Duration(i) * time.Hour), Assets: assets})
	}
	return append(out, releasetest.Rel{Tag: "v0.20.0", Published: t0.Add(-time.Hour)})
}

// Remote is a remote metadata service that counts every request and answers
// each one 500, so a test can assert it was never asked (closed with the test).
func Remote(tb testing.TB) (*httptest.Server, *atomic.Int32) {
	tb.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	tb.Cleanup(srv.Close)
	return srv, &hits
}

// Eventually polls cond until it holds, failing the test after timeout.
func Eventually(tb testing.TB, what string, timeout time.Duration, cond func() bool) {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			tb.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
