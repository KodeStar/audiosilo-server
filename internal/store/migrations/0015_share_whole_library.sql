-- Mark the shares POST /admin/library-access creates ("Library: <name>" with a
-- single whole-library rule, catalog.GrantWholeLibrary) with the library they
-- grant, so a client tells a whole-library grant from a share an admin made by
-- this column rather than by guessing from the share's name or rules. No FK: the
-- share outlives its library's deletion (its rule cascades away and a later grant
-- heals it), exactly as before. NULL = an ordinary named share.
ALTER TABLE shares ADD COLUMN whole_library_id INTEGER;

UPDATE shares
   SET whole_library_id = (
         SELECT sp.library_id
           FROM share_paths sp JOIN libraries l ON l.id = sp.library_id
          WHERE sp.share_id = shares.id AND sp.path = '' AND shares.name = 'Library: ' || l.name)
 WHERE name LIKE 'Library: %';
