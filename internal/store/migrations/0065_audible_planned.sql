-- Bridge Phase 4b owner fixes: what the console says of a title the Bridge left
-- unmatched. These columns are the poll's (each titles call writes them) and are
-- cleared whenever a match is stored for the title.

-- planned is where a backup of the title would go, as the Bridge planned it at
-- its last poll (JSON: library_id, path, file, kind, siblings, state, conflict,
-- template; '' for none). The Bridge plans it: placement rules and the disk
-- check are its own.
ALTER TABLE audible_titles ADD COLUMN planned TEXT NOT NULL DEFAULT '';

-- contest_library_id + contest_path are the book another title of the account
-- claimed as well, when the Bridge left the title unmatched as contested (the
-- account's library at the poll; NULL and '' for none). Path-keyed like the
-- match, no FK to the index, moved with the book and cleared with its library.
ALTER TABLE audible_titles ADD COLUMN contest_library_id INTEGER;
ALTER TABLE audible_titles ADD COLUMN contest_path TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_audible_titles_contest ON audible_titles(contest_library_id, contest_path)
  WHERE contest_library_id IS NOT NULL;
