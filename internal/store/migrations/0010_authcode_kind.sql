-- Distinguish admin-minted invites from user-owned recovery codes, and record
-- when an invite is first redeemed.
--
-- Recovery decouples re-authentication from invitation: an invite is a bounded,
-- single-purpose onboarding secret, while a recovery code is a durable, reusable
-- credential the user holds to get back in after signing out or losing a device —
-- without an admin minting a fresh invite each time. Both live in auth_codes and
-- redeem through the same path; only their lifetime/ownership differ.
--
--   kind 'invite'   -- admin-minted, bounded (default 5 uses / 1 day)
--   kind 'recovery' -- user-owned, durable (unlimited uses, never expires)
--
-- redeemed_at records when an invite was first redeemed ("accepted"); it is
-- informational (the console buckets invites by whether they are still
-- redeemable, not by redeemed_at). Left NULL for never-redeemed codes, cleared
-- when an invite is rotated.
ALTER TABLE auth_codes ADD COLUMN kind TEXT NOT NULL DEFAULT 'invite';
ALTER TABLE auth_codes ADD COLUMN redeemed_at TEXT;

-- Backfill: an already-used invite is, by definition, accepted — stamp it (with
-- the closest timestamp we have) so its "accepted" label is right for codes
-- issued before this column existed.
UPDATE auth_codes SET redeemed_at = created_at WHERE uses > 0 AND redeemed_at IS NULL;

-- The recovery EXISTS check on the user-read path, plus the supersede and clear
-- deletes, all filter auth_codes by (user_id, kind); index it.
CREATE INDEX IF NOT EXISTS idx_auth_codes_user_kind ON auth_codes(user_id, kind);
