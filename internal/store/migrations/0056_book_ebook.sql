-- Bridge Phase 3b: a book's ebook, the one .epub beside its audio that the
-- reader opens (library.BookEbook's rule: a folder book's one regular .epub
-- directly in its folder, several being none; a single-file book's
-- "<stem>.epub" beside it; readable, a zip listing META-INF/container.xml).
-- ebook_path is its library-relative path ('' for none, or one that can't be
-- read); ebook_sig is the signature of the file the rule names before that
-- check (size, mtime and name; '' for none), so a scan that finds the book
-- unchanged records a new, changed or gone ebook without re-indexing it, and
-- an unreadable one isn't opened again until it changes or the book is
-- re-indexed. Derived from the files, rebuildable index data: every row
-- starts with none and the next scan fills them in.
ALTER TABLE books ADD COLUMN ebook_path TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN ebook_sig TEXT NOT NULL DEFAULT '';
