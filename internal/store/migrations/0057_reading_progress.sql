-- Bridge Phase 3b: a reader's place in a book's ebook, separate from its
-- listening progress on purpose (a handoff between the two only ever offers a
-- jump, never moves one silently).
--
-- Durable, path-keyed user state like progress and ratings: (user_id,
-- library_id, rel_path) on the BOOK's own path (never the epub's, never a
-- track's), deliberately NOT FK'd to the rebuildable books index, so it
-- survives a rebuild, re-tagging and a move (catalog.carryListeningState). A
-- user or library delete purges it (cascade).
-- cfi is the place as an EPUB CFI ("epubcfi(...)"), progression the same place
-- as a fraction of the book (0-1, what the conflict rule compares), version
-- the server's count of applied writes, device_id the client's device that
-- wrote it and device_name that device's name as the server knows it (the
-- token's, never the request's; '' when unknown). updated_at is the client's
-- time of the save as fixed-width UTC milliseconds (server time when
-- implausible), so two rows' times compare as text.
CREATE TABLE reading_progress (
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    library_id  INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    rel_path    TEXT NOT NULL,
    cfi         TEXT NOT NULL,
    progression REAL NOT NULL,
    version     INTEGER NOT NULL,
    device_id   TEXT NOT NULL DEFAULT '',
    device_name TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL,
    PRIMARY KEY (user_id, library_id, rel_path)
);
-- A move or join re-keys every reader's place on one path.
CREATE INDEX idx_reading_progress_path ON reading_progress(library_id, rel_path);
