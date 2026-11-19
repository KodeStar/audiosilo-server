package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The re-key's fixtures, from explicit code points (never pasted).
const (
	rkBook     = "Émile Zola/Germinal"  // NFC on disk: three tracks
	rkBookNFD  = "Émile Zola/Germinal" // the same, decomposed
	rkTrack1   = rkBook + "/01.mp3"     // 100 s, the first
	rkTrack2   = rkBook + "/02.mp3"     // 200 s
	rkTrack3   = rkBook + "/03.mp3"     // 50 s
	rkTrack1D  = rkBookNFD + "/01.mp3"  // track 1 in the other spelling
	rkAmelie   = "Amélie/Le Fabuleux"  // NFD on disk
	rkAmelieC  = "Amélie/Le Fabuleux"   // the same, precomposed
	rkJoined   = "Joined"               // a book joined from its disc folders
	rkDisc1    = rkJoined + "/CD 1"
	rkNavBook  = "Auteurs/Livre"
	rkNav      = "Auteurs" // a navigation folder
	rkOrphan   = "Gone/Book"
	rkEpisode  = "Show/episode.mp3"
	rkBookLen  = 350.0
	rkUpdated1 = "2026-01-01T10:00:00.000Z"
	rkUpdated2 = "2026-02-01T10:00:00.000Z"
)

// rekeyFixture is a books library with the fixtures indexed, a podcast library,
// and the users: ann and bob (whole library), adm (admin), dan (granted track 3
// only) and eve (whole library).
type rekeyFixture struct {
	c                       *Catalog
	ctx                     context.Context
	lib, pod                int64
	ann, bob, adm, dan, eve int64
}

