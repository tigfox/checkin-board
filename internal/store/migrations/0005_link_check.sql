-- Deployment link check (spec 4.8).

-- Runs this node started (it is the prober).
CREATE TABLE link_checks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run INTEGER NOT NULL DEFAULT 0,
    peer_call TEXT NOT NULL,
    station_code TEXT NOT NULL,
    count INTEGER NOT NULL,
    spacing_sec INTEGER NOT NULL,
    state TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT '',
    requested_at DATETIME NOT NULL,
    started_at DATETIME,
    finished_at DATETIME,
    verdict TEXT NOT NULL DEFAULT '',
    uplink INTEGER NOT NULL DEFAULT 0,
    round_trip INTEGER NOT NULL DEFAULT 0,
    reply_received INTEGER NOT NULL DEFAULT 0,
    median_rtt_ms INTEGER,
    remote_level INTEGER,
    local_level INTEGER,
    via TEXT NOT NULL DEFAULT '',
    advice TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT ''
);
-- One run at a time per node, even with the CLI and the web UI racing.
CREATE UNIQUE INDEX link_checks_one_active ON link_checks ((1)) WHERE state IN ('requested', 'running');

CREATE TABLE link_probes (
    check_id INTEGER NOT NULL REFERENCES link_checks (id) ON DELETE CASCADE,
    idx INTEGER NOT NULL,
    gw_message_id INTEGER,
    msg_id TEXT NOT NULL DEFAULT '',
    sent_at DATETIME,
    acked_at DATETIME,
    rtt_ms INTEGER,
    PRIMARY KEY (check_id, idx)
);
CREATE INDEX link_probes_gw ON link_probes (gw_message_id);

-- Runs this node answered (it is the responder).
CREATE TABLE link_responses (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    peer_call TEXT NOT NULL,
    prober_code TEXT NOT NULL,
    run INTEGER NOT NULL,
    total INTEGER NOT NULL,
    heard TEXT NOT NULL DEFAULT '',
    first_heard_at DATETIME NOT NULL,
    last_heard_at DATETIME NOT NULL,
    level INTEGER,
    via TEXT NOT NULL DEFAULT '',
    unknown_peer INTEGER NOT NULL DEFAULT 0,
    reply_due_at DATETIME,
    reply_attempts INTEGER NOT NULL DEFAULT 0,
    reply_gw_id INTEGER,
    reply_sent_at DATETIME,
    reply_acked_at DATETIME,
    UNIQUE (peer_call, run)
);
CREATE INDEX link_responses_due ON link_responses (reply_due_at);
