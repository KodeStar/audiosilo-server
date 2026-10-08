-- Listening imported from another server (v1: Audiobookshelf), so someone moving
-- to AudioSilo keeps their history: the admin fetches a person's ABS listening,
-- reviews what matched, and applies it (internal/importer).
--
-- What an import writes is ordinary listening state, marked with the import's id
-- so it can be taken back out (undo) or replaced (re-import) without touching
-- anything else:
--
-- 1. listening_sessions rows, one per ABS session on a matched book, marked
--    backfilled like 0021's (the ABS device and app are kept on the row, but the
--    playback mode was never recorded, so they stay out of those breakdowns).
--    Only sessions that started before the import's cutoff (by default the
--    person's first listening recorded here) are imported, so nothing is counted
--    twice. A session whose last save is already older than the session
--    retention (activity.session_days) is written straight into the day totals
--    retention would roll it up into (2.). Their ids are ordinary (newer than
--    every live one), so the session lists order by when a session started
--    (indexes below), not by id.
-- 2. listening_daily rows: one estimated row per book where ABS's position is
--    further than its sessions explain (0021's semantics: 5 minutes or more, in
--    totals and tops, never in a day), and the day totals of imported sessions
--    past the retention: written so at the apply, or later by PruneSessions
--    (it keeps their import_id).
-- 3. bookmarks, deduplicated against the ones the person already has. The note
--    the import wrote is kept with it (import_note), so an undo leaves a bookmark
--    the person has since edited or labelled.
-- 4. listening_history rows: one player-style span per imported session (from
--    its start to its end position, over its start and its end), so the book's
--    History and the Journal show the imported listening as well.
-- 5. progress, fill-only: never rewound, never un-finished. The row as it was
--    before (or its absence) is kept in import_progress_prior with the row the
--    import wrote, so an undo restores it only while nobody has changed it since.
--
-- import_id 0 is everything recorded here. The ABS credentials are never stored:
-- the token lives only in the memory of the fetch that uses it.

-- One import: one ABS user's history fetched for one AudioSilo user. status:
-- fetching | review | applying | applied | failed | undone. A fetch a stopped
-- server left is failed (error_code interrupted); an apply goes back to review
-- (it is one transaction, so nothing of it was written). summary and unmatched
-- are what the review shows (JSON).
CREATE TABLE imports (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source      TEXT NOT NULL,             -- abs
    source_url  TEXT NOT NULL,             -- as the admin entered it (never with credentials)
    source_user TEXT NOT NULL DEFAULT '',  -- the ABS username
    source_id   TEXT NOT NULL DEFAULT '',  -- the ABS user id
    status      TEXT NOT NULL,
    cutoff      TEXT,                      -- sessions starting at or after it are skipped; NULL = none
    created_at  TEXT NOT NULL,
    applied_at  TEXT,
    summary     TEXT,                      -- NULL until planned
    unmatched   TEXT NOT NULL DEFAULT '[]',
    error       TEXT NOT NULL DEFAULT '',
    error_code  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_imports_user ON imports(user_id, id);

-- An import's fetched history (gzipped JSON), so a review's cutoff change and
-- the apply never fetch again. Its own table, so reading the imports never walks
-- a large blob's overflow pages; deleted once the import is applied (nothing
-- reads it after the review), undone or deleted.
CREATE TABLE import_payloads (
    import_id INTEGER PRIMARY KEY REFERENCES imports(id) ON DELETE CASCADE,
    payload   BLOB NOT NULL
);

ALTER TABLE listening_sessions ADD COLUMN import_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE listening_daily ADD COLUMN import_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE bookmarks ADD COLUMN import_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE bookmarks ADD COLUMN import_note TEXT NOT NULL DEFAULT '';
ALTER TABLE listening_history ADD COLUMN import_id INTEGER NOT NULL DEFAULT 0;
-- An undo or re-import deletes by import_id; live rows (0) are never looked up by it.
CREATE INDEX idx_sessions_import ON listening_sessions(import_id) WHERE import_id <> 0;
CREATE INDEX idx_daily_import ON listening_daily(import_id) WHERE import_id <> 0;
CREATE INDEX idx_bookmarks_import ON bookmarks(import_id) WHERE import_id <> 0;
CREATE INDEX idx_history_import ON listening_history(import_id) WHERE import_id <> 0;

-- The session lists (catalog.ListSessions) order by start, newest first: an
-- imported session is old but has a new id. Everyone's sessions, and one
-- person's; one book's are few and sort after idx_sessions_path finds them. The
-- per-user list took (user_id, id) until now.
CREATE INDEX idx_sessions_started ON listening_sessions(started_at);
CREATE INDEX idx_sessions_user_started ON listening_sessions(user_id, started_at);

-- A progress row an import changed: prior is the row before (JSON; NULL = there
-- was none), wrote the row the import left. Path-keyed like progress, so a move
-- or join carries it (catalog.carryListeningState).
CREATE TABLE import_progress_prior (
    import_id  INTEGER NOT NULL REFERENCES imports(id) ON DELETE CASCADE,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    rel_path   TEXT NOT NULL,
    prior      TEXT,
    wrote      TEXT NOT NULL,
    PRIMARY KEY (import_id, library_id, rel_path)
);
CREATE INDEX idx_import_prior_path ON import_progress_prior(library_id, rel_path);
