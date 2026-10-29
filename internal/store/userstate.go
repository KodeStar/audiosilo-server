package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"testing"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// User-state writes (server#258). Listening writes all the time: a progress
// save and its session every few seconds per listener, a token's last-seen
// touch, history, bookmarks, the Bridge's heartbeat. None of them changes what
// the content caches answer (the player's lists, the admin's book pages and
// Health, the library and token caches), bar a touched token's own row in the
// token cache, which the touch drops itself (auth.Service.resolve), so they go
// through ExecUserState or WithUserStateTx, which leave the content generation
// (ContentGeneration) where it was, and those caches warm.
//
// The classification is checked, not trusted: while a user-state write runs,
// SQLite's pre-update hook sees every row it changes (its triggers' and
// foreign-key actions' too), and the write moves the content generation like
// any other when one of them is outside its table's rule in userStateTables.
// So a misfiled write can only empty the caches, never leave a stale answer in
// them. A change to a table the list doesn't name at all is a misfiling:
// logged, and a panic in a test binary, so a test exercising the path fails.

// tableRule is what a user-state write may change in one table and stay user
// state.
type tableRule struct {
	// rows: inserting and deleting rows is user state. Without it only an
	// UPDATE is, and a new or removed row moves the content generation.
	rows bool
	// free, when not nil, are the only columns an UPDATE may change (by value)
	// and stay user state; nil is any column.
	free []string
	// watched are the column indexes outside free, in the table's order
	// (userStateRules fills it from the schema); nil when free is.
	watched []int
}

// userStateTables are the tables a user-state write may change, each with its
// rule. User state here is what no content cache reads, not the data model's
// durable per-user state: the community metadata cache (meta_cache, derived)
// and the console's bell (server_events) are listed, favourites and goals are
// not. No content cache reads what a rule lets through, bar the token cache,
// whose touch drops the row it changed; a new cache that would needs a key of
// its own, or that table out of the list (CONVENTIONS.md, Content generation).
var userStateTables = map[string]tableRule{
	// A progress row's fields are no list's, but a new, removed or re-keyed row
	// changes the Health summary's duplicate ranking (how many people listen to
	// each copy: catalog.duplicateSets), so its key (user_id, library_id,
	// rel_path) is watched.
	"progress": {free: []string{"position", "duration", "finished", "playback_speed", "version", "device_id",
		"updated_at", "started_at", "finished_at"}},
	"listening_sessions": {rows: true},
	"listening_history":  {rows: true},
	"bookmarks":          {rows: true},
	"notes":              {rows: true},
	"ratings":            {rows: true},
	"up_next":            {rows: true},
	"reading_progress":   {rows: true},
	"server_events":      {rows: true},
	"meta_cache":         {rows: true},
	// A token's touch (auth.Service.resolve): the token cache holds the row,
	// and drops the touched one itself. Any other change (a revoke, a new
	// token) moves the generation.
	"tokens": {free: []string{"last_seen", "last_ip", "client_app", "client_version", "client_platform"}},
	// A Bridge's heartbeat with the status it last sent (bridge.SaveHeartbeat).
	"bridge_status": {free: []string{"heartbeat_at"}},
	// A job report that only renews the lease (bridge.UpdateHeld).
	"bridge_jobs": {free: []string{"lease_until", "updated_at"}},
}

// userStateRules is userStateTables with each column rule's watched columns,
// read from db's schema (PRAGMA table_info: the column order the pre-update
// hook's values come in): every column the rule doesn't free. A schema a newer
// server changed (Open still starts on one: a rollback) only ever makes more
// writes content: a column rule's table that is gone is left out (a write to it
// would be a misfiling), and a column it frees that is gone frees nothing.
// TestUserStateRulesMatchTheSchema holds the rules to this server's schema.
func userStateRules(ctx context.Context, db *sql.DB) (map[string]tableRule, error) {
	out := make(map[string]tableRule, len(userStateTables))
	for table, rule := range userStateTables {
		if rule.free != nil {
			cols, err := tableColumns(ctx, db, table)
			if err != nil {
				return nil, fmt.Errorf("user-state rule for %s: %w", table, err)
			}
			if len(cols) == 0 {
				continue
			}
			for i, c := range cols {
				if !slices.Contains(rule.free, c) {
					rule.watched = append(rule.watched, i)
				}
			}
		}
		out[table] = rule
	}
	return out, nil
}

// tableColumns lists table's columns in their order, none for a table that
// doesn't exist.
func tableColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

// ExecUserState is ExecContext for a statement that writes only user state
// (userStateTables): it moves the content generation only when the statement
// changed something outside its table's rule.
func (db *DB) ExecUserState(ctx context.Context, query string, args ...any) (sql.Result, error) {
	var res sql.Result
	err := db.userStateWrite(ctx, "", time.Now(), func(conn *sql.Conn) error {
		var err error
		res, err = conn.ExecContext(ctx, query, args...)
		return err
	})
	return res, err
}

