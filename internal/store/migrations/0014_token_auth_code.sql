-- Link pairing tokens to the auth code that minted them, so an invite's QR is
-- as redeemable as the invite itself: the use is claimed at exchange (not at
-- redeem), and deleting/superseding a code kills its outstanding pairing
-- tokens via the FK cascade (foreign_keys is always ON - see store.dsnPragmas).
-- NULL = an unlinked token (sessions, /auth/pair and demo pairing tokens, and
-- all pre-migration tokens), which keeps the single-use pairing semantics.
ALTER TABLE tokens ADD COLUMN auth_code_id INTEGER REFERENCES auth_codes(id) ON DELETE CASCADE;

-- Rotate ("Resend") revokes a code's linked tokens by code id; partial index
-- because almost every token is an unlinked session.
CREATE INDEX idx_tokens_auth_code ON tokens(auth_code_id) WHERE auth_code_id IS NOT NULL;
