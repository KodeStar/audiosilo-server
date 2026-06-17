-- Normalize chapters across single-file (m4b) and multi-file (mp3 parts) books.
-- file_index selects which book file a chapter plays from (0 for single-file);
-- book_offset is the chapter's start on the whole-book timeline.
ALTER TABLE chapters ADD COLUMN file_index INTEGER NOT NULL DEFAULT 0;
ALTER TABLE chapters ADD COLUMN book_offset REAL NOT NULL DEFAULT 0;
