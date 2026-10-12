-- UV-K5/UV-K1 radio backend (phase 17b): the radio keeps no message
-- history, so the node keeps graywolf-style message rows itself. upd_seq
-- orders changes for the inbox reader's cursor, as graywolf's feed does.
-- Node data: kept by Reset (graywolf's messages are too); "graywolf
-- cleanup" deletes the race's rows.
CREATE TABLE radio_messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    direction TEXT NOT NULL,
    status TEXT NOT NULL,
    from_call TEXT NOT NULL DEFAULT '',
    to_call TEXT NOT NULL DEFAULT '',
    peer_call TEXT NOT NULL DEFAULT '',
    text TEXT NOT NULL DEFAULT '',
    msg_id TEXT NOT NULL DEFAULT '',
    via TEXT NOT NULL DEFAULT '',
    unread INTEGER NOT NULL DEFAULT 0,
    failure_reason TEXT NOT NULL DEFAULT '',
    client_id TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL,
    sent_at DATETIME,
    received_at DATETIME,
    acked_at DATETIME,
    upd_seq INTEGER NOT NULL
);
CREATE INDEX radio_messages_upd ON radio_messages (upd_seq);
CREATE INDEX radio_messages_ack ON radio_messages (peer_call, msg_id);
