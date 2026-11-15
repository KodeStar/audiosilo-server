-- server#240: an admin's own chapter list for a book, made in the console's
-- chapter editor (times and titles). list is a metadata.ChapterList v1 (the
-- shape of the Bridge's chapters.json, generator "console"): the chapters on the
-- whole-book timeline and the files they were placed on (each one's path
-- relative to the book, size and duration_ms), so the edit applies only while
-- the book's files are still those (catalog.applyChapterSource: an edit beats a
-- choice, the community's, generated chapters and the files'); on other files
-- it is dormant, never applied to other audio. Durable and path-keyed like
-- chapter_choices, no FK to the index: it survives a rebuild, moves with the
-- book (MoveDurableState), is never copied onto a joined book (other audio) and
-- goes with its library.
CREATE TABLE chapter_edits (
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    path       TEXT NOT NULL,
    list       TEXT NOT NULL,
    updated_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (library_id, path)
);

-- The editor's player streams the book's files with a cast token (0043), which
-- must not count against the person's cast tokens: a mint past that cap drops
-- the oldest, perhaps one a receiver is playing with. editor = 1 marks a token
-- minted for the editor (auth.IssueEditorToken), which has a cap of its own.
ALTER TABLE cast_grants ADD COLUMN editor INTEGER NOT NULL DEFAULT 0;
