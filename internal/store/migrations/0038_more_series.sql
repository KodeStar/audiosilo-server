-- More than one series per book: books.more_series is the effective list of the
-- other series a book belongs to beyond its main series/series_index, as JSON
-- ([{"name": ..., "position": ...}], position 0 = none). Only an edit or a
-- community match supplies it (a book_overrides row, field 'more_series', like
-- any other override), so every existing book starts with none.
ALTER TABLE books ADD COLUMN more_series TEXT NOT NULL DEFAULT '[]';
