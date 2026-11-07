-- LibraForge E6 (server#320): a book's community match, path-keyed. The
-- community work (and recording) an admin accepted, or a match run applied, for
-- the book, whether or not the match gave it an ASIN or ISBN: a store-listing
-- match with neither is matched by this row alone (catalog.matchedExpr), so it
-- leaves Health's "Not matched" and the next match run, and a refresh run looks
-- it up by the work. how: auto (a match run's confident item) | accepted (the
-- match dialog, or a review item the admin chose). run_id names the run that
-- applied it and set_by the admin (no foreign keys: runs are pruned, and the row
-- outlives either). Durable and path-keyed like book_overrides, no FK to the
-- index: it follows a move and a join, and catalog.ClearCommunityMatches drops it.
CREATE TABLE book_matches (
    library_id   INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    path         TEXT NOT NULL,
    work_id      TEXT NOT NULL,
    recording_id TEXT NOT NULL DEFAULT '',
    how          TEXT NOT NULL CHECK (how IN ('auto', 'accepted')),
    run_id       INTEGER,
    set_by       INTEGER,
    set_at       TEXT NOT NULL,
    PRIMARY KEY (library_id, path)
);
