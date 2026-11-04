-- The identifiers and language a book's file tags give (asin / audible_asin, the
-- iTunes freeform ASIN item, an ID3 TXXX ASIN; isbn; language), as HINTS for a
-- community match only: never books.asin / isbn, so a tagged book is not matched
-- by its tags (a tagged ASIN is often another store's, edition's or simply wrong)
-- and still goes through a match run, which sends the hint. Scanned values like
-- released, rewritten by every index, never on the wire.
ALTER TABLE books ADD COLUMN tag_asin TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN tag_isbn TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN tag_language TEXT NOT NULL DEFAULT '';

-- A book indexed before this never had these tags read, so it starts unchecked
-- (0) and the next scan reads its primary file once (no re-index), like
-- released_checked; every index from now on writes 1. A rebuildable index flag.
ALTER TABLE books ADD COLUMN hints_checked INTEGER NOT NULL DEFAULT 0;
