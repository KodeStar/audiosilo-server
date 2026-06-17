-- Re-base identity onto the filesystem path and replace whole-library grants
-- with filesystem-based shares.
--
-- Durable user state (progress, bookmarks, notes, history) is re-keyed to
-- (user_id, library_id, rel_path) and decoupled from the rebuildable index
-- (no FK to books), so it survives a DB rebuild, re-tagging, and being recorded
-- before a scan reaches the file. The `content_hash` column on books is reused
-- as the move-detection fingerprint. Pre-1.0: these tables hold no durable data
-- yet, so they are recreated rather than migrated in place.

DROP TABLE IF EXISTS progress;
CREATE TABLE progress (
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    library_id     INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    rel_path       TEXT NOT NULL,
    position       REAL NOT NULL DEFAULT 0,
    duration       REAL NOT NULL DEFAULT 0,
    finished       INTEGER NOT NULL DEFAULT 0,
    playback_speed REAL NOT NULL DEFAULT 1.0,
    version        INTEGER NOT NULL DEFAULT 1,
    device_id      TEXT NOT NULL DEFAULT '',
    updated_at     TEXT NOT NULL,
    PRIMARY KEY (user_id, library_id, rel_path)
);

DROP TABLE IF EXISTS bookmarks;
CREATE TABLE bookmarks (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    rel_path   TEXT NOT NULL,
    position   REAL NOT NULL DEFAULT 0,
    note       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX idx_bookmarks_user_path ON bookmarks(user_id, library_id, rel_path);

DROP TABLE IF EXISTS notes;
CREATE TABLE notes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    rel_path   TEXT NOT NULL,
    position   REAL NOT NULL DEFAULT 0,
    body       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX idx_notes_user_path ON notes(user_id, library_id, rel_path);

DROP TABLE IF EXISTS listening_history;
CREATE TABLE listening_history (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    rel_path   TEXT NOT NULL,
    from_pos   REAL NOT NULL DEFAULT 0,
    to_pos     REAL NOT NULL DEFAULT 0,
    started_at TEXT NOT NULL,
    ended_at   TEXT NOT NULL
);
CREATE INDEX idx_history_user_path ON listening_history(user_id, library_id, rel_path, started_at);

-- Chapters/parts carry the file path so playback is purely path-based.
ALTER TABLE chapters ADD COLUMN file_path TEXT NOT NULL DEFAULT '';

-- Filesystem-based shares replace whole-library grants. A share is a named set
-- of path rules; a rule's path is any node in the tree, with "" meaning the
-- whole library.
DROP TABLE IF EXISTS user_library_access;

CREATE TABLE shares (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    read_only   INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL
);

CREATE TABLE share_paths (
    share_id   INTEGER NOT NULL REFERENCES shares(id) ON DELETE CASCADE,
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    path       TEXT NOT NULL DEFAULT '',   -- "" = whole library
    PRIMARY KEY (share_id, library_id, path)
);
CREATE INDEX idx_share_paths_library ON share_paths(library_id);

CREATE TABLE user_share_access (
    user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    share_id INTEGER NOT NULL REFERENCES shares(id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, share_id)
);
