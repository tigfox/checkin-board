-- graywolf won't resend a DM while its retry ladder is off, so each
-- checkpoint retransmit is a new graywolf row (spec 3.1). batch_id links
-- every copy to its batch, so HQ's ACK for any copy confirms it.
ALTER TABLE gw_rows ADD COLUMN batch_id INTEGER;
CREATE INDEX idx_gw_rows_batch ON gw_rows(batch_id) WHERE batch_id IS NOT NULL;
