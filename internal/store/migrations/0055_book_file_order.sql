-- A folder book's file order, stored (#177): catalog.FileOrderKey of its files
-- in the order they play (the FNV-64a of their paths, one per line, bit-cast to
-- a signed 64-bit integer). The scan compares it with the order it finds, so a
-- book whose files it now orders differently is read again (#160), without
-- loading every book_files row of the library on each scan. NULL is "not
-- recorded": every row before this migration, and a single-file book.
-- file_order_indexed_at is the indexed_at the key was recorded with: NULL or
-- another index's (an older server re-indexed the row, writing its book_files in
-- its own order and leaving the key) is "not recorded" too. A folder book's key
-- is filled by the next scan from its book_files (catalog.Signatures reads them
-- only while it is not recorded), with no re-index. Derived, rebuildable index
-- data.
ALTER TABLE books ADD COLUMN file_order INTEGER;
ALTER TABLE books ADD COLUMN file_order_indexed_at TEXT;
