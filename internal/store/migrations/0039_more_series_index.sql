-- The few books in more than one series, by library: SeriesBooks finds a book
-- in a community rail's series through its more_series without reading every
-- book of the library on each /meta and /next.
CREATE INDEX idx_books_more_series ON books(library_id) WHERE more_series <> '[]';
