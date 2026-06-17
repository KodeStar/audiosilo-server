-- AudioSilo initial schema.
-- The filesystem is the source of truth for audio content; these tables hold
-- relational/derived state (users, sessions, the metadata index, listening state)
-- and can be rebuilt from a rescan.

CREATE TABLE users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,          -- argon2id encoded hash
    role          TEXT NOT NULL DEFAULT 'user', -- 'admin' | 'user'
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

-- Opaque bearer tokens (sessions) and short-lived pairing tokens. Only the
-- hash is stored so a database leak does not expose live credentials.
CREATE TABLE tokens (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash  TEXT NOT NULL UNIQUE,
    kind        TEXT NOT NULL DEFAULT 'session', -- 'session' | 'pairing'
    device_name TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    last_seen   TEXT,
    expires_at  TEXT,                    -- NULL = no expiry
    revoked     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_tokens_user ON tokens(user_id);

-- Redeemable auth codes used by the "enter your auth code" connect flow.
CREATE TABLE auth_codes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    code_hash  TEXT NOT NULL UNIQUE,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    label      TEXT NOT NULL DEFAULT '',
    max_uses   INTEGER NOT NULL DEFAULT 0, -- 0 = unlimited
    uses       INTEGER NOT NULL DEFAULT 0,
    expires_at TEXT,
    created_at TEXT NOT NULL
);

CREATE TABLE libraries (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT NOT NULL UNIQUE,
    root         TEXT NOT NULL,
    layout       TEXT NOT NULL DEFAULT 'books_in_folder',
    default_view TEXT NOT NULL DEFAULT 'hybrid', -- 'filesystem' | 'computed' | 'hybrid'
    created_at   TEXT NOT NULL
);

CREATE TABLE user_library_access (
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    read_only  INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (user_id, library_id)
);

-- A book is either a single file (rel_path -> file) or a folder of files
-- (is_folder = 1, with rows in book_files).
CREATE TABLE books (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    library_id   INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    rel_path     TEXT NOT NULL,          -- relative to the library root
    is_folder    INTEGER NOT NULL DEFAULT 0,
    title        TEXT NOT NULL DEFAULT '',
    author       TEXT NOT NULL DEFAULT '',
    series       TEXT NOT NULL DEFAULT '',
    series_index REAL NOT NULL DEFAULT 0, -- 0 means "no series index"
    narrator     TEXT NOT NULL DEFAULT '',
    duration     REAL NOT NULL DEFAULT 0, -- seconds
    asin         TEXT NOT NULL DEFAULT '',
    isbn         TEXT NOT NULL DEFAULT '',
    cover_path   TEXT NOT NULL DEFAULT '',
    format       TEXT NOT NULL DEFAULT '',
    size         INTEGER NOT NULL DEFAULT 0,
    mtime        INTEGER NOT NULL DEFAULT 0,
    content_hash TEXT NOT NULL DEFAULT '',
    indexed_at   TEXT NOT NULL,
    UNIQUE (library_id, rel_path)
);
CREATE INDEX idx_books_author  ON books(library_id, author);
CREATE INDEX idx_books_series  ON books(library_id, series, series_index);
CREATE INDEX idx_books_sort    ON books(library_id, author, series, series_index, title, id);

CREATE TABLE book_files (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    book_id  INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    rel_path TEXT NOT NULL,
    seq      INTEGER NOT NULL DEFAULT 0,
    duration REAL NOT NULL DEFAULT 0,
    format   TEXT NOT NULL DEFAULT '',
    size     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_book_files_book ON book_files(book_id, seq);

CREATE TABLE chapters (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    book_id  INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    idx      INTEGER NOT NULL,
    title    TEXT NOT NULL DEFAULT '',
    start    REAL NOT NULL DEFAULT 0,
    "end"    REAL NOT NULL DEFAULT 0
);
CREATE INDEX idx_chapters_book ON chapters(book_id, idx);

-- Per-user listening state.
CREATE TABLE progress (
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    book_id        INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    position       REAL NOT NULL DEFAULT 0,
    duration       REAL NOT NULL DEFAULT 0,
    finished       INTEGER NOT NULL DEFAULT 0,
    playback_speed REAL NOT NULL DEFAULT 1.0,
    version        INTEGER NOT NULL DEFAULT 1, -- monotonic, breaks updated_at ties
    device_id      TEXT NOT NULL DEFAULT '',
    updated_at     TEXT NOT NULL,
    PRIMARY KEY (user_id, book_id)
);

CREATE TABLE bookmarks (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    book_id    INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    position   REAL NOT NULL DEFAULT 0,
    note       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX idx_bookmarks_user_book ON bookmarks(user_id, book_id);

CREATE TABLE notes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    book_id    INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    position   REAL NOT NULL DEFAULT 0,
    body       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX idx_notes_user_book ON notes(user_id, book_id);

CREATE TABLE listening_history (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    book_id    INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    from_pos   REAL NOT NULL DEFAULT 0,
    to_pos     REAL NOT NULL DEFAULT 0,
    started_at TEXT NOT NULL,
    ended_at   TEXT NOT NULL
);
CREATE INDEX idx_history_user ON listening_history(user_id, book_id, started_at);

-- Full-text search over the catalog. A standalone FTS5 table (its own storage)
-- keyed by the book id via rowid, kept in sync by the application on every book
-- upsert/delete. Standalone (rather than external-content) storage keeps the
-- sync code to a simple delete-then-insert by rowid.
CREATE VIRTUAL TABLE books_fts USING fts5(title, author, series, narrator);
