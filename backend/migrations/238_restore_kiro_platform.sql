-- 把 237 漏掉的 kiro 重新加回平台/供应商 CHECK 约束：
--   1. user_platform_quotas.platform CHECK
--   2. composite_model_routes.target_platform CHECK
--   3. channel_monitors / channel_monitor_request_templates.provider CHECK
--
-- 237 号迁移用 DROP + 重建的写法把 minimax 加进白名单时手抄列表漏了 kiro，
-- 等于回退了 145/227/229——与 224 的事故同型。约束缺 kiro 时：
--   - 注册预填充 10 平台默认配额会因该行违约中止整条 INSERT（新用户零配额行 = 无限额）；
--   - kiro 的 composite 路由无法保存；
--   - kiro 渠道监控无法创建。
--
-- 与 227/229 同型：DROP IF EXISTS 后重建超集约束，存量行瞬时校验通过。
-- 列表取 ent schema 的枚举终态（domain 层权威），即 237 的 9 项再并上 kiro。

ALTER TABLE user_platform_quotas
    DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;

ALTER TABLE user_platform_quotas
    ADD CONSTRAINT user_platform_quotas_platform_check
    CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'kiro', 'grok',
                        'kimi', 'zhipu', 'deepseek', 'minimax'));

ALTER TABLE composite_model_routes
    DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;

ALTER TABLE composite_model_routes
    ADD CONSTRAINT composite_model_routes_target_platform_check
    CHECK (target_platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'kiro', 'grok',
                               'kimi', 'zhipu', 'deepseek', 'minimax'));

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

    IF monitor_constraint_def IS NULL OR position('kiro' IN monitor_constraint_def) = 0 THEN
        ALTER TABLE channel_monitors
            DROP CONSTRAINT IF EXISTS channel_monitors_provider_check;
        ALTER TABLE channel_monitors
            ADD CONSTRAINT channel_monitors_provider_check
            CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok', 'antigravity',
                                'kiro', 'kimi', 'zhipu', 'deepseek', 'minimax'));
    END IF;

    SELECT pg_get_constraintdef(c.oid)
      INTO template_constraint_def
      FROM pg_constraint c
      JOIN pg_class t ON t.oid = c.conrelid
     WHERE t.relname = 'channel_monitor_request_templates'
       AND c.conname = 'channel_monitor_request_templates_provider_check';

    IF template_constraint_def IS NULL OR position('kiro' IN template_constraint_def) = 0 THEN
        ALTER TABLE channel_monitor_request_templates
            DROP CONSTRAINT IF EXISTS channel_monitor_request_templates_provider_check;
        ALTER TABLE channel_monitor_request_templates
            ADD CONSTRAINT channel_monitor_request_templates_provider_check
            CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok', 'antigravity',
                                'kiro', 'kimi', 'zhipu', 'deepseek', 'minimax'));
    END IF;
END $$;
