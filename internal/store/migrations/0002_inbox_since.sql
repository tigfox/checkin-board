-- The inbox reader's starting point for a node with no cursor yet,
-- saved once on first run so a restart before any race traffic doesn't
-- move it forward and skip what graywolf received meanwhile.
ALTER TABLE inbox_state ADD COLUMN since DATETIME;
