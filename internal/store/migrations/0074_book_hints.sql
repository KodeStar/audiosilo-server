-- server#366: identifiers another app gave a book, as HINTS for a community
-- match only, like a tag's (0067): never books.asin / isbn, never on the wire,
-- so a hinted book is not matched by them and still goes through a match run,
-- which sends the hint (matchrun.QueryFor: the book's own identifiers, else an
-- Audiobookshelf hint, else its tags'). source: abs (an applied listening import
-- carries the ASIN and ISBN Audiobookshelf has for every book it pairs, whoever's
-- history it is). import_id names the import that wrote the row (the last one
-- applied owns it; an undo deletes its rows), no foreign key: the row outlives
-- the import's record like book_matches.run_id. Durable and path-keyed like
-- book_enrichment, no FK to the index: it survives a rebuild, follows a move and
-- goes with its library.
CREATE TABLE book_hints (
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    path       TEXT NOT NULL,
    source     TEXT NOT NULL,
    asin       TEXT NOT NULL DEFAULT '',
    isbn       TEXT NOT NULL DEFAULT '',
    import_id  INTEGER,
    set_at     TEXT NOT NULL,
    PRIMARY KEY (library_id, path, source)
);
-- An undo (or a re-import's) deletes by import_id.
CREATE INDEX idx_book_hints_import ON book_hints(import_id) WHERE import_id IS NOT NULL;