func newRekeyFixture(t *testing.T) *rekeyFixture {
	t.Helper()
	c, ctx := newTestCatalog(t)
	f := &rekeyFixture{c: c, ctx: ctx}
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if err != nil {
		t.Fatal(err)
	}
	pod, err := c.CreateLibrary(ctx, Library{Name: "P", Root: "/tmp/p", Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	f.lib, f.pod = lib.ID, pod.ID
	upsert(t, c, &Book{LibraryID: lib.ID, RelPath: rkBook, IsFolder: true, Title: "Germinal", Duration: rkBookLen,
		Files: []BookFile{{RelPath: rkTrack1, Duration: 100}, {RelPath: rkTrack2, Seq: 1, Duration: 200},
			{RelPath: rkTrack3, Seq: 2, Duration: 50}}})
	upsert(t, c, &Book{LibraryID: lib.ID, RelPath: rkAmelie, IsFolder: true, Title: "Amelie", Duration: 60,
		Files: []BookFile{{RelPath: rkAmelie + "/01.mp3", Duration: 60}}})
	upsert(t, c, &Book{LibraryID: lib.ID, RelPath: rkJoined, IsFolder: true, Title: "Joined", Duration: 90,
		Files: []BookFile{{RelPath: rkDisc1 + "/01.mp3", Duration: 40}, {RelPath: rkJoined + "/CD 2/01.mp3", Seq: 1, Duration: 50}}})
	upsert(t, c, &Book{LibraryID: lib.ID, RelPath: rkNavBook, IsFolder: true, Title: "Livre",
		Files: []BookFile{{RelPath: rkNavBook + "/01.mp3"}}})
	f.ann, f.bob, f.dan, f.eve = seedNamedUser(t, c, "ann"), seedNamedUser(t, c, "bob"), seedNamedUser(t, c, "dan"),
		seedNamedUser(t, c, "eve")
	f.adm = seedNamedUser(t, c, "adm")
	f.exec(t, `UPDATE users SET role = 'admin' WHERE id = ?`, f.adm)
	for _, u := range []int64{f.ann, f.bob, f.eve} {
		if err := c.GrantWholeLibrary(ctx, u, lib.ID); err != nil {
			t.Fatal(err)
		}
	}
	share, err := c.CreateShare(ctx, Share{Name: "One track"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AddSharePath(ctx, share.ID, PathRule{LibraryID: lib.ID, Path: rkTrack3}); err != nil {
		t.Fatal(err)
	}
	if err := c.GrantShare(ctx, f.dan, share.ID); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *rekeyFixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.c.db.ExecContext(f.ctx, q, args...); err != nil {
		t.Fatalf("%s: %v", strings.SplitN(q, "\n", 2)[0], err)
	}
}

// seedEverything writes a row in every per-user table at p for user in lib, as
// an older server wrote them (straight into the tables), with updated as their
// save time; pos is the place on the book's timeline.
func (f *rekeyFixture) seedEverything(t *testing.T, user, lib int64, p string, pos float64, updated string) {
	t.Helper()
	f.exec(t, `INSERT INTO progress(user_id, library_id, rel_path, position, duration, version, device_id, updated_at, device_name)
		VALUES(?, ?, ?, ?, ?, 3, 'dev', ?, 'Phone')`, user, lib, p, pos, rkBookLen, updated)
	f.exec(t, `INSERT INTO bookmarks(user_id, library_id, rel_path, position, note, created_at) VALUES(?, ?, ?, ?, 'mark', ?)`,
		user, lib, p, pos, updated)
	f.exec(t, `INSERT INTO notes(user_id, library_id, rel_path, position, body, created_at, updated_at)
		VALUES(?, ?, ?, ?, 'a note', ?, ?)`, user, lib, p, pos, updated, updated)
	f.exec(t, `INSERT INTO listening_history(user_id, library_id, rel_path, from_pos, to_pos, started_at, ended_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`, user, lib, p, pos-10, pos, updated, updated)
	f.exec(t, `INSERT INTO listening_sessions(user_id, library_id, rel_path, started_at, last_at, start_pos, end_pos, duration)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, user, lib, p, updated, updated, pos-10, pos, rkBookLen)
	f.exec(t, `INSERT INTO listening_daily(day, user_id, library_id, rel_path, listened, sessions) VALUES('2026-01-01', ?, ?, ?, 10, 1)`,
		user, lib, p)
	f.exec(t, `INSERT INTO listening_events(user_id, event_id, library_id, rel_path, kind, from_pos, to_pos, started_at, ended_at,
		    status, received_at) VALUES(?, ?, ?, ?, 'span', ?, ?, ?, ?, 'applied', ?)`,
		user, fmt.Sprintf("e-%d-%s", user, p), lib, p, pos-10, pos, updated, updated, updated)
	res, err := f.c.db.ExecContext(f.ctx, `INSERT INTO imports(user_id, source, source_url, status, created_at)
		VALUES(?, 'abs', 'http://abs', 'applied', 't')`, user)
	if err != nil {
		t.Fatal(err)
	}
	imp, _ := res.LastInsertId()
	f.exec(t, `INSERT INTO import_progress_prior(import_id, user_id, library_id, rel_path, prior, wrote) VALUES(?, ?, ?, ?, NULL, '{}')`,
		imp, user, lib, p)
	f.exec(t, `INSERT INTO up_next(user_id, library_id, rel_path, position, added_at) VALUES(?, ?, ?, 0, ?)`, user, lib, p, updated)
	coll, err := f.c.CreateCollection(f.ctx, user, fmt.Sprintf("c-%d-%s", user, p), "")
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO collection_items(collection_id, library_id, rel_path, position, added_at) VALUES(?, ?, ?, 0, ?)`,
		coll.ID, lib, p, updated)
	f.exec(t, `INSERT INTO ratings(user_id, library_id, rel_path, rating, note, created_at, updated_at) VALUES(?, ?, ?, 4, 'good', ?, ?)`,
		user, lib, p, updated, updated)
	f.exec(t, `INSERT INTO reading_progress(user_id, library_id, rel_path, cfi, progression, version, updated_at)
		VALUES(?, ?, ?, 'epubcfi(/6/2)', 0.5, 1, ?)`, user, lib, p, updated)
	f.exec(t, `INSERT INTO favourites(user_id, library_id, rel_path, created_at) VALUES(?, ?, ?, ?)`, user, lib, p, updated)
}

// dumpRows is every row of table at p in lib, one line each (column=type:value,
// in the table's order), sorted.
func dumpRows(t *testing.T, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, table string, lib int64, p string) []string {
	t.Helper()
	rs, err := q.QueryContext(context.Background(), `SELECT * FROM `+table+` WHERE library_id = ? AND rel_path = ?`, lib, p)
	if err != nil {
		t.Fatal(err)
	}
	return formatRows(t, rs)
}

func formatRows(t *testing.T, rs *sql.Rows) []string {
	t.Helper()
	defer rs.Close()
	names, _ := rs.Columns()
	var out []string
	for rs.Next() {
		vals := make([]any, len(names))
		dest := make([]any, len(names))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err := rs.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		cols := make([]string, len(names))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			cols[i] = fmt.Sprintf("%s=%T:%v", names[i], v, v)
		}
		out = append(out, strings.Join(cols, " "))
	}
	if err := rs.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// restoreLogged is a path_rekeys row put back as the table's row: decoded from
// row_json and inserted into a scratch copy of table's columns (the same column
// types), then read back as dumpRows reads the table.
func restoreLogged(t *testing.T, c *Catalog, table, rowJSON string) string {
	t.Helper()
	var out []string
	err := c.db.WithTx(context.Background(), "test", func(tx *sql.Tx) error {
		ctx := context.Background()
		if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE restored AS SELECT * FROM `+table+` WHERE 0`); err != nil {
			return err
		}
		dec := json.NewDecoder(strings.NewReader(rowJSON))
		dec.UseNumber()
		var row map[string]any
		if err := dec.Decode(&row); err != nil {
			return err
		}
		names, args := make([]string, 0, len(row)), make([]any, 0, len(row))
		for k, v := range row {
			if n, ok := v.(json.Number); ok {
				if i, err := n.Int64(); err == nil && !strings.ContainsAny(n.String(), ".eE") {
					v = i
				} else if v, err = n.Float64(); err != nil {
					return err
				}
			}
			names, args = append(names, k), append(args, v)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO restored(`+strings.Join(names, ", ")+`) VALUES(`+
			placeholders(len(names))+`)`, args...); err != nil {
			return err
		}
		rs, err := tx.QueryContext(ctx, `SELECT * FROM restored`)
		if err != nil {
			return err
		}
		out = formatRows(t, rs)
		_, err = tx.ExecContext(ctx, `DROP TABLE temp.restored`)
		return err
	})
	if err != nil || len(out) != 1 {
		t.Fatalf("restoring a %s row: %v (%d rows)", table, err, len(out))
	}
	return out[0]
}

var rekeyedTables = []string{"progress", "bookmarks", "notes", "listening_history", "listening_sessions", "listening_daily",
	"listening_events", "import_progress_prior", "up_next", "collection_items", "ratings", "reading_progress", "favourites"}

// TestRekeyTablesMatchCarry: rekeyTables names every table carryState writes,
// in the carry group that writes it, so no carried row is missing from the log.
func TestRekeyTablesMatchCarry(t *testing.T) {
	want := map[string]carrySet{"progress": carryTimeline, "favourites": carryFavs}
	table := regexp.MustCompile(`(?:UPDATE(?: OR IGNORE)?|INSERT INTO|DELETE FROM)\s+(\w+)`)
	for set, stmts := range map[carrySet][]string{carryTimeline: timelineCarries, carryBookLevel: bookLevelCarries} {
		for _, stmt := range stmts {
			m := table.FindStringSubmatch(stmt)
			if m == nil {
				t.Fatalf("no table in %q", stmt)
			}
			want[m[1]] = set
		}
	}
	got := map[string]carrySet{}
	for _, rt := range rekeyTables {
		got[rt.name] = rt.set
	}
	if !maps.Equal(got, want) {
		t.Errorf("rekeyTables = %v, carried = %v", got, want)
	}
	var names []string
	for _, rt := range rekeyTables {
		names = append(names, rt.name)
	}
	if !slices.Equal(names, rekeyedTables) {
		t.Errorf("rekeyTables = %v, the tests' rekeyedTables = %v", names, rekeyedTables)
	}
}

// rowCount is how many rows table holds at p in lib.
func (f *rekeyFixture) rowCount(t *testing.T, table string, lib int64, p string) int {
	t.Helper()
	return len(dumpRows(t, f.c.db, table, lib, p))
}

// TestRekeyPathState (server#210): a seeded database's rows under a track path,
// another Unicode spelling and a disc folder move to the book as their reason
// allows, each logged whole first; collisions keep the newer save; guards,
// favourites on tracks and navigation folders, orphans and podcast rows stay;
// a second run is a no-op; the log puts every moved row back exactly.
func TestRekeyPathState(t *testing.T) {
	t.Parallel()
	f := newRekeyFixture(t)
	c, ctx := f.c, f.ctx

	// ann: everything under track 1 (an old player at a track path), and a newer
	// save of her own on the book, not as far on: the collision keeps the newer
	// (mergeNewest), not the furthest.
	f.seedEverything(t, f.ann, f.lib, rkTrack1, 300, rkUpdated1)
	if _, err := c.SaveProgress(ctx, f.ann, Progress{Ref: Ref{LibraryID: f.lib, Path: rkBook}, Position: 220,
		UpdatedAt: rkUpdated2, Version: 7}); err != nil {
		t.Fatal(err)
	}
	// bob: everything under the decomposed spelling of the book, newer than his
	// rating on the book's own path (the carried one wins there).
	f.seedEverything(t, f.bob, f.lib, rkBookNFD, 80, rkUpdated2)
	if _, err := c.SetRating(ctx, f.bob, Ref{LibraryID: f.lib, Path: rkBook}, 2, "older"); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE ratings SET updated_at = ? WHERE user_id = ? AND rel_path = ?`, rkUpdated1, f.bob, rkBook)
	// adm: everything under the precomposed spelling of a book stored decomposed,
	// and under track 1 in the decomposed spelling (a track in another spelling).
	f.seedEverything(t, f.adm, f.lib, rkAmelieC, 30, rkUpdated1)
	f.seedEverything(t, f.adm, f.lib, rkTrack1D, 40, rkUpdated1)
	// eve: everything under the disc folder of the joined book (book-level rows
	// move, timeline rows stay) and, on track 2, a place in that track only (its
	// length is the track's): the timeline guard keeps it.
	f.seedEverything(t, f.eve, f.lib, rkDisc1, 20, rkUpdated1)
	f.exec(t, `INSERT INTO progress(user_id, library_id, rel_path, position, duration, updated_at) VALUES(?, ?, ?, 30, 200, ?)`,
		f.eve, f.lib, rkTrack2, rkUpdated1)
	// dan reaches only track 3, and eve shares the path: the scope guard is per
	// listener, so dan's row stays (logged once, reason scope) and eve's moves.
	f.exec(t, `INSERT INTO progress(user_id, library_id, rel_path, position, duration, updated_at) VALUES(?, ?, ?, 10, ?, ?)`,
		f.dan, f.lib, rkTrack3, rkBookLen, rkUpdated1)
	f.exec(t, `INSERT INTO bookmarks(user_id, library_id, rel_path, position, created_at) VALUES(?, ?, ?, 300, ?)`,
		f.eve, f.lib, rkTrack3, rkUpdated1)
	// Untouched: a navigation folder's favourite, an orphan's progress, a podcast
	// episode's progress (an episode is its own book).
	f.exec(t, `INSERT INTO favourites(user_id, library_id, rel_path, created_at) VALUES(?, ?, ?, 't')`, f.ann, f.lib, rkNav)
	f.exec(t, `INSERT INTO progress(user_id, library_id, rel_path, position, duration, updated_at) VALUES(?, ?, ?, 5, 9, ?)`,
		f.ann, f.lib, rkOrphan, rkUpdated1)
	f.exec(t, `INSERT INTO progress(user_id, library_id, rel_path, position, duration, updated_at) VALUES(?, ?, ?, 5, 9, ?)`,
		f.ann, f.pod, rkEpisode, rkUpdated1)

	// What every moving path holds, to check the log against.
	type at struct{ table, path string }
	before := map[at][]string{}
	for _, p := range []string{rkTrack1, rkBookNFD, rkAmelieC, rkTrack1D, rkDisc1, rkTrack3} {
		for _, table := range rekeyedTables {
			before[at{table, p}] = dumpRows(t, c.db, table, f.lib, p)
		}
	}
	total := func() map[string]int {
		n := map[string]int{}
		for _, table := range rekeyedTables {
			var k int
			if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&k); err != nil {
				t.Fatal(err)
			}
			n[table] = k
		}
		return n
	}
	countsBefore := total()

	rep, err := c.RekeyPathState(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Failed) != 0 {
		t.Fatalf("failed: %+v", rep.Failed)
	}
	if rep.Moved[rekeySpelling] != 2 || rep.Moved[rekeyFile] != 3 || rep.Moved[rekeyFolder] != 1 || rep.Kept != 1 {
		t.Errorf("moved = %v, kept %d, want spelling 2 (bob's, adm's book), file 3 (ann's, adm's track, eve's on track 3), "+
			"folder 1 (eve's disc), dan's one row kept", rep.Moved, rep.Kept)
	}
	guards := map[string]string{}
	for _, s := range rep.Skipped {
		guards[s.Path] = s.Why
	}
	if guards[rkTrack3] != RekeyGuardScope || guards[rkTrack2] != RekeyGuardTimeline || len(guards) != 2 {
		t.Errorf("skipped = %+v, want track 3 by scope, track 2 by timeline", rep.Skipped)
	}

	// The moves, by table: what stays and where the rest went. A track row's
	// heart stays on the track (file), in the index's spelling: adm's, on track 1
	// in the other spelling, joins ann's on track 1.
	stays := map[at]bool{{"favourites", rkTrack1}: true, {"progress", rkTrack3}: true}
	gains := map[at]int{{"favourites", rkTrack1}: 1}
	for _, table := range rekeyedTables {
		if i := slices.IndexFunc(rekeyTables, func(rt rekeyTable) bool { return rt.name == table }); rekeyTables[i].set != carryBookLevel {
			stays[at{table, rkDisc1}] = true // a disc's timeline and its heart (folder)
		}
	}
	for k, rows := range before {
		got := f.rowCount(t, k.table, f.lib, k.path)
		if want := map[bool]int{true: len(rows), false: 0}[stays[k]] + gains[k]; got != want {
			t.Errorf("%s at %+q: %d rows left, want %d", k.table, k.path, got, want)
		}
	}
	for _, k := range []at{{"progress", rkTrack2}, {"favourites", rkNav}, {"progress", rkOrphan}} {
		if f.rowCount(t, k.table, f.lib, k.path) != 1 {
			t.Errorf("%s at %+q moved, want it left", k.table, k.path)
		}
	}
	if f.rowCount(t, "progress", f.pod, rkEpisode) != 1 {
		t.Error("a podcast row moved")
	}

	// Positions unchanged; the collision kept the newer save.
	book := Ref{LibraryID: f.lib, Path: rkBook}
	if p, _ := c.GetProgress(ctx, f.ann, book); p == nil || p.Position != 220 || p.UpdatedAt != rkUpdated2 || p.Version != 8 {
		t.Errorf("ann's progress = %+v, want her newer book save at 220, version above both", p)
	}
	if p, _ := c.GetProgress(ctx, f.bob, book); p == nil || p.Position != 80 {
		t.Errorf("bob's progress = %+v, want 80 from the other spelling", p)
	}
	if bms, _ := c.ListBookmarks(ctx, f.ann, book); len(bms) != 1 || bms[0].Position != 300 {
		t.Errorf("ann's bookmarks on the book = %+v, want one at 300", bms)
	}
	if bms, _ := c.ListBookmarks(ctx, f.eve, book); len(bms) != 1 || bms[0].Position != 300 {
		t.Errorf("eve's bookmarks on the book = %+v, want hers from track 3 though dan's row there stays", bms)
	}
	if p, _ := c.GetProgress(ctx, f.dan, Ref{LibraryID: f.lib, Path: rkTrack3}); p == nil || p.Position != 10 {
		t.Errorf("dan's progress on track 3 = %+v, want it kept where he can reach it", p)
	}
	if r, _ := c.GetRating(ctx, f.bob, book); r == nil || r.Stars != 4 {
		t.Errorf("bob's rating = %+v, want the newer carried 4", r)
	}
	if favs := f.rowCount(t, "favourites", f.lib, rkBook); favs != 1 {
		t.Errorf("favourites on the book = %d, want bob's (spelling) only", favs)
	}
	if f.rowCount(t, "favourites", f.lib, rkAmelie) != 1 || f.rowCount(t, "progress", f.lib, rkAmelie) != 1 {
		t.Error("adm's rows under the precomposed spelling did not reach the decomposed book")
	}
	if f.rowCount(t, "ratings", f.lib, rkJoined) != 1 || f.rowCount(t, "up_next", f.lib, rkJoined) != 1 {
		t.Error("eve's disc-folder rating and list entry did not reach the joined book")
	}

	// Every row before is a row after or a logged loser: counts reconcile.
	var logged, kept int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM path_rekeys WHERE reason != ?`, RekeyGuardScope).
		Scan(&logged); err != nil {
		t.Fatal(err)
	}
	if logged != rep.Rows {
		t.Errorf("path_rekeys holds %d moved rows, the report %d", logged, rep.Rows)
	}
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM path_rekeys WHERE reason = ? AND user_id = ?
		AND table_name = 'progress' AND from_path = ? AND to_path = ?`, RekeyGuardScope, f.dan, rkTrack3, rkBook).
		Scan(&kept); err != nil || kept != 1 {
		t.Errorf("dan's kept row logged %d times (%v), want once", kept, err)
	}
	logTotal := func() int {
		var n int
		if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM path_rekeys`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	logs0 := logTotal()
	after := total()
	// The merges: ann's track-1 progress onto her own book save, bob's rating
	// in the other spelling onto his rating on the book.
	merged := map[string]int{"progress": 1, "ratings": 1}
	for _, table := range rekeyedTables {
		if countsBefore[table] != after[table]+merged[table] {
			t.Errorf("%s: %d rows before, %d after, %d merged", table, countsBefore[table], after[table], merged[table])
		}
	}

	// The log puts back every moved row exactly, attributed to its listener.
	type logRow struct {
		table, from, js string
		user            sql.NullInt64
	}
	logs, err := queryRows(ctx, c.db, func(r *sql.Rows, l *logRow) error { return r.Scan(&l.table, &l.from, &l.js, &l.user) },
		`SELECT table_name, from_path, row_json, user_id FROM path_rekeys WHERE reason != ?`, RekeyGuardScope)
	if err != nil {
		t.Fatal(err)
	}
	restored := map[at][]string{}
	for _, l := range logs {
		if !l.user.Valid {
			t.Errorf("a logged %s row has no listener", l.table)
		}
		restored[at{l.table, l.from}] = append(restored[at{l.table, l.from}], restoreLogged(t, c, l.table, l.js))
	}
	for k, rows := range before {
		if stays[k] {
			rows = nil
		}
		got := restored[k]
		slices.Sort(got)
		if strings.Join(got, "\n") != strings.Join(rows, "\n") {
			t.Errorf("%s at %+q restored:\n%s\nwant:\n%s", k.table, k.path, strings.Join(got, "\n"), strings.Join(rows, "\n"))
		}
	}

	// The version is recorded, so the next start does nothing; a scan-end run
	// finds nothing left to move either.
	if v, _ := getServerState[string](ctx, c.db, rekeyStateKey); v != pathKeysVersion {
		t.Errorf("server_state %s = %q, want %q", rekeyStateKey, v, pathKeysVersion)
	}
	again, err := c.RekeyPathState(ctx, nil, nil)
	if err != nil || len(again.Moved) != 0 || len(again.Skipped) != 0 || again.Rows != 0 {
		t.Errorf("second start: %+v %v, want nothing", again, err)
	}
	f.exec(t, `DELETE FROM server_state WHERE key = ?`, rekeyStateKey)
	rerun, err := c.RekeyPathState(ctx, nil, nil)
	if err != nil || len(rerun.Moved) != 0 || rerun.Rows != 0 || len(rerun.Failed) != 0 {
		t.Errorf("a full re-run: %+v %v, want nothing moved", rerun, err)
	}
	scan, err := c.RekeyLibrary(ctx, f.lib)
	if err != nil || len(scan.Moved) != 0 {
		t.Errorf("scan-end run: %+v %v, want nothing moved", scan, err)
	}
	if !maps.Equal(total(), after) {
		t.Error("a re-run changed the tables")
	}
	if rerun.Kept != 1 || scan.Kept != 1 || logTotal() != logs0 {
		t.Errorf("re-runs kept %d and %d rows and grew the log by %d, want dan's one row each time, logged once",
			rerun.Kept, scan.Kept, logTotal()-logs0)
	}

	// A user delete takes their logged rows with them.
	f.exec(t, `DELETE FROM users WHERE id = ?`, f.bob)
	var bobs int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM path_rekeys WHERE user_id = ?`, f.bob).Scan(&bobs); err != nil || bobs != 0 {
		t.Errorf("bob's logged rows after his delete: %d (%v)", bobs, err)
	}
}

// TestRekeyScopeGuardAllowsReachableBook: the scope guard is per path: dan,
// granted track 3 only, keeps his row there, while on a book he can reach (a
// grant on the whole book) his track row moves.
func TestRekeyScopeGuardAllowsReachableBook(t *testing.T) {
	t.Parallel()
	f := newRekeyFixture(t)
	share, err := f.c.CreateShare(f.ctx, Share{Name: "Amelie"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.c.AddSharePath(f.ctx, share.ID, PathRule{LibraryID: f.lib, Path: rkAmelie}); err != nil {
		t.Fatal(err)
	}
	if err := f.c.GrantShare(f.ctx, f.dan, share.ID); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{rkTrack3, rkAmelie + "/01.mp3"} {
		f.exec(t, `INSERT INTO progress(user_id, library_id, rel_path, position, duration, updated_at) VALUES(?, ?, ?, 10, ?, ?)`,
			f.dan, f.lib, p, rkBookLen, rkUpdated1)
	}
	rep, err := f.c.RekeyLibrary(f.ctx, f.lib)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Moved[rekeyFile] != 1 || len(rep.Skipped) != 1 || rep.Skipped[0].Path != rkTrack3 {
		t.Fatalf("report = %+v, want the reachable book's track moved and track 3 kept", rep)
	}
	if f.rowCount(t, "progress", f.lib, rkAmelie) != 1 || f.rowCount(t, "progress", f.lib, rkTrack3) != 1 {
		t.Error("rows not where the guard leaves them")
	}
}

// TestRekeyScopeIsPerListener: on one path, the rows of a listener who can't
// reach where they would go (dan, granted track 3 only; zed, whose access was
// removed) stay, every table of them, logged once with reason scope, while the
// rows of a listener who can (ann) all move: one listener never pins another's.
func TestRekeyScopeIsPerListener(t *testing.T) {
	t.Parallel()
	f := newRekeyFixture(t)
	zed := seedNamedUser(t, f.c, "zed") // no grant: access since removed
	// Track 1 in the other spelling: the file's rows go to the book, its heart
	// to track 1 in the index's spelling.
	for _, u := range []int64{f.ann, f.dan, zed} {
		f.seedEverything(t, u, f.lib, rkTrack1D, 30, rkUpdated1)
	}
	rows := func(table string, user int64, p string) int {
		q := `SELECT COUNT(*) FROM ` + table + ` WHERE user_id = ? AND library_id = ? AND rel_path = ?`
		if table == "collection_items" {
			q = `SELECT COUNT(*) FROM collection_items i JOIN collections c ON c.id = i.collection_id
			      WHERE c.user_id = ? AND i.library_id = ? AND i.rel_path = ?`
		}
		var n int
		if err := f.c.db.QueryRowContext(f.ctx, q, user, f.lib, p).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	rep, err := f.c.RekeyPathState(f.ctx, nil, nil)
	if err != nil || len(rep.Failed) != 0 {
		t.Fatalf("re-key: %+v %v", rep, err)
	}
	n := len(rekeyedTables)
	if rep.Moved[rekeyFile] != 1 || rep.Rows != n || rep.Kept != 2*n {
		t.Errorf("report = %+v, want the path moved for ann's %d rows, dan's and zed's %d kept", rep, n, 2*n)
	}
	if len(rep.Skipped) != 1 || rep.Skipped[0].Why != RekeyGuardScope {
		t.Errorf("skipped = %+v, want the path named once by the scope guard", rep.Skipped)
	}
	for _, table := range rekeyedTables {
		to := rkBook
		if table == "favourites" {
			to = rkTrack1 // a track row's heart stays on the track, in the index's spelling
		}
		if rows(table, f.ann, rkTrack1D) != 0 || rows(table, f.ann, to) != 1 {
			t.Errorf("%s: ann's row did not move to %+q", table, to)
		}
		for _, u := range []int64{f.dan, zed} {
			if rows(table, u, rkTrack1D) != 1 || rows(table, u, to) != 0 {
				t.Errorf("%s: user %d's row moved, want it kept", table, u)
			}
		}
	}
	var kept int
	if err := f.c.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM path_rekeys WHERE reason = ? AND user_id IN (?, ?)
		AND from_path = ?`, RekeyGuardScope, f.dan, zed, rkTrack1D).Scan(&kept); err != nil || kept != 2*n {
		t.Errorf("kept rows logged %d (%v), want %d", kept, err, 2*n)
	}
	// A scan-end run keeps them again and logs nothing new.
	if again, err := f.c.RekeyLibrary(f.ctx, f.lib); err != nil || again.Rows != 0 || again.Kept == 0 {
		t.Errorf("scan-end run: %+v %v, want the kept rows kept, nothing moved", again, err)
	}
	var logged int
	if err := f.c.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM path_rekeys`).Scan(&logged); err != nil || logged != 3*n {
		t.Errorf("path_rekeys holds %d rows (%v), want %d: a re-run logs nothing again", logged, err, 3*n)
	}
}

// TestRekeyScopeKeptRowsFollowAGrant: the rows the scope guard kept move once
// their listener can reach the book, even when what is left on the path is a
// derived table's only (dan's day total: ann's progress, the path's one row a
// scan-end run looks for, has moved), at the next scan-end run.
func TestRekeyScopeKeptRowsFollowAGrant(t *testing.T) {
	t.Parallel()
	f := newRekeyFixture(t)
	f.exec(t, `INSERT INTO progress(user_id, library_id, rel_path, position, duration, updated_at) VALUES(?, ?, ?, 10, ?, ?)`,
		f.ann, f.lib, rkTrack3, rkBookLen, rkUpdated1)
	f.exec(t, `INSERT INTO listening_daily(day, user_id, library_id, rel_path, listened, sessions) VALUES('2026-01-01', ?, ?, ?, 10, 1)`,
		f.dan, f.lib, rkTrack3)
	rep, err := f.c.RekeyPathState(f.ctx, nil, nil)
	if err != nil || rep.Rows != 1 || rep.Kept != 1 {
		t.Fatalf("start run: %+v %v, want ann's row moved and dan's kept", rep, err)
	}
	if err := f.c.GrantWholeLibrary(f.ctx, f.dan, f.lib); err != nil {
		t.Fatal(err)
	}
	if rep, err := f.c.RekeyLibrary(f.ctx, f.lib); err != nil || rep.Rows != 1 || rep.Kept != 0 {
		t.Fatalf("scan-end run after the grant: %+v %v, want dan's row moved", rep, err)
	}
	if f.rowCount(t, "listening_daily", f.lib, rkTrack3) != 0 || f.rowCount(t, "listening_daily", f.lib, rkBook) != 1 {
		t.Error("dan's day total did not follow the grant to the book")
	}
}

// TestRekeyRespellsWhatStays: the rows a path in another spelling keeps (a
// disc folder's timeline rows, a navigation folder's favourite) move to the
// index's spelling of that path, keeping their own length, while its book-level
// rows go to the book; and a later disc's track holding the disc's own place
// (its length the disc's: the disc was its own book before the join) is left
// where it is, as a row on the book's timeline from the same disc moves.
func TestRekeyRespellsWhatStays(t *testing.T) {
	t.Parallel()
	f := newRekeyFixture(t)
	const (
		discs   = "Disques \u00c9t\u00e9" // NFC: joined from two discs
		disc1   = discs + "/CD 1"
		disc1D  = "Disques E\u0301te\u0301/CD 1" // disc 1 decomposed
		disc2   = discs + "/CD 2"
		nav     = "\u00c9crivains"  // a navigation folder, NFC
		navD    = "E\u0301crivains" // the same, decomposed
		discLen = 60.0
	)
	upsert(t, f.c, &Book{LibraryID: f.lib, RelPath: discs, IsFolder: true, Title: "Disques", Duration: 140,
		Files: []BookFile{{RelPath: disc1 + "/01.mp3", Duration: 40}, {RelPath: disc1 + "/02.mp3", Seq: 1, Duration: 40},
			{RelPath: disc2 + "/01.mp3", Seq: 2, Duration: 30}, {RelPath: disc2 + "/02.mp3", Seq: 3, Duration: 30}}})
	upsert(t, f.c, &Book{LibraryID: f.lib, RelPath: nav + "/Livre", IsFolder: true, Title: "Livre",
		Files: []BookFile{{RelPath: nav + "/Livre/01.mp3"}}})
	f.exec(t, `INSERT INTO progress(user_id, library_id, rel_path, position, duration, updated_at) VALUES(?, ?, ?, 20, 80, ?)`,
		f.eve, f.lib, disc1D, rkUpdated1)
	f.exec(t, `INSERT INTO ratings(user_id, library_id, rel_path, rating, note, created_at, updated_at) VALUES(?, ?, ?, 4, '', ?, ?)`,
		f.eve, f.lib, disc1D, rkUpdated1, rkUpdated1)
	f.exec(t, `INSERT INTO favourites(user_id, library_id, rel_path, created_at) VALUES(?, ?, ?, ?)`, f.adm, f.lib, navD, rkUpdated1)
	// bob's place on disc 2's second track is the disc's (length 60): kept.
	// ann's on disc 2's first track is the joined book's (length 140): moved.
	f.exec(t, `INSERT INTO progress(user_id, library_id, rel_path, position, duration, updated_at) VALUES(?, ?, ?, 45, ?, ?)`,
		f.bob, f.lib, disc2+"/02.mp3", discLen, rkUpdated1)
	f.exec(t, `INSERT INTO progress(user_id, library_id, rel_path, position, duration, updated_at) VALUES(?, ?, ?, 95, 140, ?)`,
		f.ann, f.lib, disc2+"/01.mp3", rkUpdated1)

	rep, err := f.c.RekeyLibrary(f.ctx, f.lib)
	if err != nil || len(rep.Failed) != 0 {
		t.Fatalf("re-key: %+v %v", rep, err)
	}
	if len(rep.Skipped) != 1 || rep.Skipped[0].Path != disc2+"/02.mp3" || rep.Skipped[0].Why != RekeyGuardTimeline {
		t.Errorf("skipped = %+v, want disc 2's second track by the timeline guard", rep.Skipped)
	}
	if p, _ := f.c.GetProgress(f.ctx, f.eve, Ref{LibraryID: f.lib, Path: disc1}); p == nil || p.Position != 20 || p.Duration != 80 {
		t.Errorf("eve's disc progress = %+v, want 20 of 80 on the disc in the index's spelling", p)
	}
	for _, k := range []struct {
		table, path string
		want        int
	}{
		{"progress", disc1D, 0}, {"ratings", disc1D, 0}, {"ratings", discs, 1},
		{"favourites", navD, 0}, {"favourites", nav, 1},
		{"progress", disc2 + "/02.mp3", 1}, {"progress", disc2 + "/01.mp3", 0}, {"progress", discs, 1},
	} {
		if got := f.rowCount(t, k.table, f.lib, k.path); got != k.want {
			t.Errorf("%s at %+q: %d rows, want %d", k.table, k.path, got, k.want)
		}
	}
	var logged int
	if err := f.c.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM path_rekeys`).Scan(&logged); err != nil || logged != rep.Rows || logged != 4 {
		t.Errorf("path_rekeys holds %d rows (%v), the report %d, want 4", logged, err, rep.Rows)
	}
}

