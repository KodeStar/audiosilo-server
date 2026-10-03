-- Metadata overrides: an admin's edits to a book's metadata, kept as durable,
-- path-keyed rows (no FK to the rebuildable books index, like folder_overrides and
-- book_enrichment). Files on disk are never touched. The books row holds the
-- EFFECTIVE values (scan, then enrichment, then overrides), so players, search and
-- export read edited values with no read-time join; catalog.refreshEffective
-- rebuilds them, and a scan never overwrites an overridden field because it
-- re-applies the overrides in the same transaction as the upsert.
--
-- source records where the value came from: 'edited' (typed by an admin) or
-- 'community' (accepted from a community-metadata match). updated_by survives the
-- editor's account being deleted (SET NULL), so an edit outlives its author.
CREATE TABLE book_overrides (
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    path       TEXT NOT NULL,             -- library-relative book path (slash-separated)
    field      TEXT NOT NULL,             -- catalog.OverrideFields
    value      TEXT NOT NULL,
    source     TEXT NOT NULL DEFAULT 'edited',
    updated_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (library_id, path, field)
);

-- Chapter-title overrides, by chapter index within the book. An index the book no
-- longer has after a rescan is kept (and reapplies if the chapter comes back).
CREATE TABLE chapter_overrides (
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    path       TEXT NOT NULL,
    idx        INTEGER NOT NULL,
    title      TEXT NOT NULL,
    updated_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (library_id, path, idx)
);

-- A custom cover uploaded in the console. Stored in the database rather than the
-- library folder (files stay untouched) or a loose data-dir file, so it is
-- path-keyed durable state that moves with MoveDurableState and is part of any
-- database backup. Small by construction (the upload is capped).
CREATE TABLE book_covers (
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    path       TEXT NOT NULL,
    mime       TEXT NOT NULL,
    data       BLOB NOT NULL,
    updated_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (library_id, path)
);

-- Fields that only an edit or a community match can supply today (the scanner
-- reads neither from tags).
ALTER TABLE books ADD COLUMN published TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN description TEXT NOT NULL DEFAULT '';

-- Whether the book has cover art (a sibling image or art embedded in the audio).
-- NULL until a scan has checked: the scanner backfills it with a cheap tag read.
ALTER TABLE books ADD COLUMN has_cover INTEGER;
UPDATE books SET has_cover = 1 WHERE cover_path <> '';

-- A series-part tag of "inf" parses as an infinite position, which no JSON reply
-- can carry; it is no position, as the export already treats it.
UPDATE books SET series_index = 0 WHERE series_index IN (9e999, -9e999);

-- What the scan found, before enrichment and overrides were layered on: a JSON
-- object of field -> value (a blank and a missing field read the same), stamped
-- with the indexed_at of the upsert that wrote it ('@indexed_at'; see
-- catalog.scannedStampKey). It is what a revert restores, what the console shows
-- beside an edited value, and (against the path) where a value came from. Existing
-- rows are backfilled from their current values: no overrides exist yet, and
-- asin/isbn only ever come from enrichment, so they are left out.
ALTER TABLE books ADD COLUMN scanned TEXT NOT NULL DEFAULT '';
UPDATE books SET scanned = json_object(
    'title', title, 'author', author, 'narrator', narrator, 'series', series,
    'series_index', CAST(series_index AS TEXT), '@indexed_at', indexed_at
);

-- The scanner used to re-apply book_enrichment at the end of every scan; now the
-- upsert does, but only for a book it re-indexes. A row an earlier scan left
-- without its ASIN/ISBN (one stopped between re-indexing a book and that final
-- pass) is put right here, once, by the same rule: a non-blank enrichment field
-- wins.
UPDATE books SET
    asin = COALESCE((SELECT NULLIF(e.asin, '') FROM book_enrichment e
                      WHERE e.library_id = books.library_id AND e.path = books.rel_path), asin),
    isbn = COALESCE((SELECT NULLIF(e.isbn, '') FROM book_enrichment e
                      WHERE e.library_id = books.library_id AND e.path = books.rel_path), isbn)
 WHERE EXISTS(SELECT 1 FROM book_enrichment e
               WHERE e.library_id = books.library_id AND e.path = books.rel_path);

ALTER TABLE chapters ADD COLUMN scanned_title TEXT NOT NULL DEFAULT '';
UPDATE chapters SET scanned_title = title;

-- Each part's own codec, so the book page can show a mixed-codec folder honestly.
ALTER TABLE book_files ADD COLUMN codec TEXT NOT NULL DEFAULT '';

-- The book page lists every user's progress on one path; the primary key leads
-- with user_id, so without this that is a scan of all progress (the same WHERE
-- serves MoveDurableState's progress update).
CREATE INDEX idx_progress_path ON progress(library_id, rel_path);
