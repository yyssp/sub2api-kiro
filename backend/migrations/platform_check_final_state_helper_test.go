package migrations

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// platformCheckDroppedAt 起平台白名单改由应用层平台清单（internal/domain/platforms.go）
// 校验，user_platform_quotas / composite_model_routes 的平台 CHECK 被删除；之后的迁移不得
// 再重建它们（见 TestLaterMigrationsDoNotRestorePlatformCheckConstraints）。
const platformCheckDroppedAt = "242_drop_platform_check_constraints.sql"

// assertPlatformCheckFinalState 校验平台 CHECK 约束的终态：最后一个触及该约束的迁移必须是
// 242 的删除；而 242 之前最后一次重建的列表必须覆盖全部平台——存量库在 242 执行前要先跑完
// 这些迁移，任何一次重建漏掉已有数据的平台都会让 ADD CONSTRAINT 失败、阻断启动。
func assertPlatformCheckFinalState(t *testing.T, constraint, column string, want []string) {
	t.Helper()
	entries, err := FS.ReadDir(".")
	require.NoError(t, err)

	var touching []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		body, readErr := FS.ReadFile(name)
		require.NoError(t, readErr)
		if strings.Contains(string(body), constraint) {
			touching = append(touching, name)
		}
	}
	require.NotEmpty(t, touching, "no migration touches %s", constraint)
	sort.Strings(touching) // 与 ApplyMigrations 的执行顺序一致
	require.Equal(t, platformCheckDroppedAt, touching[len(touching)-1],
		"%s 的终态应由 %s 删除，之后不得再有迁移触及它", constraint, platformCheckDroppedAt)

	checkRe := regexp.MustCompile(`CHECK \(` + column + ` IN \(([^)]*)\)\)`)
	var lastRebuild string
	var got []string
	for _, name := range touching[:len(touching)-1] {
		body, readErr := FS.ReadFile(name)
		require.NoError(t, readErr)
		matches := checkRe.FindAllStringSubmatch(strings.Join(strings.Fields(string(body)), " "), -1)
		if len(matches) == 0 {
			continue
		}
		lastRebuild = name
		got = got[:0]
		for _, raw := range strings.Split(matches[len(matches)-1][1], ",") {
			if v := strings.Trim(strings.TrimSpace(raw), "'"); v != "" {
				got = append(got, v)
			}
		}
	}
	require.NotEmpty(t, lastRebuild, "no migration before %s rebuilds %s", platformCheckDroppedAt, constraint)
	sort.Strings(got)
	require.Subset(t, got, want,
		"migration %s 是 %s 之前最后一次重建 %s，其平台列表漏了已有平台；"+
			"存量库上该迁移的 ADD CONSTRAINT 会因已有行违约而失败", lastRebuild, platformCheckDroppedAt, constraint)
}
