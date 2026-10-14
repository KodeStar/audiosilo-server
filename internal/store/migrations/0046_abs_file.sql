-- What an Audiobookshelf metadata.json in a folder book's folder says (catalog:
-- absfile.go), a layer of the effective values over the tags (bookLayers.resolve),
-- derived from the file like `scanned`, so rebuildable:
--   abs_sig: the file's signature (size and mtime; '' when there is none), what a
--     scan compares to notice ABS rewriting the file without touching the audio;
--   abs_indexed_at: the indexed_at the file was read with. NULL (every row before
--     this migration) or another index's (an older server re-indexed the row) means
--     the next scan reads the file again, without a re-index;
--   abs_file: what the file says, a JSON object of field -> value normalized as an
--     edit would be ('' for nothing).
-- The narrow columns come first, so a scan reading them never reads abs_file.
ALTER TABLE books ADD COLUMN abs_sig TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN abs_indexed_at TEXT;
ALTER TABLE books ADD COLUMN abs_file TEXT NOT NULL DEFAULT '';
