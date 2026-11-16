package migrations

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDropPlatformCheckConstraintsMigration(t *testing.T) {
	content, err := FS.ReadFile("242_drop_platform_check_constraints.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql,
		"ALTER TABLE user_platform_quotas DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;")
	require.Contains(t, sql,
		"ALTER TABLE composite_model_routes DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;")
	require.NotContains(t, sql, "ADD CONSTRAINT")
	require.NotContains(t, sql, "DROP CONSTRAINT IF EXISTS channel_monitors_provider_check")
	require.NotContains(t, sql, "DROP CONSTRAINT IF EXISTS channel_monitor_request_templates_provider_check")
}

func TestPlatformCheckTransitionMigrationsKeepAllPlatforms(t *testing.T) {
	const platformSet = "'anthropic', 'openai', 'gemini', 'antigravity', 'kiro', 'grok', 'kimi', 'zhipu', 'deepseek', 'minimax', 'cursor', 'adobe', 'opencode_go', 'typesafe', 'command_code', 'cline', 'leonardo', 'fish'"
	for _, name := range []string{
		"238_opencode_go_platform.sql",
		"239_fork_platform_constraints_superset.sql",
		"241_add_typesafe_platform.sql",
		"241_platform_checks_include_opencode_go.sql",
	} {
		t.Run(name, func(t *testing.T) {
			content, err := FS.ReadFile(name)
			require.NoError(t, err)
			sql := strings.Join(strings.Fields(string(content)), " ")
			require.Contains(t, sql, "CHECK (platform IN ("+platformSet+")",
				"quota CHECK must accept all current and historical values until migration 242 removes it")
			require.Contains(t, sql, "CHECK (target_platform IN ("+platformSet+")",
				"composite CHECK must accept all current and historical values until migration 242 removes it")
		})
	}
}

// Platform membership is validated against the application catalog after 242.
// A later migration must not restore a database whitelist that can drift from it.
func TestLaterMigrationsDoNotRestorePlatformCheckConstraints(t *testing.T) {
	const droppedAt = "242_drop_platform_check_constraints.sql"
	entries, err := fs.ReadDir(FS, ".")
	require.NoError(t, err)
	comments := regexp.MustCompile(`(?ms)/\*.*?\*/|--[^\n]*`)
	addConstraint := regexp.MustCompile(`(?i)\bADD\s+CONSTRAINT\s+"?(user_platform_quotas_platform_check|composite_model_routes_target_platform_check)\b`)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() <= droppedAt {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			content, err := FS.ReadFile(entry.Name())
			require.NoError(t, err)
			sql := comments.ReplaceAll(content, nil)
			require.Nil(t, addConstraint.Find(sql), "platform CHECK constraints removed in 242 must remain managed by application validation")
		})
	}
}
