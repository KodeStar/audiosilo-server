-- A series position may be a range: an omnibus of books 1-3 is at 1 to 3 in its
-- series (catalog: the series_index field's "1-3"; metadata.ParseSeriesRange).
-- series_index stays the range's start, the position every client reads;
-- series_index_end is its end, 0 when the position isn't a range. The same for a
-- book's other series (book_more_series.position_end). Both are effective values
-- written by refreshEffective, like the columns beside them: derived,
-- rebuildable index data. Every row before this migration holds a single
-- position, so the default is right for all of them.
ALTER TABLE books ADD COLUMN series_index_end REAL NOT NULL DEFAULT 0;
ALTER TABLE book_more_series ADD COLUMN position_end REAL NOT NULL DEFAULT 0;
-- The next book's query reads a list member's position and end off the key index
-- alone (catalog.numberedSeriesMembers): the index covers the new column too.
DROP INDEX idx_book_more_series_key;
CREATE INDEX idx_book_more_series_key ON book_more_series(series_key, library_id, position, position_end);
