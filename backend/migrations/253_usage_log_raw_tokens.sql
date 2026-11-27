ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS raw_input_tokens INTEGER,
    ADD COLUMN IF NOT EXISTS raw_output_tokens INTEGER,
    ADD COLUMN IF NOT EXISTS raw_cache_read_tokens INTEGER,
    ADD COLUMN IF NOT EXISTS raw_cache_creation_tokens INTEGER;
