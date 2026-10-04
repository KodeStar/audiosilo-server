-- Admin console Phase 4a: listening sessions, devices and the Activity stats.

-- Which app a token belongs to, from the X-AudioSilo-Client header players send
-- ("AudioSilo/1.4.2 (ios)"; see auth.ParseClient). Empty until the token makes a
-- request carrying the header, which is how a client released before the header
-- reads as an unknown app. last_ip is the address of the token's newest request:
-- overwritten each time, never a history.
ALTER TABLE tokens ADD COLUMN client_app TEXT NOT NULL DEFAULT '';
ALTER TABLE tokens ADD COLUMN client_version TEXT NOT NULL DEFAULT '';
ALTER TABLE tokens ADD COLUMN client_platform TEXT NOT NULL DEFAULT '';
ALTER TABLE tokens ADD COLUMN last_ip TEXT NOT NULL DEFAULT '';

-- When a listener started and finished a book, for the admin's progress edits and
-- the Activity stats. Set by SaveProgress from here on (started_at on the first
-- save, finished_at when finished turns on, cleared when it turns off). Rows from
-- before this migration have no known start; a finished one takes its last save
-- as the finish, which is when a player saves the end of a book. updated_at is
-- the client's own RFC3339 (any offset, maybe fractions), so it is normalized to
-- the UTC-to-the-second form SaveProgress writes, which the stats compare as text.
ALTER TABLE progress ADD COLUMN started_at TEXT;
ALTER TABLE progress ADD COLUMN finished_at TEXT;
UPDATE progress SET finished_at = COALESCE(strftime('%Y-%m-%dT%H:%M:%SZ', updated_at), updated_at)
 WHERE finished = 1;

-- One row per listening session, derived on the server from the progress saves a
-- player makes every few seconds while playing (catalog.RecordHeartbeat): the same
-- token on the same book with no save for 10 minutes starts a new session. Durable
-- user state keyed by the path, with no FK to the rebuildable books index (it
-- moves with the book, like progress). token_id has no FK either: a session
-- outlives its token's sign-out, so the device and app are copied onto the row.
-- Times are UTC in a fixed-width RFC3339 form with milliseconds
-- (catalog.sessionTime), so they compare as strings.
-- listened is wall-clock seconds of playback (position advanced / speed, capped by
-- the time between saves), not book time. Raw rows are kept for a bounded time and
-- then rolled up into listening_daily (catalog.PruneSessions).
CREATE TABLE listening_sessions (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    library_id      INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    rel_path        TEXT NOT NULL,
    token_id        INTEGER NOT NULL DEFAULT 0,
    device_name     TEXT NOT NULL DEFAULT '',
    client_app      TEXT NOT NULL DEFAULT '',
    client_version  TEXT NOT NULL DEFAULT '',
    client_platform TEXT NOT NULL DEFAULT '',
    started_at      TEXT NOT NULL,
    last_at         TEXT NOT NULL,
    start_pos       REAL NOT NULL DEFAULT 0,
    end_pos         REAL NOT NULL DEFAULT 0,
    duration        REAL NOT NULL DEFAULT 0,
    speed           REAL NOT NULL DEFAULT 1,
    listened        REAL NOT NULL DEFAULT 0,
    codec           TEXT NOT NULL DEFAULT '',
    transcoded      INTEGER NOT NULL DEFAULT 0,
    finished        INTEGER NOT NULL DEFAULT 0
);
-- The heartbeat's lookup of a device's open session on a book (every progress
-- save, inside the write transaction).
CREATE INDEX idx_sessions_token_book ON listening_sessions(token_id, library_id, rel_path, last_at);
CREATE INDEX idx_sessions_last ON listening_sessions(last_at);
CREATE INDEX idx_sessions_user ON listening_sessions(user_id, id);
CREATE INDEX idx_sessions_path ON listening_sessions(library_id, rel_path);

-- Sessions older than the raw retention, summed per local day, listener and book.
-- What survives is how long someone listened to which book on which day; the
-- device, app, time of day and playback mode go with the raw row. No primary key:
-- a book that moves onto a path that already has rows just adds rows, and every
-- reader sums.
CREATE TABLE listening_daily (
    day        TEXT NOT NULL,              -- YYYY-MM-DD, server time
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    rel_path   TEXT NOT NULL,
    listened   REAL NOT NULL DEFAULT 0,
    sessions   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_daily_day ON listening_daily(day);
CREATE INDEX idx_daily_user ON listening_daily(user_id, day);
CREATE INDEX idx_daily_path ON listening_daily(library_id, rel_path);
