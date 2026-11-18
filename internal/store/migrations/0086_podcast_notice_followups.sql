-- server#403: follow-ups to the podcast notifications (0076).
--
-- podcast_feed_state.failing_code is the code podcast_feed_failing was last
-- announced with ('' while none is): a later `gone` or lost private-feed secret,
-- which stop the feed for good, is announced once more when it differs
-- (catalog.feedRun.next). A feed announced before this migration takes the
-- code of its last failed check. A feed that sits at `gone` ended before the
-- upgrade, when it was told (0076 announces `gone` at once) or was older than
-- the notices: it is marked announced with `gone`, whatever its flag, so a
-- Check now of it tells nothing again.
--
-- podcast_shows.feed_read_at is when the server began reading the feed.json
-- the show's index was folded from (feed.File.ReadAt, UTC to the nanosecond):
-- a scan that read feed.json before the Bridge's rescan read and indexed a
-- newer one doesn't fold it over that index (catalog.IndexPodcastShow). ''
-- until the show is indexed again.
ALTER TABLE podcast_feed_state ADD COLUMN failing_code TEXT NOT NULL DEFAULT '';
UPDATE podcast_feed_state SET failing_code = error_code WHERE failing_announced = 1;
UPDATE podcast_feed_state SET failing_announced = 1, failing_code = 'gone' WHERE error_code = 'gone';
ALTER TABLE podcast_shows ADD COLUMN feed_read_at TEXT NOT NULL DEFAULT '';
