-- Calendar feed tokens (Track U3, capability upcoming_calendar): a tokens row of
-- kind 'feed', hashed like every token, no expiry, accepted only on
-- GET /me/upcoming/releases.ics. One per user: a mint deletes the earlier one in
-- the same transaction, and this index keeps a second live row out whatever
-- path writes it.
CREATE UNIQUE INDEX idx_tokens_feed_user ON tokens(user_id) WHERE kind = 'feed' AND revoked = 0;
