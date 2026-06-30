-- Path-keyed metadata enrichment (ASIN/ISBN), attached by the manager when it
-- matches an external source (e.g. an Audible library) to an indexed book — so a
-- book scanned without an ASIN gains one, making future matches exact.
--
-- Durable, path-keyed config deliberately NOT FK'd to the rebuildable books index
-- (like folder_overrides and the durable user state): it survives a re-scan/rebuild
-- and the scanner re-applies it onto freshly-indexed books (catalog.ApplyEnrichments).
CREATE TABLE book_enrichment (
    library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    path       TEXT NOT NULL,            -- library-relative book path (slash-separated)
    asin       TEXT NOT NULL DEFAULT '',
    isbn       TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL,
    PRIMARY KEY (library_id, path)
);
