package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestForkPlatformConstraintsSupersetMigration(t *testing.T) {
	const name = "239_fork_platform_constraints_superset.sql"
	content, err := FS.ReadFile(name)
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql,
		"CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'kiro', 'grok', 'kimi', 'zhipu', 'deepseek', 'minimax', 'cursor', 'adobe', 'opencode_go', 'typesafe', 'command_code', 'cline', 'leonardo', 'fish'))")
	require.Contains(t, sql,
		"CHECK (target_platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'kiro', 'grok', 'kimi', 'zhipu', 'deepseek', 'minimax', 'cursor', 'adobe', 'opencode_go', 'typesafe', 'command_code', 'cline', 'leonardo', 'fish'))")
	require.Contains(t, sql,
		"ADD CONSTRAINT channel_monitors_provider_check CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok', 'antigravity', 'kiro', 'kimi', 'zhipu', 'deepseek', 'minimax', 'cursor', 'opencode_go'))")
	require.Contains(t, sql,
		"ADD CONSTRAINT channel_monitor_request_templates_provider_check CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok', 'antigravity', 'kiro', 'kimi', 'zhipu', 'deepseek', 'minimax', 'cursor', 'opencode_go'))")

	// 239 必须晚于旧版 Cursor CHECK 重建迁移；之后的 241 provider CHECK
	// 仍须包含完整 provider 集合，由 TestChannelMonitorProviderCheckFinalStateCoversAllProviders 校验。
	require.Greater(t, "239_fork_platform_constraints_superset.sql", "239_add_cursor_platform.sql")
}
