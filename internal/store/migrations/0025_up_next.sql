-- Player redesign Phase 1b: a listener's "up next" queue, the books they mean to
-- play after the current one, in their order.
--
-- Durable, path-keyed user state like favourites/progress: (user_id, library_id,
-- rel_path), deliberately NOT FK'd to the rebuildable books index, so it survives
-- a re-scan, re-tagging, and a share being revoked and granted again (a row
-- outside the listener's current access is kept but not returned, until an add
-- needs its room: catalog.orderedList). rel_path is
-- always a BOOK's path (catalog.CleanRelPath form). A move or a join carries the
-- rows (catalog.carryListeningState); user and library deletes cascade them away.
--
-- position orders a user's rows (catalog.orderedList; gaps allowed: a remove
-- leaves one, an insert shifts only the rows after it); added_at is when the
-- book was queued (RFC3339 UTC), and orders the evictions of hidden rows. At
-- most catalog.MaxQueue rows per user.
CREATE TABLE up_next (
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    rel_path   TEXT NOT NULL,
    position   INTEGER NOT NULL,
    added_at   TEXT NOT NULL,
    PRIMARY KEY (user_id, library_id, rel_path)
);
-- The scanner's carry on a move/join looks rows up by path, across users.
CREATE INDEX idx_up_next_path ON up_next(library_id, rel_path);
