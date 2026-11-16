-- server#241: other recordings together. A book's community work key is its
-- recorded match (book_matches.work_id), else the work the upcoming job
-- resolved from its identifiers (upcoming_owned.work_id, already indexed by
-- idx_upcoming_owned_work). The other recordings of a work the caller holds
-- (catalog.OtherRecordings) are found from the work id on both sides, so the
-- recorded matches need an index by work too.
CREATE INDEX idx_book_matches_work ON book_matches(work_id);
