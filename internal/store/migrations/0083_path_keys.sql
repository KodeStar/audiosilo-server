-- A book's path in Unicode NFC, the comparison key of a path spelled another way
-- (server#210, catalog.Spelling): a request for "Émile" with a combining accent
-- (NFD) finds the book indexed as "Émile" precomposed (NFC), and the other way
-- round, while the stored and answered spelling stays the index's (the disk's).
-- NULL = not keyed yet (catalog.KeyPaths keys it at the next start; a book an
-- older server inserts after a rollback stays NULL until then), '' = rel_path is
-- already NFC (every ASCII path: the key is rel_path itself), else NFC(rel_path).
-- Derived from rel_path, which never changes on a row, so a rebuildable index
-- value, never on the wire.
ALTER TABLE books ADD COLUMN path_key TEXT;
-- Only the books whose path is not NFC: few, so the index is small.
CREATE INDEX idx_books_path_key ON books(library_id, path_key) WHERE path_key != '';
-- "Is any book unkeyed?" at the start, without a table scan.
CREATE INDEX idx_books_path_key_unset ON books(id) WHERE path_key IS NULL;
