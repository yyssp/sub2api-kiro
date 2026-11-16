-- 统一配额/组合路由平台 CHECK 为当前完整平台全集。
--
-- 迁移按文件名排序执行：238_opencode_go_platform 先于 238_restore_kiro_platform、
-- 239_add_cursor_platform 运行，而后两者各自用 DROP + 重建写入不含 opencode_go 的列表，
-- 新库约束终态因此缺 opencode_go。保留完整平台全集，避免过渡阶段拒绝已有平台行；
-- 242 随后移除这两项 CHECK。
--
-- 写法沿用 227/229/238/239：DROP IF EXISTS 后重建超集约束，存量行瞬时校验通过。
-- 列表取 ent schema 的枚举终态。

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

ALTER TABLE channel_monitors
    DROP CONSTRAINT IF EXISTS channel_monitors_provider_check;

ALTER TABLE channel_monitors
    ADD CONSTRAINT channel_monitors_provider_check
    CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok', 'antigravity',
                        'kiro', 'kimi', 'zhipu', 'deepseek', 'minimax', 'cursor', 'opencode_go'));

ALTER TABLE channel_monitor_request_templates
    DROP CONSTRAINT IF EXISTS channel_monitor_request_templates_provider_check;

ALTER TABLE channel_monitor_request_templates
    ADD CONSTRAINT channel_monitor_request_templates_provider_check
    CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok', 'antigravity',
                        'kiro', 'kimi', 'zhipu', 'deepseek', 'minimax', 'cursor', 'opencode_go'));
