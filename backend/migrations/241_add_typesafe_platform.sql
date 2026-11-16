-- Add TypeSafe (Jev System One) as a first-class platform.
--
-- 1. user_platform_quotas.platform CHECK
-- 2. composite_model_routes.target_platform CHECK
--
-- TypeSafe 不是对话模型，不进入渠道监控 provider，因此 channel_monitors /
-- channel_monitor_request_templates 的约束保持不变。
--
-- Runs after 238_opencode_go_platform.sql. DROP ... IF EXISTS 保证可重入；
-- 新约束必须是当前全部平台的超集：本 fork 的 238_restore_kiro / 239_add_cursor
-- 已把 kiro、cursor 加进约束，上游原版漏了这两项，存量 kiro 配额行会让
-- ADD CONSTRAINT 直接失败、阻断启动。上游原版 checksum 见 migrations_runner.go
-- 的兼容规则；两项约束随后由 242 删除，改由应用层平台清单校验。

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
