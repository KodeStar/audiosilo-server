-- The Bridge's generated files in a book's .audiosilo/ folder (characters.json,
-- recaps.json, bridge.json; catalog: attachments.go), checked and stored here
-- for /meta to serve. Derived from the files like abs_file (0046), so
-- rebuildable: no durable state, nothing a move carries.
--   attachments_sig: the known files' signatures (name:size:mtime_ns, joined;
--     '' for none), what a scan compares to notice the Bridge writing them
--     without touching the audio;
--   attachments_indexed_at: the indexed_at they were read with. NULL (every row
--     before this migration) or another index's means the next scan reads them
--     again, without a re-index.
ALTER TABLE books ADD COLUMN attachments_sig TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN attachments_indexed_at TEXT;

-- What the files say, checked against audiosilo-meta's formats: one row per book
-- with any (JSON, '' for a file that isn't there or can't be used), gone with
-- its book.
CREATE TABLE book_attachments (
    book_id    INTEGER PRIMARY KEY REFERENCES books(id) ON DELETE CASCADE,
    characters TEXT NOT NULL DEFAULT '',
    recaps     TEXT NOT NULL DEFAULT '',
    provenance TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL
);