// WithUserStateTx is WithTx for a transaction that writes only user state
// (userStateTables): it moves the content generation only when it changed
// something outside a table's rule (a rolled-back one too: the check is on the
// changes it made, not on what committed). name labels it in the
// slow-transaction log.
func (db *DB) WithUserStateTx(ctx context.Context, name string, fn func(*sql.Tx) error) error {
	start := time.Now()
	return db.userStateWrite(ctx, name, start, func(conn *sql.Conn) error {
		return db.withTx(ctx, name, start, conn, false, fn)
	})
}

// userStateWrite runs write on the writer's one connection with the pre-update
// hook watching it, and moves the content generation afterwards when the hook
// saw a change outside the rules, or could not be set. name and start, when
// name is set, log a wait for the writer that timed out, as WithTx logs it.
func (db *DB) userStateWrite(ctx context.Context, name string, start time.Time, write func(*sql.Conn) error) error {
	conn, err := db.writer.Conn(ctx)
	if err != nil {
		if name != "" {
			db.logSlow(name, start, err)
		}
		return err
	}
	defer func() { _ = conn.Close() }()
	w := &userStateWatch{rules: db.userState}
	// Deferred before the unset, so it runs after it: once the statement or the
	// transaction finished.
	defer func() {
		if w.content {
			db.content.Add(1)
		}
		if w.misfiled != "" {
			db.misfiled(w.misfiled)
		}
	}()
	if w.set(conn) {
		defer w.unset(conn)
	} else {
		w.content = true // unwatched: a write like any other
	}
	return write(conn)
}

// misfiled reports a user-state write that changed table, which no rule names:
// to onMisfiled (tests), else a panic in a test binary (the write has finished
// and moved the content generation), else a warning.
func (db *DB) misfiled(table string) {
	if db.onMisfiled != nil {
		db.onMisfiled(table)
		return
	}
	if testing.Testing() {
		panic(fmt.Sprintf("store: a user-state write changed %s, which userStateTables does not list", table))
	}
	db.log.Warn("a user-state write changed a table outside its list; the content caches were emptied", "table", table)
}

// userStateWatch is one user-state write's pre-update hook and what it saw.
// The hook runs on the goroutine running the write, which holds the writer's
// only connection, so nothing else reads or writes it meanwhile.
type userStateWatch struct {
	rules map[string]tableRule
	// content: a change outside the rules (the content generation moves).
	content bool
	// misfiled is a table the rules don't name that the write changed.
	misfiled string
}

// set installs w's hook on conn, false when it couldn't be.
func (w *userStateWatch) set(conn *sql.Conn) bool {
	set := false
	_ = conn.Raw(func(dc any) error {
		if h, ok := dc.(sqlite.HookRegisterer); ok {
			h.RegisterPreUpdateHook(w.change)
			set = true
		}
		return nil
	})
	return set
}

// unset removes w's hook from conn (a connection closed meanwhile dropped it).
func (w *userStateWatch) unset(conn *sql.Conn) {
	_ = conn.Raw(func(dc any) error {
		dc.(sqlite.HookRegisterer).RegisterPreUpdateHook(nil)
		return nil
	})
}

// change is the pre-update hook: one row about to be inserted, updated or
// deleted.
func (w *userStateWatch) change(d sqlite.SQLitePreUpdateData) {
	rule, ok := w.rules[d.TableName]
	switch {
	case !ok:
		w.content = true
		if w.misfiled == "" {
			w.misfiled = d.TableName
		}
	case w.content: // decided: only a misfiling is still worth seeing
	case d.Op != sqlite3.SQLITE_UPDATE:
		w.content = !rule.rows
	case d.OldRowID != d.NewRowID:
		w.content = true
	case rule.watched != nil:
		w.content = watchedChanged(&d, rule.watched)
	}
}

// watchedChanged reports whether an UPDATE changes any of the watched columns'
// values (or the values can't be read).
func watchedChanged(d *sqlite.SQLitePreUpdateData, watched []int) bool {
	n := d.Count()
	// Old and New store each value in the slice they are handed (they assign
	// through &dest[i]), so the slices themselves end up holding the row.
	before, after := make([]any, n), make([]any, n)
	if d.Old(before...) != nil || d.New(after...) != nil {
		return true
	}
	for _, i := range watched {
		if i >= n || !sameValue(before[i], after[i]) {
			return true
		}
	}
	return false
}

// sameValue reports whether two column values (nil, int64, float64, string,
// []byte) are the same. Only two []byte would panic on ==; any other pair of
// different types is simply unequal.
func sameValue(a, b any) bool {
	if ab, ok := a.([]byte); ok {
		bb, ok := b.([]byte)
		return ok && bytes.Equal(ab, bb)
	}
	return a == b
}
