-- Per-user favourites: items (folders or books) a user has hearted to curate a
-- smaller, personal view of a library.
--
-- Like progress/bookmarks/notes, this is durable, path-keyed user state —
-- (user_id, library_id, rel_path) — deliberately NOT FK'd to the rebuildable
-- books index, so it survives a re-scan/rebuild, re-tagging, and being recorded
-- before a scan reaches the path. A favourite can address any path: a navigation
-- folder (a series/author), a book folder, or a single-file book.
CREATE TABLE favourites (
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    rel_path   TEXT NOT NULL,            -- library-relative path (slash-separated)
    created_at TEXT NOT NULL,
    PRIMARY KEY (user_id, library_id, rel_path)
);
CREATE INDEX idx_favourites_user_library ON favourites(user_id, library_id);
