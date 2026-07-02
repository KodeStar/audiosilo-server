-- Keyset-pagination support indexes for the per-library book list. ListBooks
-- paginates on (sort_column, id) within a library, but the only prior index that
-- began with library_id led with `author` (idx_books_sort), so `sort=title` and
-- `sort=recent` fell back to a full per-page scan+sort of the whole library — the
-- opposite of the "fast regardless of library size" goal. These two composite
-- indexes let SQLite serve the ordered page directly:
--   sort=title  -> ORDER BY title,  id  within a library
--   sort=recent -> ORDER BY added_at, id within a library
-- (sort=author is already served by idx_books_sort's leading (library_id, author)).
CREATE INDEX IF NOT EXISTS idx_books_lib_title ON books(library_id, title, id);
CREATE INDEX IF NOT EXISTS idx_books_lib_added ON books(library_id, added_at, id);
