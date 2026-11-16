-- Add OpenCode as a first-class platform (account types Zen / GO).
--
-- 1. user_platform_quotas.platform CHECK
-- 2. composite_model_routes.target_platform CHECK
-- 3. channel_monitors / channel_monitor_request_templates provider CHECK
--
-- Runs after 237_add_minimax_platform.sql. DROP ... IF EXISTS + 幂等守卫保证可重入；
-- 新约束必须是当前平台全集的超集：保留本 fork 的 Kiro/Cursor 与上游 Adobe、OpenCode，
-- 以及已由主分支加入的其他 concrete platform；否则存量配额/路由行会令 ADD CONSTRAINT 失败。
-- 历史 checksum 兼容规则见 migrations_runner.go。

ALTER TABLE user_platform_quotas
    DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;

ALTER TABLE user_platform_quotas
    ADD CONSTRAINT user_platform_quotas_platform_check
    CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'kiro', 'grok',
                        'kimi', 'zhipu', 'deepseek', 'minimax', 'cursor', 'adobe', 'opencode_go',
                        'typesafe', 'command_code', 'cline', 'leonardo', 'fish'));

ALTER TABLE composite_model_routes
    DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;

ALTER TABLE composite_model_routes
    ADD CONSTRAINT composite_model_routes_target_platform_check
    CHECK (target_platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'kiro', 'grok',
                               'kimi', 'zhipu', 'deepseek', 'minimax', 'cursor', 'adobe', 'opencode_go',
                               'typesafe', 'command_code', 'cline', 'leonardo', 'fish'));

DO $$
DECLARE
    monitor_constraint_def TEXT;
    template_constraint_def TEXT;
BEGIN
    SELECT pg_get_constraintdef(c.oid)
      INTO monitor_constraint_def
      FROM pg_constraint c
      JOIN pg_class t ON t.oid = c.conrelid
     WHERE t.relname = 'channel_monitors'
       AND c.conname = 'channel_monitors_provider_check';

    IF monitor_constraint_def IS NULL OR position('opencode_go' IN monitor_constraint_def) = 0 THEN
        ALTER TABLE channel_monitors
            DROP CONSTRAINT IF EXISTS channel_monitors_provider_check;
        ALTER TABLE channel_monitors
            ADD CONSTRAINT channel_monitors_provider_check
            CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok', 'antigravity',
                                'kiro', 'kimi', 'zhipu', 'deepseek', 'minimax', 'cursor', 'opencode_go'));
    END IF;

    SELECT pg_get_constraintdef(c.oid)
      INTO template_constraint_def
      FROM pg_constraint c
      JOIN pg_class t ON t.oid = c.conrelid
     WHERE t.relname = 'channel_monitor_request_templates'
       AND c.conname = 'channel_monitor_request_templates_provider_check';

    IF template_constraint_def IS NULL OR position('opencode_go' IN template_constraint_def) = 0 THEN
        ALTER TABLE channel_monitor_request_templates
            DROP CONSTRAINT IF EXISTS channel_monitor_request_templates_provider_check;
        ALTER TABLE channel_monitor_request_templates
            ADD CONSTRAINT channel_monitor_request_templates_provider_check
            CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok', 'antigravity',
                                'kiro', 'kimi', 'zhipu', 'deepseek', 'minimax', 'cursor', 'opencode_go'));
    END IF;
END $$;
