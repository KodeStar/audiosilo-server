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
-- redeemed_at flips an invite from "pending" to "accepted" so the admin console
-- can show one active invite per user and collapse spent ones into history. It is
-- left NULL for never-redeemed codes and cleared when an invite is rotated.
ALTER TABLE auth_codes ADD COLUMN kind TEXT NOT NULL DEFAULT 'invite';
ALTER TABLE auth_codes ADD COLUMN redeemed_at TEXT;
