-- Community chapters: a community recording's chapter list (meta.audiosilo.app),
-- fitted onto a book's own audio (internal/chapteralign) by a background check
-- (internal/chaptercheck). Files on disk are never touched.

-- The last check of each book: what it found, and for a fitted status the
-- chapters on the book's timeline. A record of the index, not user state: it is
-- rebuilt by checking again (path-keyed like book_enrichment, no FK to books, so
-- an index rebuild keeps it, and it moves with the book). basis is what the check
-- saw (catalog.chapterBasisExpr: the identifiers and the audio's shape); a book
-- whose basis has changed since is checked again, and its check is not used
-- meanwhile. list_hash is the community list's, so a recheck that finds the same
-- list for the same audio only renews checked_at.
-- status: a chapteralign.Status, or no_match (no community recording for the
-- book's identifiers) | unavailable (the recording has no chapter list).
CREATE TABLE community_chapters (
    library_id   INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    path         TEXT NOT NULL,
    basis        TEXT NOT NULL,
    status       TEXT NOT NULL,
    work_id      TEXT NOT NULL DEFAULT '',
    recording_id TEXT NOT NULL DEFAULT '',
    list_hash    TEXT NOT NULL DEFAULT '',
    detail       TEXT NOT NULL DEFAULT '{}', -- chapteralign.Detail (JSON)
    chapters     TEXT NOT NULL DEFAULT '[]', -- the fitted chapters (JSON); [] unless fitted
    checked_at   TEXT NOT NULL,
    PRIMARY KEY (library_id, path)
);

-- An admin's choice of where a book's chapters come from: 'files' (the scan's) or
-- 'community' (the fitted list). Durable, path-keyed, no FK to the index, moved
-- with the book's other edits; never copied onto a joined book (other audio). No
-- row: automatic (community only when the book has no chapters of its own).
CREATE TABLE chapter_choices (
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    path       TEXT NOT NULL,
    source     TEXT NOT NULL,
    updated_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (library_id, path)
);

-- Where a book's chapters come from now: '' its own files (the scan), 'community'
-- a fitted community list, chapters_fit naming which (a hash of it, so an
-- unchanged fit is not written again). scanned_chapters is always the scan's own
-- chapters (JSON, scanned titles), as books.scanned is its metadata: what a check
-- fits against and what the chapters go back to. '' on a row indexed before 0035,
-- whose chapter rows are the scan's.
ALTER TABLE books ADD COLUMN chapters_source TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN chapters_fit TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN scanned_chapters TEXT NOT NULL DEFAULT '';
-- A hash of scanned_chapters, for a check's basis: chapters re-tagged since make
-- the check stale.
ALTER TABLE books ADD COLUMN chapters_hash TEXT NOT NULL DEFAULT '';
