-- Track U1: upcoming releases. A background job (internal/upcoming) reads the
-- community metadata for the series the server holds and works out, per local
-- series, the volumes it doesn't hold: preorders, recent releases and missing
-- volumes (GET /me/upcoming), and announces a new release to the admin
-- (book_released). Every table here is DERIVED and rebuildable from the books
-- and the community metadata: path-keyed, no FK to books, cascaded with their
-- library. Written through the user-state path (no content cache reads them).

-- Each owned book's community work: (library, path) -> work id, resolved from
-- its asin/isbn. id_key is the identifiers it was resolved by (meta's cache
-- key), so a changed ASIN re-resolves. work_id '' = no community match.
CREATE TABLE upcoming_owned (
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    path       TEXT NOT NULL,
    id_key     TEXT NOT NULL,
    work_id    TEXT NOT NULL DEFAULT '',
    checked_at TEXT NOT NULL,
    PRIMARY KEY (library_id, path)
) WITHOUT ROWID;
CREATE INDEX idx_upcoming_owned_work ON upcoming_owned(work_id);

-- A local series (library, folded series key) -> the community series it is.
-- series_id '' records that none was found, so it isn't asked again until the
-- series gains books or the row ages out. name is the local spelling (the
-- player opens the series view by it); books the series' book count when it
-- was resolved; classified_at when its items were first worked out (NULL: the
-- next run's items for it are a silent baseline, never announced).
CREATE TABLE upcoming_series (
    library_id    INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    series_key    TEXT NOT NULL,
    series_id     TEXT NOT NULL,
    name          TEXT NOT NULL DEFAULT '',
    books         INTEGER NOT NULL DEFAULT 0,
    resolved_at   TEXT NOT NULL,
    classified_at TEXT,
    PRIMARY KEY (library_id, series_key, series_id)
) WITHOUT ROWID;

-- A community series as series/{id} last answered it: members is a JSON list of
-- {work_id, position, title, authors, cover_url, release_date, added_at}.
-- state 'gone' (a 404) keeps the last members for a grace period (gone_at).
CREATE TABLE meta_series (
    series_id  TEXT PRIMARY KEY,
    name       TEXT NOT NULL DEFAULT '',
    members    TEXT NOT NULL DEFAULT '[]',
    fetched_at TEXT NOT NULL,
    state      TEXT NOT NULL DEFAULT 'ok' CHECK (state IN ('ok', 'gone')),
    gone_at    TEXT
);

-- What a local series lacks: one row per work, its kind as of the last run.
-- first_seen_at is when the item first appeared (the player's "new" badge);
-- announced_at when book_released told the admin about it (or the silent
-- baseline marked it), so it is announced exactly once.
CREATE TABLE upcoming_items (
    library_id    INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    series_key    TEXT NOT NULL,
    work_id       TEXT NOT NULL,
    series_id     TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('preorder', 'released', 'missing')),
    release_date  TEXT NOT NULL DEFAULT '',
    position      TEXT NOT NULL DEFAULT '',
    first_seen_at TEXT NOT NULL,
    announced_at  TEXT,
    PRIMARY KEY (library_id, series_key, work_id)
) WITHOUT ROWID;
CREATE INDEX idx_upcoming_items_work ON upcoming_items(work_id);
