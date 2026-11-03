-- Bridge Phase 4b owner fixes: what the console says of a title the Bridge left
-- unmatched. Both columns are the poll's (each titles call writes them) and are
-- cleared whenever a match is stored for the title.

-- planned is where a backup of the title would go, as the Bridge planned it at
-- its last poll (JSON: library_id, path, file, kind, siblings, state, conflict,
-- template; '' for none). The Bridge plans it: placement rules and the disk
-- check are its own.
ALTER TABLE audible_titles ADD COLUMN planned TEXT NOT NULL DEFAULT '';

-- contest_path is the book (a library-relative path in the account's library)
-- another title of the account claimed as well, when the Bridge left the title
-- unmatched as contested; '' for none. Path-keyed, no FK to the index, moved
-- with the book.
ALTER TABLE audible_titles ADD COLUMN contest_path TEXT NOT NULL DEFAULT '';
