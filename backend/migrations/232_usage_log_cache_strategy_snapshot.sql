-- Persist the effective cache strategy identity on each usage record.
-- The name is a historical snapshot so later rename/delete operations do not
-- make past usage rows ambiguous.
ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS cache_strategy_id BIGINT,
    ADD COLUMN IF NOT EXISTS cache_strategy_name VARCHAR(100);

CREATE INDEX IF NOT EXISTS idx_usage_logs_cache_strategy_created
    ON usage_logs(cache_strategy_id, created_at DESC)
    WHERE cache_strategy_id IS NOT NULL;
