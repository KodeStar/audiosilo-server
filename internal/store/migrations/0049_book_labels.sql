-- More of a book's metadata as effective values (catalog: overrides.go,
-- labels.go), layered like the rest (bookLayers.resolve: the scan, an
-- Audiobookshelf metadata.json, then a community match or an edit):
--   subtitle, publisher, language: text ('' for none), from the ABS file, a
--     match or an edit;
--   abridged: 1 or 0, NULL while nothing says (ABS's false says nothing, nor
--     does a match's);
--   genres, tags: the book's labels, a canonical JSON list ('' for none).
--     Genres come from the genre tag too, tags only from the ABS file or an
--     edit.
-- Effective values like title, rewritten from the layers by refreshEffective.
ALTER TABLE books ADD COLUMN subtitle TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN publisher TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN language TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN abridged INTEGER;
ALTER TABLE books ADD COLUMN genres TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN tags TEXT NOT NULL DEFAULT '';

-- A book indexed before this never had its genre tag read, so it starts
-- unchecked (0) and the next scan reads its primary file once (tags and
-- ffprobe, no re-index; catalog.SetGenres), like 0037's released_checked; every
-- index from now on writes 1. A rebuildable index flag.
ALTER TABLE books ADD COLUMN genres_checked INTEGER NOT NULL DEFAULT 0;

-- Every metadata.json was read before its subtitle, publisher, language,
-- abridged, genres and tags were: marking each folder book's file unread for
-- this index (0046's abs_indexed_at) has the next scan read it again, without a
-- re-index.
UPDATE books SET abs_indexed_at = NULL;

-- Each book's labels by key (catalog.labelKey: lowercased, spacing collapsed),
-- the genre= and tag= filters' and the facet counts' index, with the spelling
-- the book uses. Written by Go (refreshEffective, beside book_more_series) from
-- the effective lists, never computed in SQL. Derived, rebuildable index data
-- like the books rows they belong to; every row before this has no labels, so
-- the table starts empty.
CREATE TABLE book_labels (
    book_id    INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    library_id INTEGER NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('genre', 'tag')),
    label_key  TEXT NOT NULL,
    name       TEXT NOT NULL,
    PRIMARY KEY (book_id, kind, label_key)
) WITHOUT ROWID;
CREATE INDEX idx_book_labels_key ON book_labels(kind, label_key, library_id);

-- The full-text index gains the labels (a label a line), so a search finds a
-- book by a genre or a tag. FTS5 can't add a column, so it is made again with
-- every row copied over: no book has a label yet, so the copy is the whole index.
CREATE VIRTUAL TABLE books_fts_new USING fts5(title, author, series, narrator, genres, tags);
INSERT INTO books_fts_new(rowid, title, author, series, narrator, genres, tags)
    SELECT rowid, title, author, series, narrator, '', '' FROM books_fts;
DROP TABLE books_fts;
ALTER TABLE books_fts_new RENAME TO books_fts;