// TestRekeyFolderMatchIsByteExact: a folder inside a book is matched byte for
// byte, case included (SQLite's LIKE ignores ASCII case): on a disk that keeps
// "cd 1" and "CD 1" apart, rows at a "cd 1" no indexed file lies under stay
// where they are rather than moving into the book holding "CD 1", and the
// timeline guard measures a disc by its own files only, so a place saved on
// "cd1"'s track with "cd1"'s length is kept, not read as the book's because
// "CD1" starts the book.
func TestRekeyFolderMatchIsByteExact(t *testing.T) {
	t.Parallel()
	f := newRekeyFixture(t)
	const (
		lower = rkJoined + "/cd 1" // the joined book has only "CD 1"
		mixed = "Mixed"
		cdLen = 80.0
	)
	upsert(t, f.c, &Book{LibraryID: f.lib, RelPath: mixed, IsFolder: true, Title: "Mixed", Duration: 180,
		Files: []BookFile{{RelPath: mixed + "/CD1/01.mp3", Duration: 100}, {RelPath: mixed + "/cd1/01.mp3", Seq: 1, Duration: 50},
			{RelPath: mixed + "/cd1/02.mp3", Seq: 2, Duration: 30}}})
	f.exec(t, `INSERT INTO progress(user_id, library_id, rel_path, position, duration, updated_at) VALUES(?, ?, ?, 20, 40, ?)`,
		f.eve, f.lib, lower, rkUpdated1)
	f.exec(t, `INSERT INTO ratings(user_id, library_id, rel_path, rating, note, created_at, updated_at) VALUES(?, ?, ?, 4, '', ?, ?)`,
		f.eve, f.lib, lower, rkUpdated1, rkUpdated1)
	f.exec(t, `INSERT INTO progress(user_id, library_id, rel_path, position, duration, updated_at) VALUES(?, ?, ?, 25, ?, ?)`,
		f.bob, f.lib, mixed+"/cd1/02.mp3", cdLen, rkUpdated1)

	rep, err := f.c.RekeyLibrary(f.ctx, f.lib)
	if err != nil || len(rep.Failed) != 0 || rep.Rows != 0 || rep.Moved[rekeyFolder] != 0 {
		t.Fatalf("re-key: %+v %v, want nothing moved", rep, err)
	}
	if len(rep.Skipped) != 1 || rep.Skipped[0].Path != mixed+"/cd1/02.mp3" || rep.Skipped[0].Why != RekeyGuardTimeline {
		t.Errorf("skipped = %+v, want cd1's second track by the timeline guard", rep.Skipped)
	}
	for _, k := range []struct {
		table, path string
		want        int
	}{
		{"progress", lower, 1}, {"ratings", lower, 1}, {"ratings", rkJoined, 0}, {"progress", rkJoined, 0},
		{"progress", mixed + "/cd1/02.mp3", 1}, {"progress", mixed, 0},
	} {
		if got := f.rowCount(t, k.table, f.lib, k.path); got != k.want {
			t.Errorf("%s at %q: %d rows, want %d", k.table, k.path, got, k.want)
		}
	}
}

