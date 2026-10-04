-- Admin console Phase 3: scan history, scheduled scans, ignore rules and the
-- Health page's issues.

-- One row per library scan the job queue ran: when, why, what it found, and a
-- bounded log (JSON array of events; see library.RunEvent). A record of the index,
-- not durable user state, so it goes with its library. Retention is bounded per
-- library (catalog.FinishScanRun). finished_at is NULL while the scan runs; a row
-- still NULL at startup belonged to a server that stopped mid-scan.
CREATE TABLE scan_runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    library_id  INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    trigger     TEXT NOT NULL,            -- manual | schedule | startup | change
    started_by  INTEGER REFERENCES users(id) ON DELETE SET NULL,
    started_at  TEXT NOT NULL,
    finished_at TEXT,
    status      TEXT NOT NULL,            -- running | ok | partial | unavailable | failed | cancelled | interrupted
    books       INTEGER NOT NULL DEFAULT 0, -- books discovered on disk
    added       INTEGER NOT NULL DEFAULT 0,
    updated     INTEGER NOT NULL DEFAULT 0,
    moved       INTEGER NOT NULL DEFAULT 0,
    removed     INTEGER NOT NULL DEFAULT 0,
    errors      INTEGER NOT NULL DEFAULT 0,
    log         TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX idx_scan_runs_library ON scan_runs(library_id, id);

-- An admin's "ignore this" on a Health issue: path-keyed durable state with no FK
-- to the rebuildable books index (it survives a rebuild and moves with the book),
-- like folder_overrides. kind is one of catalog.IssueKinds.
CREATE TABLE issue_ignores (
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    path       TEXT NOT NULL,
    kind       TEXT NOT NULL,
    created_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (library_id, path, kind)
);

-- Scheduled scans ("" = none; see library.ParseSchedule) and ignore rules (one
-- pattern per line; see library.ParseIgnore). Ignore rules live in the database,
-- not in a file in the library folder, because the server never writes to the
-- library and its folders may be mounted read-only.
ALTER TABLE libraries ADD COLUMN scan_schedule TEXT NOT NULL DEFAULT '';
ALTER TABLE libraries ADD COLUMN ignore_patterns TEXT NOT NULL DEFAULT '';

-- What went wrong reading a book's files on its last indexing: a code (see
-- library.noteProblem), the library-relative file, and the tool's or the OS's
-- message; '' when nothing did.
ALTER TABLE books ADD COLUMN scan_error TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN scan_error_file TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN scan_error_detail TEXT NOT NULL DEFAULT '';

-- How many separate books a folder book's parts look like (their own titles, each
-- book-length): 0 = one book, >= 2 = the folder may hold several. NULL = not
-- checked yet: only folder books whose parts are all an hour or longer can be
-- suspect, so every other existing row is settled here and the scanner checks the
-- rest with a tag read.
ALTER TABLE books ADD COLUMN suspect_parts INTEGER;
UPDATE books SET suspect_parts = 0
 WHERE is_folder = 0
    OR (SELECT COUNT(*) FROM book_files bf WHERE bf.book_id = books.id) < 2
    OR EXISTS (SELECT 1 FROM book_files bf WHERE bf.book_id = books.id AND bf.duration < 3600);
