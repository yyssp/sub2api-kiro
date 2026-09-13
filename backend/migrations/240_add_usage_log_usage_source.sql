-- Record whether a usage row's token counts came from the upstream or were
-- estimated by the gateway.
--
-- Cursor's agent.v1 protocol returns no token usage fields at all, so every
-- Cursor row is billed on a local tokenizer estimate. Without this column a
-- reconciliation query cannot separate measured rows from estimated ones —
-- they are indistinguishable once written.
--
-- NULL means "not declared" and must be read as upstream-reported, which is
-- the correct interpretation for every historical row: Cursor is the only
-- platform that bills on estimates, and it postdates all of them.
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS usage_source TEXT;

COMMENT ON COLUMN usage_logs.usage_source IS
  'Token usage provenance: estimated (gateway-side tokenizer) or upstream (provider-reported). NULL = historical row, treat as upstream.';
