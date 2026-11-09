-- server#330: the identifiers a book's recorded community match offered when it
-- was recorded (the recording's ASIN and ISBN, in stored form), whether or not
-- the admin ticked them. A refresh by the recorded match never fills an
-- identifier a book with neither has when it is the one the match offered: the
-- admin unticked it, or took it and reverted it since. One the recording gained
-- since (another value) still fills. No backfill: '' (a match recorded before
-- 0070, or by a client that sends none) behaves as before, filling whatever the
-- recording has.
ALTER TABLE book_matches ADD COLUMN offered_asin TEXT NOT NULL DEFAULT '';
ALTER TABLE book_matches ADD COLUMN offered_isbn TEXT NOT NULL DEFAULT '';
