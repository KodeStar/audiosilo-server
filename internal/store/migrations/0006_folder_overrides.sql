-- Per-folder detection overrides for the auto book/folder classifier.
--
-- The scanner classifies each folder on its own (single-file books,
-- folder-per-book, or multi-file parts). When it gets a folder wrong, an admin
-- can pin it: mode 'book' forces the folder to be one (multi-file) book, mode
-- 'collection' forces each direct audio file to be its own book. This is durable,
-- path-keyed config — deliberately NOT FK'd to the rebuildable books index — so
-- it survives a re-scan/rebuild, matching how durable user state is keyed.
CREATE TABLE folder_overrides (
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    path       TEXT NOT NULL,            -- library-relative folder path (slash-separated)
    mode       TEXT NOT NULL,            -- 'book' | 'collection'
    updated_at TEXT NOT NULL,
    PRIMARY KEY (library_id, path)
);
