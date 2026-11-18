-- The re-key's log (server#210, catalog.RekeyPathState): a listener's row a
-- client stored under a track path, a disc folder or another Unicode spelling
-- of a book's path moves to the book's own path, and each row is copied here
-- whole first (row_json: every column as it was), in the transaction that moves
-- it. With the before-upgrade copy (this migration being pending is what makes
-- the start that first runs the re-key take one), it is the way back: one row
-- per moved row, written once, kept. It holds the listener's own text (notes,
-- bookmark notes), so it goes with the user and the library, and is never on
-- the wire.
CREATE TABLE path_rekeys (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    at         TEXT NOT NULL,     -- fixed-width UTC with milliseconds
    reason     TEXT NOT NULL,     -- 'spelling' | 'file' | 'folder'
    user_id    INTEGER REFERENCES users(id) ON DELETE CASCADE, -- the row's listener (a collection item: its owner)
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    table_name TEXT NOT NULL,
    from_path  TEXT NOT NULL,
    to_path    TEXT NOT NULL,
    row_json   TEXT NOT NULL      -- the row as it was, every column by name
);
CREATE INDEX idx_path_rekeys_user ON path_rekeys(user_id);
CREATE INDEX idx_path_rekeys_library ON path_rekeys(library_id);
