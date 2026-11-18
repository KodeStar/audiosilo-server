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
	// dan reaches only track 3, and eve shares the path: the scope guard keeps
	// both (the path moves whole or not at all).
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
	for _, p := range []string{rkTrack1, rkBookNFD, rkAmelieC, rkTrack1D, rkDisc1} {
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

	rep, err := c.RekeyPathState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Failed) != 0 {
		t.Fatalf("failed: %+v", rep.Failed)
	}
	if rep.Moved[rekeySpelling] != 2 || rep.Moved[rekeyFile] != 2 || rep.Moved[rekeyFolder] != 1 {
		t.Errorf("moved = %v, want spelling 2 (bob's, adm's book), file 2 (ann's, adm's track), folder 1 (eve's disc)", rep.Moved)
	}
	guards := map[string]string{}
	for _, s := range rep.Skipped {
		guards[s.Path] = s.Why
	}
	if guards[rkTrack3] != RekeyGuardScope || guards[rkTrack2] != RekeyGuardTimeline || len(guards) != 2 {
		t.Errorf("skipped = %+v, want track 3 by scope, track 2 by timeline", rep.Skipped)
	}

	// The moves, by table: what stays and where the rest went.
	stays := map[at]bool{
		{"favourites", rkTrack1}:  true, // a track row's heart stays (file)
		{"favourites", rkTrack1D}: true,
	}
	for _, table := range rekeyedTables {
		if i := slices.IndexFunc(rekeyTables, func(rt rekeyTable) bool { return rt.name == table }); rekeyTables[i].set != carryBookLevel {
			stays[at{table, rkDisc1}] = true // a disc's timeline and its heart (folder)
		}
	}
	for k, rows := range before {
		got := f.rowCount(t, k.table, f.lib, k.path)
		if want := map[bool]int{true: len(rows), false: 0}[stays[k]]; got != want {
			t.Errorf("%s at %+q: %d rows left, want %d", k.table, k.path, got, want)
		}
	}
	for _, k := range []at{{"progress", rkTrack2}, {"progress", rkTrack3}, {"bookmarks", rkTrack3}, {"favourites", rkNav},
		{"progress", rkOrphan}} {
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
	var logged int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM path_rekeys`).Scan(&logged); err != nil {
		t.Fatal(err)
	}
	if logged != rep.Rows {
		t.Errorf("path_rekeys holds %d rows, the report %d", logged, rep.Rows)
	}
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
		`SELECT table_name, from_path, row_json, user_id FROM path_rekeys`)
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
	again, err := c.RekeyPathState(ctx)
	if err != nil || len(again.Moved) != 0 || len(again.Skipped) != 0 || again.Rows != 0 {
		t.Errorf("second start: %+v %v, want nothing", again, err)
	}
	f.exec(t, `DELETE FROM server_state WHERE key = ?`, rekeyStateKey)
	rerun, err := c.RekeyPathState(ctx)
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
	rep, err := f.c.RekeyPathState(f.ctx)
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
	rep, err = clean.c.RekeyPathState(clean.ctx)
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

// TestRekeyFailureLeavesVersionUnrecorded: a path whose move fails is reported,
// nothing of it moves, and the start's run is not recorded, so the next start
// tries it again.
func TestRekeyFailureLeavesVersionUnrecorded(t *testing.T) {
	t.Parallel()
	f := newRekeyFixture(t)
	f.seedEverything(t, f.ann, f.lib, rkTrack1, 30, rkUpdated1)
	// A trigger that refuses the carry's write onto the book.
	f.exec(t, `CREATE TRIGGER refuse_carry BEFORE UPDATE OF rel_path ON bookmarks BEGIN SELECT RAISE(ABORT, 'refused'); END`)
	rep, err := f.c.RekeyPathState(f.ctx)
	if err != nil || len(rep.Failed) != 1 || rep.Failed[0].Path != rkTrack1 || len(rep.Moved) != 0 {
		t.Fatalf("report = %+v %v, want track 1 failed", rep, err)
	}
	var logged int
	if err := f.c.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM path_rekeys`).Scan(&logged); err != nil {
		t.Fatal(err)
	}
	if f.rowCount(t, "progress", f.lib, rkTrack1) != 1 || logged != 0 {
		t.Error("a failed move wrote something")
	}
	if v, _ := getServerState[string](f.ctx, f.c.db, rekeyStateKey); v != "" {
		t.Errorf("a failed run recorded %q", v)
	}
	f.exec(t, `DROP TRIGGER refuse_carry`)
	if rep, err := f.c.RekeyPathState(f.ctx); err != nil || rep.Moved[rekeyFile] != 1 {
		t.Fatalf("the next start: %+v %v, want the path moved", rep, err)
	}
}
