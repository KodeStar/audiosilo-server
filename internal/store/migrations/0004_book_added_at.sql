-- Track when a book was added, sourced from the filesystem (birth time, falling
-- back to mtime) by the scanner. Unlike the auto-increment id - which the old
-- "recent" sort relied on and which a full re-index reshuffles - this is a stable
-- chronological key that also works for a single cross-library "recently added"
-- query.
--
-- Existing rows are backfilled with indexed_at (the best proxy available without
-- re-reading the filesystem); the scanner fills in real birth times for books it
-- (re)indexes after this migration.

ALTER TABLE books ADD COLUMN added_at TEXT NOT NULL DEFAULT '';
UPDATE books SET added_at = indexed_at WHERE added_at = '';

-- Supports ORDER BY added_at DESC, id DESC for the recent listings.
CREATE INDEX idx_books_added ON books(added_at, id);