// TestRekeyLibraryAfterIndexing: a row written while its book was not indexed
// stays (no book to move to) until a scan indexes the book; the scan-end run
// then moves it. A podcast library is never re-keyed.
func TestRekeyLibraryAfterIndexing(t *testing.T) {
	t.Parallel()
	f := newRekeyFixture(t)
	const fresh = "New/Book"
	f.exec(t, `INSERT INTO progress(user_id, library_id, rel_path, position, duration, updated_at) VALUES(?, ?, ?, 12, 0, ?)`,
		f.ann, f.lib, fresh+"/02.mp3", rkUpdated1)
	if rep, err := f.c.RekeyLibrary(f.ctx, f.lib); err != nil || len(rep.Moved) != 0 {
		t.Fatalf("before the book is indexed: %+v %v, want nothing moved", rep, err)
	}
	upsert(t, f.c, &Book{LibraryID: f.lib, RelPath: fresh, IsFolder: true, Title: "New", Duration: 30,
		Files: []BookFile{{RelPath: fresh + "/01.mp3", Duration: 10}, {RelPath: fresh + "/02.mp3", Seq: 1, Duration: 20}}})
	rep, err := f.c.RekeyLibrary(f.ctx, f.lib)
	if err != nil || rep.Moved[rekeyFile] != 1 || rep.Rows != 1 {
		t.Fatalf("after indexing: %+v %v, want the row moved", rep, err)
	}
	if p, _ := f.c.GetProgress(f.ctx, f.ann, Ref{LibraryID: f.lib, Path: fresh}); p == nil || p.Position != 12 || p.Duration != 30 {
		t.Errorf("progress on the new book = %+v, want 12 of 30", p)
	}
	if rep, err := f.c.RekeyLibrary(f.ctx, f.pod); err != nil || rep.Rows != 0 {
		t.Errorf("podcast library: %+v %v", rep, err)
	}
}

