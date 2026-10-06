-- Player redesign Phase 1b: a listener's own star rating (1-5) and a short note on
-- a book.
--
-- Durable, path-keyed user state like progress and favourites: (user_id,
-- library_id, rel_path) on the book's own path, deliberately NOT FK'd to the
-- rebuildable books index, so it survives a rebuild, re-tagging and a move
-- (catalog.carryRatings). A user or library delete purges it (cascade).
-- created_at / updated_at are fixed-width millisecond UTC strings, so they order
-- and compare as text (a move collision keeps the newer updated_at).
CREATE TABLE ratings (
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    rel_path   TEXT NOT NULL,
    rating     INTEGER NOT NULL CHECK (rating BETWEEN 1 AND 5),
    note       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (user_id, library_id, rel_path)
);
-- A move or join re-keys every listener's rating on one path.
CREATE INDEX idx_ratings_path ON ratings(library_id, rel_path);
