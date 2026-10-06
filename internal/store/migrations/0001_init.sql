-- Race data, settings and graywolf bookkeeping (spec section 5).
-- Timestamps are UTC, whole seconds (normTime). Bool columns are
-- INTEGER 0/1 with no gorm default tag (see models.go).

CREATE TABLE settings (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    role TEXT NOT NULL DEFAULT '',
    race_name TEXT NOT NULL DEFAULT '',
    race_state TEXT NOT NULL DEFAULT 'setup',
    race_started_at DATETIME,
    station_tactical TEXT NOT NULL DEFAULT '',
    checkpoint_code TEXT NOT NULL DEFAULT '',
    hq_local_codes TEXT NOT NULL DEFAULT '',
    hq_call TEXT NOT NULL DEFAULT '',
    gw_channel INTEGER NOT NULL DEFAULT 0,
    path TEXT NOT NULL DEFAULT '',
    max_text_len INTEGER NOT NULL DEFAULT 67,
    flush_after_sec INTEGER NOT NULL DEFAULT 20,
    max_in_flight INTEGER NOT NULL DEFAULT 4,
    heartbeat_sec INTEGER NOT NULL DEFAULT 300,
    gap_grace_sec INTEGER NOT NULL DEFAULT 90,
    updated_at DATETIME NOT NULL
);

-- HQ reference data.
CREATE TABLE checkpoints (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    code TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    course_order INTEGER NOT NULL DEFAULT 0,
    expected_call TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);

-- Deliberately no columns for personal data.
CREATE TABLE runners (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    bib INTEGER NOT NULL UNIQUE,
    category TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);

-- Checkpoint outbox. One batch = one graywolf message row once sent.
CREATE TABLE batches (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    cp_code TEXT NOT NULL,
    seq INTEGER NOT NULL,
    text TEXT NOT NULL,
    state TEXT NOT NULL,
    gw_message_id INTEGER,
    gw_msg_id TEXT NOT NULL DEFAULT '',
    client_id TEXT NOT NULL DEFAULT '',
    attempts INTEGER NOT NULL DEFAULT 0,
    last_tx_at DATETIME,
    next_tx_at DATETIME NOT NULL,
    acked_at DATETIME,
    created_at DATETIME NOT NULL,
    UNIQUE (cp_code, seq)
);
CREATE INDEX idx_batches_due ON batches(state, next_tx_at);
CREATE UNIQUE INDEX idx_batches_gw_message ON batches(gw_message_id) WHERE gw_message_id IS NOT NULL;

-- Checkpoint entry log. void_of points at the entry a void cancels.
CREATE TABLE local_entries (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    cp_code TEXT NOT NULL,
    bib INTEGER NOT NULL,
    time_in DATETIME NOT NULL,
    clock_synced INTEGER NOT NULL,
    state TEXT NOT NULL,
    void_of INTEGER REFERENCES local_entries(id) ON DELETE CASCADE,
    batch_id INTEGER REFERENCES batches(id) ON DELETE SET NULL,
    created_at DATETIME NOT NULL
);
CREATE INDEX idx_local_entries_state ON local_entries(cp_code, state);
CREATE INDEX idx_local_entries_batch_id ON local_entries(batch_id);
CREATE INDEX idx_local_entries_void_of ON local_entries(void_of);

-- HQ batch-level dedup key: (cp_code, seq, text).
CREATE TABLE received_batches (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    cp_code TEXT NOT NULL,
    seq INTEGER NOT NULL,
    text TEXT NOT NULL,
    source_call TEXT NOT NULL DEFAULT '',
    gw_message_id INTEGER,
    received_at DATETIME NOT NULL,
    UNIQUE (cp_code, seq, text)
);

-- HQ append-only event log of entries and voids.
CREATE TABLE received_entries (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    cp_code TEXT NOT NULL,
    bib INTEGER NOT NULL,
    time_in DATETIME NOT NULL,
    is_void INTEGER NOT NULL,
    batch_seq INTEGER,
    source_call TEXT NOT NULL DEFAULT '',
    received_at DATETIME NOT NULL
);
CREATE INDEX idx_received_entries_key ON received_entries(cp_code, bib, time_in);
CREATE INDEX idx_received_entries_bib ON received_entries(bib);

-- HQ per-checkpoint health.
CREATE TABLE cp_status (
    cp_code TEXT PRIMARY KEY,
    last_heard_at DATETIME,
    last_source_call TEXT NOT NULL DEFAULT '',
    heartbeat_at DATETIME,
    heartbeat_last_seq INTEGER NOT NULL DEFAULT 0,
    clock_skew_sec INTEGER,
    max_seq INTEGER NOT NULL DEFAULT 0,
    batches_received INTEGER NOT NULL DEFAULT 0,
    seq_reuse_count INTEGER NOT NULL DEFAULT 0,
    bad_reports INTEGER NOT NULL DEFAULT 0
);

-- HQ: RC1 text graywolf ACKed but the app couldn't decode.
CREATE TABLE bad_reports (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    gw_message_id INTEGER NOT NULL UNIQUE,
    from_call TEXT NOT NULL,
    text TEXT NOT NULL,
    error TEXT NOT NULL,
    received_at DATETIME NOT NULL
);

-- Every graywolf message row the app sent or ingested: idempotent
-- inbox processing, and the only rows post-race cleanup may delete.
CREATE TABLE gw_rows (
    gw_message_id INTEGER PRIMARY KEY,
    kind TEXT NOT NULL,
    created_at DATETIME NOT NULL,
    deleted_at DATETIME
);
CREATE INDEX idx_gw_rows_kind ON gw_rows(kind);

-- graywolf inbox cursor (singleton).
CREATE TABLE inbox_state (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    cursor TEXT NOT NULL DEFAULT '',
    updated_at DATETIME NOT NULL
);

-- Race peers' graywolf conversation prefs before the app changed them.
CREATE TABLE peer_prefs_backup (
    callsign TEXT PRIMARY KEY,
    send_path TEXT NOT NULL DEFAULT '',
    wait_for_ack INTEGER NOT NULL,
    saved_at DATETIME NOT NULL
);
