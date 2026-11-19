-- server#465: the notification outbox. One row per event and destination,
-- written in the transaction that records the event (catalog.RecordServerEvent,
-- inside the write that raised it for a podcast notice), so a notice due when
-- the server stopped is still owed at the next start (notify.Service.Run
-- redelivers every pending row once). The bell is server_events itself, never
-- written again by a redelivery.
--
-- state: pending, delivered, failed (the attempts ran out or the destination
-- refused it) or skipped (the destination was switched off or unsubscribed
-- before it went). attempts counts the sends begun, across restarts, so a
-- failing destination is tried a bounded number of times
-- (notify.Service.maxAttempts). Rows go with their event or destination, and
-- after catalog.NotifyOutboxRetention (catalog.PruneNotifyOutbox).
CREATE TABLE notify_outbox (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id   INTEGER NOT NULL REFERENCES server_events(id) ON DELETE CASCADE,
    target_id  INTEGER NOT NULL REFERENCES notification_targets(id) ON DELETE CASCADE,
    state      TEXT NOT NULL DEFAULT 'pending',
    attempts   INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    UNIQUE (event_id, target_id)
);
CREATE INDEX idx_notify_outbox_pending ON notify_outbox(state) WHERE state = 'pending';
CREATE INDEX idx_notify_outbox_target ON notify_outbox(target_id);
