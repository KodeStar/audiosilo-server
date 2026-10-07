-- Player redesign Phase 4: a bookmark's label, a machine key the player maps to
-- its own text and icon ("quote", "favourite", "fell_asleep", ...), '' for none.
-- The server checks only its shape (catalog.checkBookmark), never the set of
-- keys, so a newer player can add one. It lives on the bookmark row, so a move or
-- join (catalog.carryListeningState), a backup and a user delete take it along.
ALTER TABLE bookmarks ADD COLUMN label TEXT NOT NULL DEFAULT '';