// TestRekeyOnUpgrade: as the start runs them on a database a v2.6 server left
// (no path keys yet, no re-key recorded), KeyPaths then RekeyPathState: the
// spelling of a book stored decomposed resolves through the keys KeyPaths has
// just written, and its rows move. A database with nothing to move changes
// nothing but the recorded version.
func TestRekeyOnUpgrade(t *testing.T) {
	t.Parallel()
	f := newRekeyFixture(t)
	f.seedEverything(t, f.ann, f.lib, rkAmelieC, 30, rkUpdated1)
	f.exec(t, `UPDATE books SET path_key = NULL`)
	f.exec(t, `DELETE FROM server_state WHERE key IN (?, ?)`, pathKeysKey, rekeyStateKey)
	if _, err := f.c.KeyPaths(f.ctx, nil); err != nil {
		t.Fatal(err)
	}
	rep, err := f.c.RekeyPathState(f.ctx, nil, nil)
	if err != nil || rep.Moved[rekeySpelling] != 1 {
		t.Fatalf("upgrade re-key: %+v %v, want the precomposed spelling moved", rep, err)
	}
	if f.rowCount(t, "progress", f.lib, rkAmelie) != 1 || f.rowCount(t, "progress", f.lib, rkAmelieC) != 0 {
		t.Error("the row did not reach the book's own spelling")
	}

	clean := newRekeyFixture(t)
	if _, err := clean.c.SaveProgress(clean.ctx, clean.ann, Progress{Ref: Ref{LibraryID: clean.lib, Path: rkBook}, Position: 5,
		UpdatedAt: rkUpdated1, Version: 1}); err != nil {
		t.Fatal(err)
	}
	rep, err = clean.c.RekeyPathState(clean.ctx, nil, nil)
	if err != nil || len(rep.Moved) != 0 || rep.Rows != 0 || len(rep.Skipped) != 0 {
		t.Fatalf("nothing to move: %+v %v", rep, err)
	}
	if clean.rowCount(t, "progress", clean.lib, rkBook) != 1 {
		t.Error("a book-path row changed")
	}
	if v, _ := getServerState[string](clean.ctx, clean.c.db, rekeyStateKey); v != pathKeysVersion {
		t.Errorf("version not recorded: %q", v)
	}
}

