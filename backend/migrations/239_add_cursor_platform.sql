-- 把新增的 cursor 平台加进平台/供应商 CHECK 约束：
--   1. user_platform_quotas.platform CHECK
--   2. composite_model_routes.target_platform CHECK
--   3. channel_monitors / channel_monitor_request_templates.provider CHECK
--
-- 与 224 / 237 / 238 同型的坑：这四处约束把平台白名单硬编码在 SQL 字符串里，
-- Go 侧任何编译期检查都盖不到。cursor 接入时服务层已经放行
-- （composite_platform.go 的 isConcreteRequestPlatform 显式列了 cursor），
-- 但库里约束仍是 10 项，于是：
--   - cursor 的 composite 路由在服务层校验通过、INSERT 时被 Postgres 拒绝，
--     管理台只看到一个不知所云的 500；
--   - 注册预填充默认配额时 cursor 行违约，会中止整条 INSERT；
--   - cursor 渠道监控无法创建。
-- 单元测试全绿也挡不住——它们不连 Postgres。
--
-- 写法沿用 227/229/238：DROP IF EXISTS 后重建超集约束，存量行瞬时校验通过。
-- 列表取 ent schema 的枚举终态（domain 层权威），即 238 的 10 项再并上 cursor。

ALTER TABLE user_platform_quotas
    DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;

ALTER TABLE user_platform_quotas
    ADD CONSTRAINT user_platform_quotas_platform_check
    CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'kiro', 'grok',
                        'kimi', 'zhipu', 'deepseek', 'minimax', 'cursor'));

ALTER TABLE composite_model_routes
    DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;

ALTER TABLE composite_model_routes
    ADD CONSTRAINT composite_model_routes_target_platform_check
    CHECK (target_platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'kiro', 'grok',
                               'kimi', 'zhipu', 'deepseek', 'minimax', 'cursor'));

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

    IF monitor_constraint_def IS NULL OR position('cursor' IN monitor_constraint_def) = 0 THEN
        ALTER TABLE channel_monitors
            DROP CONSTRAINT IF EXISTS channel_monitors_provider_check;
        ALTER TABLE channel_monitors
            ADD CONSTRAINT channel_monitors_provider_check
            CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok', 'antigravity',
                                'kiro', 'kimi', 'zhipu', 'deepseek', 'minimax', 'cursor'));
    END IF;

    SELECT pg_get_constraintdef(c.oid)
      INTO template_constraint_def
      FROM pg_constraint c
      JOIN pg_class t ON t.oid = c.conrelid
     WHERE t.relname = 'channel_monitor_request_templates'
       AND c.conname = 'channel_monitor_request_templates_provider_check';

    IF template_constraint_def IS NULL OR position('cursor' IN template_constraint_def) = 0 THEN
        ALTER TABLE channel_monitor_request_templates
            DROP CONSTRAINT IF EXISTS channel_monitor_request_templates_provider_check;
        ALTER TABLE channel_monitor_request_templates
            ADD CONSTRAINT channel_monitor_request_templates_provider_check
            CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok', 'antigravity',
                                'kiro', 'kimi', 'zhipu', 'deepseek', 'minimax', 'cursor'));
    END IF;
END $$;
