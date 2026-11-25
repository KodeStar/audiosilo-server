-- The original session for an invite QR and device. NULL retry_until spends
-- its one retry. Removing the QR (including daily retention) drops its history.
CREATE TABLE invite_exchanges (
    pairing_id INTEGER NOT NULL REFERENCES tokens(id) ON DELETE CASCADE,
    device_hash TEXT NOT NULL,
    session_id INTEGER NOT NULL UNIQUE REFERENCES tokens(id) ON DELETE CASCADE,
    retry_until TEXT,
    PRIMARY KEY (pairing_id, device_hash)
);
