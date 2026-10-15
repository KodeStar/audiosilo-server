-- The Bridge (BRIDGE-PLAN.md Phase 0): the optional companion that writes to the
-- libraries and runs the heavy jobs, so the server stays read-only. The server
-- never connects to the Bridge: it stores intents (jobs) and the Bridge's last
-- reported status, and the Bridge pulls.
--
-- bridge_jobs: one row per job an admin asked for. Path-keyed like the other
-- durable tables (library_id + rel_path, NULL for a job about no book or no
-- library), with no FK to the rebuildable books index; an open job follows its
-- book on a move (catalog.MoveDurableState) and goes with its library.
--
-- state: queued -> claimed -> running -> done | failed | needs_attention |
-- cancelled. A claim takes a lease (lease_until, renewed by every progress
-- update); a lease that runs out puts the job back in the queue (attempts
-- counts the claims, and past the cap the job fails), so a Bridge that dies
-- never loses one. needs_attention is a parked job the Bridge still holds; it
-- has no lease. Cancelling a claimed or running job is cooperative: it sets
-- cancel_requested, which the Bridge reads in the reply to its next update.
--
-- dedupe_key (kind + library + path) is unique among the open jobs, so a second
-- press while one is queued, running or parked returns that one.
CREATE TABLE bridge_jobs (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    kind             TEXT NOT NULL,
    library_id       INTEGER REFERENCES libraries(id) ON DELETE CASCADE,
    rel_path         TEXT,
    params           TEXT NOT NULL DEFAULT '{}',
    state            TEXT NOT NULL DEFAULT 'queued',
    progress         TEXT NOT NULL DEFAULT '{}',
    result           TEXT NOT NULL DEFAULT '{}',
    error            TEXT NOT NULL DEFAULT '',
    requested_by     INTEGER REFERENCES users(id) ON DELETE SET NULL,
    priority         INTEGER NOT NULL DEFAULT 0,
    dedupe_key       TEXT NOT NULL,
    attempts         INTEGER NOT NULL DEFAULT 0,
    lease_until      TEXT,
    cancel_requested INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    claimed_at       TEXT,
    finished_at      TEXT
);
CREATE UNIQUE INDEX idx_bridge_jobs_open ON bridge_jobs(dedupe_key)
    WHERE state IN ('queued', 'claimed', 'running', 'needs_attention');
-- The claim: the highest priority queued job, oldest first.
CREATE INDEX idx_bridge_jobs_queue ON bridge_jobs(priority DESC, id) WHERE state = 'queued';
-- Leases to expire.
CREATE INDEX idx_bridge_jobs_lease ON bridge_jobs(lease_until) WHERE state IN ('claimed', 'running');
-- A book's jobs (the console's book page, and a move).
CREATE INDEX idx_bridge_jobs_book ON bridge_jobs(library_id, rel_path);

-- bridge_status: the one paired Bridge (one row, id 1). The pairing code waiting
-- to be exchanged (hashed, expiring), who paired it and when, and the last
-- heartbeat (PUT /bridge/status) as the Bridge sent it. The Bridge's token is a
-- tokens row of kind 'bridge' (auth); this row holds no secret but the code's
-- hash.
CREATE TABLE bridge_status (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    pair_code_hash  TEXT,
    pair_expires_at TEXT,
    pair_created_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    paired_at       TEXT,
    paired_by       INTEGER REFERENCES users(id) ON DELETE SET NULL,
    heartbeat_at    TEXT,
    status          TEXT NOT NULL DEFAULT '{}'
);

-- The live bridge token (auth.BridgeLiveSQL, read by the console and GET
-- /server through the heartbeat), found without scanning every session and key.
CREATE INDEX idx_tokens_bridge ON tokens(user_id) WHERE kind = 'bridge' AND revoked = 0;
