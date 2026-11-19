-- server#375: a book's recorded match (book_matches) is now part of what its
-- community chapter check depends on (community_chapters.basis, the catalog's
-- chapterBasisExpr): a matched book's basis ends with char(30) and the match's
-- work and recording, so a match made, changed or unlinked makes the check
-- stale. Every check made before that carries no match, so on the upgrade each
-- matched book's would read as stale at once: its community chapters out of
-- use until it is checked again, and thousands of books (a bulk match run
-- writes the ASIN and the match together) fitted again in one pass, pauses and
-- all.
--
-- A check made of the identifiers alone found their recording, and still would
-- under the new rule when the match names that same recording, or names none
-- and is of that recording's work (chaptercheck's chapters). Those checks take
-- the match into their basis here and stay as they are. A check of another
-- recording or work than the match's, or one that found nothing, is left
-- without: it is stale, and the next pass checks the book by its match.
UPDATE community_chapters SET basis = basis || char(30) || m.work_id || '/' || m.recording_id
  FROM book_matches m
 WHERE m.library_id = community_chapters.library_id AND m.path = community_chapters.path
   AND community_chapters.work_id <> '' AND community_chapters.work_id = m.work_id
   AND (m.recording_id = '' OR m.recording_id = community_chapters.recording_id)
   AND instr(community_chapters.basis, char(30)) = 0;
