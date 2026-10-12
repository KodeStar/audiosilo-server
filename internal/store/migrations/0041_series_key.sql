-- One series, any spelling, found by an index (catalog/moreseries.go):
-- books.series_key is the main series' key (catalog.seriesKey: case, accents,
-- punctuation and spacing aside; '' for none) and book_more_series holds the
-- book's other series (books.more_series, less its main one), one row each, by
-- key. Both are written by Go (refreshEffective) from the effective values, never
-- computed in SQL: the key needs Unicode normalisation, and the catalogue's SQL
-- functions must stay out of the schema. Rows indexed before this migration are
-- keyed once at start (Catalog.KeySeries, server_state 'series_keys').
--
-- Derived, rebuildable index data like the books rows they belong to.
--
-- idx_books_series_key replaces idx_books_series, which no query reads once the
-- filters go by key. series sits before series_index so a series' spellings are
-- adjacent: Health's "spelled several ways" is two range seeks per book (a
-- spelling below this one, or above), not a walk of the series.
ALTER TABLE books ADD COLUMN series_key TEXT NOT NULL DEFAULT '';
DROP INDEX idx_books_series;
-- The lists' own index (0039) served only the reads book_more_series replaces.
DROP INDEX idx_books_more_series;
CREATE INDEX idx_books_series_key ON books(library_id, series_key, series, series_index);

CREATE TABLE book_more_series (
    book_id    INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    library_id INTEGER NOT NULL,
    series_key TEXT NOT NULL,
    position   REAL NOT NULL DEFAULT 0,
    PRIMARY KEY (book_id, series_key)
) WITHOUT ROWID;
CREATE INDEX idx_book_more_series_key ON book_more_series(series_key, library_id, position);
