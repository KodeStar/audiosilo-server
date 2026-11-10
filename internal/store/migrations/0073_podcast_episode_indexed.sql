-- server#182: the books.indexed_at an episode's row was written with by the
-- podcast index (catalog.IndexPodcastShow), as file_order_indexed_at is for a
-- book's file order. An older server run on this database (a rollback) scans a
-- podcast library as books and can rewrite an episode's books row in place,
-- leaving its podcast_episodes row: the two stamps then differ, and the next
-- scan re-indexes the show (catalog.PodcastEpisodeRows counts only rows whose
-- stamps agree). Backfilled from each episode's books row as it is now, so a
-- show whose feed never changes again is covered from the first rollback after
-- the upgrade; NULL (a row written without the stamp) counts as the podcast
-- index's, as before.
ALTER TABLE podcast_episodes ADD COLUMN indexed_at TEXT;
UPDATE podcast_episodes SET indexed_at = (SELECT b.indexed_at FROM books b WHERE b.id = podcast_episodes.book_id);
