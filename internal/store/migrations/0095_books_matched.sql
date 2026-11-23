-- Summary reads count a book with an ASIN, an ISBN or a recorded community
-- match. Derive that flag once here and keep it current in the writer's own
-- transaction, including writes by a server rolled back to an older build.
ALTER TABLE books ADD COLUMN matched INTEGER NOT NULL DEFAULT 0;

UPDATE books SET matched = 1
 WHERE asin <> '' OR isbn <> '' OR EXISTS (
    SELECT 1 FROM book_matches m WHERE m.library_id = books.library_id AND m.path = books.rel_path
 );

-- A durable match can predate the index row, including after a prune and rescan.
CREATE TRIGGER books_matched_insert AFTER INSERT ON books
WHEN NEW.asin <> '' OR NEW.isbn <> '' OR EXISTS (
    SELECT 1 FROM book_matches WHERE library_id = NEW.library_id AND path = NEW.rel_path
)
BEGIN
    UPDATE books SET matched = 1 WHERE id = NEW.id;
END;

CREATE TRIGGER books_matched_update AFTER UPDATE OF asin, isbn, library_id, rel_path ON books
WHEN (OLD.asin = '' AND OLD.isbn = '') <> (NEW.asin = '' AND NEW.isbn = '')
  OR OLD.library_id <> NEW.library_id OR OLD.rel_path <> NEW.rel_path
BEGIN
    UPDATE books SET matched = (NEW.asin <> '' OR NEW.isbn <> '' OR EXISTS (
        SELECT 1 FROM book_matches WHERE library_id = NEW.library_id AND path = NEW.rel_path
    )) WHERE id = NEW.id;
END;

CREATE TRIGGER book_matches_matched_insert AFTER INSERT ON book_matches
BEGIN
    UPDATE books SET matched = 1 WHERE library_id = NEW.library_id AND rel_path = NEW.path AND matched = 0;
END;

CREATE TRIGGER book_matches_matched_delete AFTER DELETE ON book_matches
BEGIN
    UPDATE books SET matched = 0 WHERE library_id = OLD.library_id AND rel_path = OLD.path AND asin = '' AND isbn = '';
END;

-- A move can have indexed books at both ends. A new work or recording at the
-- same key still counts as a match.
CREATE TRIGGER book_matches_matched_move AFTER UPDATE OF library_id, path ON book_matches
WHEN OLD.library_id <> NEW.library_id OR OLD.path <> NEW.path
BEGIN
    UPDATE books SET matched = 0 WHERE library_id = OLD.library_id AND rel_path = OLD.path AND asin = '' AND isbn = '';
    UPDATE books SET matched = 1 WHERE library_id = NEW.library_id AND rel_path = NEW.path AND matched = 0;
END;
