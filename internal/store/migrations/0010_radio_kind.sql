-- The node's radio kind (radiokind: graywolf, uvk5, ...), chosen on the
-- Station page and changeable mid-race when a radio is swapped. kind is
-- what the operator chose; active_kind is what the app last started
-- with ('' before the first start that knew about kinds). Node data:
-- kept by Reset.
CREATE TABLE node_radio (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    kind TEXT NOT NULL DEFAULT '',
    active_kind TEXT NOT NULL DEFAULT '',
    updated_at DATETIME NOT NULL
);

-- Each kind's inbox cursor while another kind is active: the cursors are
-- the backends' own and mean nothing to each other. inbox_state holds
-- the active kind's.
CREATE TABLE inbox_cursors (
    kind TEXT PRIMARY KEY,
    cursor TEXT NOT NULL DEFAULT ''
);

-- Radio message ids start at 2^40, far above graywolf's, so a batch
-- bound to one backend's row is never mistaken for the other's row after
-- a switch (gw_rows, batches, bad reports and link probes key on the
-- id). A node that already has radio rows (a phase 17 radio node, which
-- had no graywolf rows) has them, and every reference, moved up.
UPDATE gw_rows SET gw_message_id = gw_message_id + 1099511627776
    WHERE gw_message_id IN (SELECT id FROM radio_messages WHERE id < 1099511627776);
UPDATE batches SET gw_message_id = gw_message_id + 1099511627776
    WHERE gw_message_id IN (SELECT id FROM radio_messages WHERE id < 1099511627776);
UPDATE received_batches SET gw_message_id = gw_message_id + 1099511627776
    WHERE gw_message_id IN (SELECT id FROM radio_messages WHERE id < 1099511627776);
UPDATE bad_reports SET gw_message_id = gw_message_id + 1099511627776
    WHERE gw_message_id IN (SELECT id FROM radio_messages WHERE id < 1099511627776);
UPDATE link_probes SET gw_message_id = gw_message_id + 1099511627776
    WHERE gw_message_id IN (SELECT id FROM radio_messages WHERE id < 1099511627776);
UPDATE radio_messages SET id = id + 1099511627776 WHERE id < 1099511627776;

UPDATE sqlite_sequence
    SET seq = MAX(1099511627776, seq, COALESCE((SELECT MAX(id) FROM radio_messages), 0))
    WHERE name = 'radio_messages';
INSERT INTO sqlite_sequence (name, seq)
    SELECT 'radio_messages', MAX(1099511627776, COALESCE((SELECT MAX(id) FROM radio_messages), 0))
    WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'radio_messages');