// refuseCarry is a trigger that refuses the carry's write of a bookmark onto
// its book, so every path holding one fails to move.
const refuseCarry = `CREATE TRIGGER refuse_carry BEFORE UPDATE OF rel_path ON bookmarks BEGIN SELECT RAISE(ABORT, 'refused'); END`

// TestRekeyRetriesOnlyFailedPaths: a path whose move fails is reported and
// nothing of it moves; the run is recorded all the same, with the path, so a
// later start looks at that path alone (never at every path again) until it
// moves, then at none.
func TestRekeyRetriesOnlyFailedPaths(t *testing.T) {
	t.Parallel()
	f := newRekeyFixture(t)
	f.seedEverything(t, f.ann, f.lib, rkTrack1, 30, rkUpdated1)
	f.exec(t, refuseCarry)
	failedPaths := func() []Ref {
		t.Helper()
		refs, err := getServerState[[]Ref](f.ctx, f.c.db, rekeyFailedKey)
		if err != nil {
			t.Fatal(err)
		}
		return refs
	}
	track1 := []Ref{{LibraryID: f.lib, Path: rkTrack1}}
	var told [][2]int
	rep, err := f.c.RekeyPathState(f.ctx, nil, func(done, total int) { told = append(told, [2]int{done, total}) })
	if err != nil || len(rep.Failed) != 1 || rep.Failed[0].Path != rkTrack1 || len(rep.Moved) != 0 || len(rep.Retried) != 0 {
		t.Fatalf("report = %+v %v, want track 1 failed", rep, err)
	}
	if !slices.Equal(told, [][2]int{{1, 1}}) {
		t.Errorf("progress = %v, want the one path of one", told)
	}
	var logged int
	if err := f.c.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM path_rekeys`).Scan(&logged); err != nil {
		t.Fatal(err)
	}
	if f.rowCount(t, "progress", f.lib, rkTrack1) != 1 || logged != 0 {
		t.Error("a failed move wrote something")
	}
	if v, _ := getServerState[string](f.ctx, f.c.db, rekeyStateKey); v != pathKeysVersion {
		t.Errorf("the run recorded %q, want its version (every path was looked at)", v)
	}
	if got := failedPaths(); !slices.Equal(got, track1) {
		t.Errorf("failed paths recorded = %+v, want track 1", got)
	}

	// A path that turns up later is a scan's to move, not a start's: the
	// start no longer looks at every path.
	f.exec(t, `INSERT INTO progress(user_id, library_id, rel_path, position, duration, version, device_id, updated_at, device_name)
		VALUES(?, ?, ?, 5, ?, 1, 'dev', ?, 'Phone')`, f.bob, f.lib, rkTrack2, rkBookLen, rkUpdated1)
	rep, err = f.c.RekeyPathState(f.ctx, nil, nil)
	if err != nil || !slices.Equal(rep.Retried, track1) || len(rep.Failed) != 1 || len(rep.Moved) != 0 {
		t.Fatalf("the next start, still refused: %+v %v, want track 1 tried again and failed", rep, err)
	}
	if got := failedPaths(); !slices.Equal(got, track1) {
		t.Errorf("failed paths after a second failure = %+v, want track 1 kept", got)
	}

	f.exec(t, `DROP TRIGGER refuse_carry`)
	rep, err = f.c.RekeyPathState(f.ctx, nil, nil)
	if err != nil || !slices.Equal(rep.Retried, track1) || rep.Moved[rekeyFile] != 1 || len(rep.Failed) != 0 {
		t.Fatalf("the start after: %+v %v, want track 1 moved", rep, err)
	}
	if f.rowCount(t, "progress", f.lib, rkTrack1) != 0 || f.rowCount(t, "bookmarks", f.lib, rkBook) != 1 {
		t.Error("track 1's rows did not reach the book")
	}
	if f.rowCount(t, "progress", f.lib, rkTrack2) != 1 {
		t.Error("a start that retries failed paths moved another path")
	}
	if got := failedPaths(); len(got) != 0 {
		t.Errorf("failed paths after the move = %+v, want none", got)
	}
	if rep, err := f.c.RekeyPathState(f.ctx, nil, nil); err != nil || len(rep.Retried) != 0 || len(rep.Moved) != 0 {
		t.Errorf("a start with nothing failed: %+v %v, want nothing looked at", rep, err)
	}
	// The scan-end run takes the path that turned up.
	if rep, err := f.c.RekeyLibrary(f.ctx, f.lib); err != nil || rep.Moved[rekeyFile] != 1 {
		t.Errorf("the scan-end run: %+v %v, want track 2 moved", rep, err)
	}
}

// TestRekeyRetryDropsWhatIsGone: a failed path whose rows are gone by the next
// start (the listener's account removed), or whose library is, is tried no
// more and leaves the list.
func TestRekeyRetryDropsWhatIsGone(t *testing.T) {
	t.Parallel()
	f := newRekeyFixture(t)
	f.seedEverything(t, f.ann, f.lib, rkTrack1, 30, rkUpdated1)
	f.exec(t, refuseCarry)
	if rep, err := f.c.RekeyPathState(f.ctx, nil, nil); err != nil || len(rep.Failed) != 1 {
		t.Fatalf("report = %+v %v, want track 1 failed", rep, err)
	}
	// A path of a library since removed joins the list; ann's rows go.
	gone := Ref{LibraryID: f.lib + 99, Path: "Gone/01.mp3"}
	if err := putServerState(f.ctx, f.c.db, rekeyFailedKey, []Ref{{LibraryID: f.lib, Path: rkTrack1}, gone}, f.c.stamp()); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `DELETE FROM users WHERE id = ?`, f.ann)
	rep, err := f.c.RekeyPathState(f.ctx, nil, nil)
	if err != nil || len(rep.Failed) != 0 || len(rep.Moved) != 0 || slices.Contains(rep.Retried, gone) {
		t.Fatalf("the next start: %+v %v, want nothing failed or moved, the removed library's path left out", rep, err)
	}
	if refs, err := getServerState[[]Ref](f.ctx, f.c.db, rekeyFailedKey); err != nil || len(refs) != 0 {
		t.Errorf("failed paths = %+v %v, want none", refs, err)
	}
}

// TestRekeyTooManyFailuresRunsAgain: a run where more paths failed than it
// records (the database's trouble, not a path's) is not recorded, so the next
// start runs over every path again; its progress counts each path once.
func TestRekeyTooManyFailuresRunsAgain(t *testing.T) {
	t.Parallel()
	f := newRekeyFixture(t)
	const book, n = "Big/Book", maxRekeyFailed + 1
	files := make([]BookFile, n)
	for i := range files {
		files[i] = BookFile{RelPath: fmt.Sprintf("%s/%04d.mp3", book, i), Seq: i, Duration: 10}
	}
	upsert(t, f.c, &Book{LibraryID: f.lib, RelPath: book, IsFolder: true, Title: "Big", Duration: 10 * n, Files: files})
	for _, file := range files {
		f.exec(t, `INSERT INTO bookmarks(user_id, library_id, rel_path, position, note, created_at) VALUES(?, ?, ?, 1, 'mark', ?)`,
			f.ann, f.lib, file.RelPath, rkUpdated1)
	}
	f.exec(t, refuseCarry)
	var told [][2]int
	rep, err := f.c.RekeyPathState(f.ctx, nil, func(done, total int) { told = append(told, [2]int{done, total}) })
	if err != nil || len(rep.Failed) != n {
		t.Fatalf("report: %d failed %v, want %d", len(rep.Failed), err, n)
	}
	if len(told) != n || told[0] != [2]int{1, n} || told[n-1] != [2]int{n, n} {
		t.Errorf("progress: %d calls, first %v, last %v, want 1..%d of %d", len(told), told[0], told[len(told)-1], n, n)
	}
	if v, _ := getServerState[string](f.ctx, f.c.db, rekeyStateKey); v != "" {
		t.Errorf("a run with %d failures recorded %q", n, v)
	}
	if refs, _ := getServerState[[]Ref](f.ctx, f.c.db, rekeyFailedKey); len(refs) != 0 {
		t.Errorf("a run with %d failures recorded %d paths", n, len(refs))
	}
	f.exec(t, `DROP TRIGGER refuse_carry`)
	if rep, err := f.c.RekeyPathState(f.ctx, nil, nil); err != nil || rep.Moved[rekeyFile] != n || len(rep.Retried) != 0 {
		t.Fatalf("the next start: %+v %v, want every path moved", rep.Moved, err)
	}
	if v, _ := getServerState[string](f.ctx, f.c.db, rekeyStateKey); v != pathKeysVersion {
		t.Errorf("the clean run recorded %q", v)
	}
}

// TestRekeySkipsUnansweringLibrary: the start's run leaves a library whose
// folder does not answer whole, failed paths included (Spelling's look at the
// disk can't keep a sibling folder of another spelling apart from the book),
// records it, and the next start runs over its every path once it answers.
func TestRekeySkipsUnansweringLibrary(t *testing.T) {
	t.Parallel()
	f := newRekeyFixture(t)
	f.seedEverything(t, f.ann, f.lib, rkTrack1, 30, rkUpdated1)
	f.seedEverything(t, f.bob, f.lib, rkBookNFD, 30, rkUpdated1)
	asleep := func(l Library) bool { return l.ID != f.lib }
	rep, err := f.c.RekeyPathState(f.ctx, asleep, nil)
	if err != nil || len(rep.Moved) != 0 || !slices.Equal(rep.Unreached, []int64{f.lib}) {
		t.Fatalf("report = %+v %v, want the library left whole", rep, err)
	}
	if f.rowCount(t, "progress", f.lib, rkTrack1) != 1 || f.rowCount(t, "progress", f.lib, rkBookNFD) != 1 {
		t.Fatal("rows moved in a library whose folder did not answer")
	}
	// Still asleep at the next start (a failed path of it too): left again.
	if err := putServerState(f.ctx, f.c.db, rekeyFailedKey, []Ref{{LibraryID: f.lib, Path: rkTrack1}}, f.c.stamp()); err != nil {
		t.Fatal(err)
	}
	rep, err = f.c.RekeyPathState(f.ctx, asleep, nil)
	if err != nil || len(rep.Moved) != 0 || len(rep.Retried) != 0 || !slices.Equal(rep.Unreached, []int64{f.lib}) {
		t.Fatalf("still asleep: %+v %v, want it left again", rep, err)
	}
	// Awake: every path of it, once.
	rep, err = f.c.RekeyPathState(f.ctx, nil, nil)
	if err != nil || rep.Moved[rekeyFile] != 1 || rep.Moved[rekeySpelling] != 1 || len(rep.Unreached) != 0 ||
		!slices.Equal(rep.Retried, []Ref{{LibraryID: f.lib}}) {
		t.Fatalf("awake: %+v %v, want its track and spelling moved", rep, err)
	}
	for _, key := range []string{rekeyUnreachedKey, rekeyFailedKey} {
		if v, err := getServerState[json.RawMessage](f.ctx, f.c.db, key); err != nil || v != nil {
			t.Errorf("%s = %s %v, want no row", key, v, err)
		}
	}
	if rep, err := f.c.RekeyPathState(f.ctx, nil, nil); err != nil || len(rep.Retried) != 0 || len(rep.Moved) != 0 {
		t.Errorf("the start after: %+v %v, want nothing", rep, err)
	}
}

// TestRekeyLibraryPassesByIdlePaths: a scan-end run looks again only at what
// may have changed since the last: a path it found nothing to do for (a stray
// track no book has) is passed by while its rows' tables and the index are as
// they were; a new path, a path with rows in another table, and every path once
// the index changes are looked at; a path the scope guard kept rows on is
// looked at every run (a grant since lets them follow).
func TestRekeyLibraryPassesByIdlePaths(t *testing.T) {
	t.Parallel()
	f := newRekeyFixture(t)
	f.c.memoRekeyFrom = 1
	var looked []string
	f.c.rekeyLooked = func(p string) { looked = append(looked, p) }
	stray := func(i int) string { return fmt.Sprintf("%s/stray-%d.mp3", rkNavBook, i) }
	for i := range 5 {
		f.exec(t, `INSERT INTO bookmarks(user_id, library_id, rel_path, position, note, created_at) VALUES(?, ?, ?, 1, '', ?)`,
			f.ann, f.lib, stray(i), rkUpdated1)
	}
	// dan reaches track 3 only: his progress there is kept, not moved.
	f.exec(t, `INSERT INTO progress(user_id, library_id, rel_path, position, duration, updated_at) VALUES(?, ?, ?, 10, ?, ?)`,
		f.dan, f.lib, rkTrack3, rkBookLen, rkUpdated1)
	run := func(want ...string) {
		t.Helper()
		looked = nil
		rep, err := f.c.RekeyLibrary(f.ctx, f.lib)
		if err != nil || len(rep.Failed) != 0 || rep.Kept != 1 {
			t.Fatalf("re-key: %+v %v, want dan's row kept", rep, err)
		}
		slices.Sort(looked)
		slices.Sort(want)
		if !slices.Equal(looked, want) {
			t.Fatalf("looked at %+q, want %+q", looked, want)
		}
	}
	all := []string{rkTrack3, stray(0), stray(1), stray(2), stray(3), stray(4)}
	run(all...)
	run(rkTrack3)
	f.exec(t, `INSERT INTO bookmarks(user_id, library_id, rel_path, position, note, created_at) VALUES(?, ?, ?, 1, '', ?)`,
		f.ann, f.lib, stray(5), rkUpdated1)
	run(rkTrack3, stray(5))
	f.exec(t, `INSERT INTO ratings(user_id, library_id, rel_path, rating, note, created_at, updated_at) VALUES(?, ?, ?, 4, '', ?, ?)`,
		f.ann, f.lib, stray(0), rkUpdated1, rkUpdated1)
	run(rkTrack3, stray(0))
	run(rkTrack3)
	// The index changes: a file of the book is added, which takes stray 1.
	upsert(t, f.c, &Book{LibraryID: f.lib, RelPath: rkNavBook, IsFolder: true, Title: "Livre",
		Files: []BookFile{{RelPath: rkNavBook + "/01.mp3"}, {RelPath: stray(1), Seq: 1}}})
	run(append(all, stray(5))...)
	if f.rowCount(t, "bookmarks", f.lib, stray(1)) != 0 || f.rowCount(t, "bookmarks", f.lib, rkNavBook) != 1 {
		t.Error("stray 1's bookmark did not move to the book once the book had the file")
	}
	run(rkTrack3)
	// A deleted library's memo goes with it.
	if err := f.c.DeleteLibrary(f.ctx, f.lib); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.c.rekeyMemos[f.lib]; ok {
		t.Error("the deleted library's re-key memo is still held")
	}
}

// TestRekeyStoppedIsNoFailure: a path whose look or move the server's stop
// ends is not reported failed (the start would record it for a retry, the log
// would warn): the run returns the stop, and the next run takes the path.
func TestRekeyStoppedIsNoFailure(t *testing.T) {
	t.Parallel()
	f := newRekeyFixture(t)
	f.exec(t, `INSERT INTO bookmarks(user_id, library_id, rel_path, position, note, created_at) VALUES(?, ?, ?, 1, '', ?)`,
		f.ann, f.lib, rkTrack2, rkUpdated1)
	stopped, stop := context.WithCancel(f.ctx)
	stop()
	var rep RekeyReport
	if idle := f.c.rekeyOne(stopped, f.lib, rkTrack2, carryTimeline, map[int64][]Scope{}, &rep); idle || len(rep.Failed) != 0 {
		t.Fatalf("a stopped path: idle %v, report %+v, want neither idle nor failed", idle, rep)
	}
	if rep, err := f.c.RekeyLibrary(f.ctx, f.lib); err != nil || rep.Rows != 1 || len(rep.Failed) != 0 {
		t.Fatalf("the next run: %+v %v, want the bookmark moved", rep, err)
	}
}

// A re-key carries an Audible account's place as a move does (moveCarry): the
// row under another spelling of a synced book is the same book's, so an
// unchanged place that wins the merge (mergeNewest: the newer save) stays the
// account's, never offered back to Audible over a rewind made in its app since.
// A newer place of the listener's own already on the book wins whole, with its
// device; and an Audible place already on the book that wins over a carried
// older save keeps the account's device (its place is unchanged).
func TestRekeyCarriesAnAudiblePlace(t *testing.T) {
	t.Parallel()
	const audible, name = "audible:acct0001", "Audible (US)"
	for _, tc := range []struct {
		name       string
		carried    string // the row under the decomposed spelling: its device
		carriedAt  string
		have       string // the row already on the book: its device ("" none)
		haveAt     string
		wantDevice string
		wantPos    float64
	}{
		{name: "onto no row", carried: audible, carriedAt: rkUpdated1, wantDevice: audible, wantPos: 80},
		{name: "onto an older save", carried: audible, carriedAt: rkUpdated2, have: "phone", haveAt: rkUpdated1,
			wantDevice: audible, wantPos: 80},
		{name: "onto a newer save of the listener's", carried: audible, carriedAt: rkUpdated1, have: "phone",
			haveAt: rkUpdated2, wantDevice: "phone", wantPos: 200},
		{name: "an older save onto Audible's newer place", carried: "phone", carriedAt: rkUpdated1, have: audible,
			haveAt: rkUpdated2, wantDevice: audible, wantPos: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRekeyFixture(t)
			// Inserted with its device as a plain one, then named the Audible
			// route's way (the guard, 0085, takes it off an inserted row).
			row := func(p, device string, pos float64, at string) {
				t.Helper()
				f.exec(t, `INSERT INTO progress(user_id, library_id, rel_path, position, duration, version, device_id, device_name, updated_at)
					VALUES(?, ?, ?, ?, ?, 3, 'dev', 'Phone', ?)`, f.ann, f.lib, p, pos, rkBookLen, at)
				dn := "Phone"
				if device == audible {
					dn = name
				}
				f.exec(t, `UPDATE progress SET device_id = ?, device_name = ? WHERE user_id = ? AND library_id = ? AND rel_path = ?`,
					device, dn, f.ann, f.lib, p)
			}
			row(rkBookNFD, tc.carried, 80, tc.carriedAt)
			if tc.have != "" {
				row(rkBook, tc.have, 200, tc.haveAt)
			}
			if rep, err := f.c.RekeyLibrary(f.ctx, f.lib); err != nil || rep.Rows != 1 {
				t.Fatalf("re-key: %+v %v, want the row moved", rep, err)
			}
			var device string
			var pos float64
			if err := f.c.db.QueryRowContext(f.ctx, `SELECT device_id, position FROM progress
			    WHERE user_id = ? AND library_id = ? AND rel_path = ?`, f.ann, f.lib, rkBook).Scan(&device, &pos); err != nil {
				t.Fatal(err)
			}
			if device != tc.wantDevice || pos != tc.wantPos {
				t.Fatalf("row on the book = %q at %v, want %q at %v", device, pos, tc.wantDevice, tc.wantPos)
			}
			if n := f.rowCount(t, "progress", f.lib, rkBookNFD); n != 0 {
				t.Fatalf("%d rows left under the other spelling", n)
			}
		})
	}
}
