-- Track W1 (Sync v2): the listening-event log. Every listen span, finish and
-- unfinish a player posts to POST /me/listening/sync, and every legacy PUT
-- .../progress (kind 'put'), is one row; progress, sessions and history are
-- derived from them. event_id is the client's id (put-<hex> for a PUT), unique
-- per user, so a replayed batch is answered "duplicate" and counted once.
--
-- Durable, path-keyed user state like progress: rel_path is the progress key
-- (the book's path, catalog's progressPath rule), deliberately NOT FK'd to the
-- rebuildable books index; a move or join carries it
-- (catalog.carryListeningState). A user or library delete purges it (cascade).
-- Positions are whole-book seconds after the file remap (fe#188); times are the
-- clock-corrected device times (finish/unfinish/put: started_at = ended_at = the
-- event's time) as fixed-width UTC milliseconds, so they compare as text.
-- base_version NULL = not sent. Rejected events are never stored. Rows older
-- than 120 days are pruned (catalog.PruneListeningEvents).
CREATE TABLE listening_events (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    event_id     TEXT NOT NULL,
    library_id   INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    rel_path     TEXT NOT NULL,
    kind         TEXT NOT NULL,
    device_id    TEXT NOT NULL DEFAULT '',
    token_id     INTEGER NOT NULL DEFAULT 0,
    from_pos     REAL NOT NULL DEFAULT 0,
    to_pos       REAL NOT NULL DEFAULT 0,
    speed        REAL NOT NULL DEFAULT 1,
    duration     REAL NOT NULL DEFAULT 0,
    started_at   TEXT NOT NULL,
    ended_at     TEXT NOT NULL,
    clock_shift  REAL NOT NULL DEFAULT 0,
    start        TEXT NOT NULL DEFAULT '',
    base_version INTEGER,
    status       TEXT NOT NULL,
    reason       TEXT NOT NULL DEFAULT '',
    received_at  TEXT NOT NULL,
    UNIQUE (user_id, event_id)
);
-- A move or join re-keys every listener's events on one path.
CREATE INDEX idx_listening_events_path ON listening_events(library_id, rel_path);
-- Retention deletes by age.
CREATE INDEX idx_listening_events_ended ON listening_events(ended_at);

-- A listen span extends the history row on its book that ended just before it
-- began (catalog's recordHistory looks it up by ended_at on the path).
CREATE INDEX idx_history_user_path_ended ON listening_history(user_id, library_id, rel_path, ended_at);

-- The progress row learns who saved it by name (the saving token's), when its
-- finish state last changed (a finish or unfinish older than that is "older"),
-- and the place a diverged event did not keep (other_place, all NULL = none;
-- a device whose base is at or after other_version had seen it, so its applied
-- listen clears it).
ALTER TABLE progress ADD COLUMN device_name TEXT NOT NULL DEFAULT '';
ALTER TABLE progress ADD COLUMN finish_changed_at TEXT;
ALTER TABLE progress ADD COLUMN other_position REAL;
ALTER TABLE progress ADD COLUMN other_device_id TEXT;
ALTER TABLE progress ADD COLUMN other_device_name TEXT;
ALTER TABLE progress ADD COLUMN other_at TEXT;
ALTER TABLE progress ADD COLUMN other_version INTEGER;
