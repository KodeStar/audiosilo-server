-- server#232: a podcast feed's run of failed checks as the admin's notifications
-- count it (catalog.FoldPodcastJob, podcast.poll only). feed_failures counts the
-- polls in a row the feed itself failed (a code outside bridge's notFeedFailures:
-- an offline Bridge or an unwritable library never counts, nor resets it), and
-- feed_network_failures how many of them were network-class (bridge.NetworkFailure:
-- dns, network, timeout, tls), which a Bridge losing its internet raises for every
-- show at once, so they take twice as many to announce; failing_announced is set
-- once podcast_feed_failing was raised (catalog.feedRun: `gone` or a lost
-- private-feed secret at once) and cleared by the next successful poll, which
-- raises podcast_feed_recovered. The existing `failures` still drives the backoff.
-- No backfill: an existing failing feed counts from its next failure.
ALTER TABLE podcast_feed_state ADD COLUMN feed_failures INTEGER NOT NULL DEFAULT 0;
ALTER TABLE podcast_feed_state ADD COLUMN feed_network_failures INTEGER NOT NULL DEFAULT 0;
ALTER TABLE podcast_feed_state ADD COLUMN failing_announced INTEGER NOT NULL DEFAULT 0;
