-- Reusable protocol-neutral cache shaping policies bound to groups.
CREATE TABLE IF NOT EXISTS cache_strategies (
    id BIGSERIAL PRIMARY KEY,
    name VARCHAR(100) NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    revision BIGINT NOT NULL DEFAULT 1,
    config JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS cache_strategy_id BIGINT REFERENCES cache_strategies(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_groups_cache_strategy_id
    ON groups(cache_strategy_id)
    WHERE cache_strategy_id IS NOT NULL AND deleted_at IS NULL;
