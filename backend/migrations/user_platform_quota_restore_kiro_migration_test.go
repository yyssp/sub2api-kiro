package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// expectedUserPlatformQuotaPlatforms 是 user_platform_quotas.platform 允许的平台集合，
// 需与 service.AllowedQuotaPlatforms 及 ent/schema/user_platform_quota.go 的 Validate 同步。
// 此处硬编码而非 import service：migrations 是叶子包，被 repository 依赖，反向 import 会成环。
// 不含 typesafe：它只由上游 241_add_typesafe_platform 引入，该迁移按文件名紧排在本 fork 的
// 241_platform_checks_include_opencode_go 之前；新库此时没有 typesafe 行，存量 fork 库上后者
// 早已应用而被跳过，随后 242 删除约束。因此 242 之前最后一次重建无需覆盖 typesafe。
var expectedUserPlatformQuotaPlatforms = []string{
	"anthropic", "antigravity", "cursor", "deepseek", "gemini", "grok", "kimi", "kiro", "minimax", "openai", "opencode_go", "zhipu",
}

// TestUserPlatformQuotasRestoreKiroMigration 校验 227 号迁移把 224 漏掉的 kiro
// 重新加回 user_platform_quotas.platform 的 CHECK 约束。
//
// kiro 自 145 号迁移起就在约束内，224 号用 DROP + 重建的写法加国产 3 家时漏掉了它，
// 等于回退了 145。约束缺 kiro 时，注册预填充 9 平台默认配额会因该行违约中止整条
// INSERT → 快照 fail-open 仅 warn → 新用户零配额行（缺失配额行 = 无限额）。
func TestUserPlatformQuotasRestoreKiroMigration(t *testing.T) {
	content, err := FS.ReadFile("227_user_platform_quotas_restore_kiro.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check")
	require.Contains(t, sql,
		"CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'kiro', 'grok', 'kimi', 'zhipu', 'deepseek'))")
}

// TestUserPlatformQuotaPlatformCheckFinalStateCoversAllPlatforms 是防回归护栏：
// 按文件名顺序（= 迁移执行顺序）取最后一个重建 user_platform_quotas_platform_check
// 的迁移，断言其平台列表覆盖全部允许平台。
//
// 224 的事故根因是 DROP + 重建时手抄平台列表漏了一项，而针对 224 自身的用例只断言
// 「新增的 kimi/zhipu/deepseek 在列表里」，无法发现旧平台被删。此用例校验终态，
// 任何后续重建该约束却漏平台的迁移都会在这里失败。
func TestUserPlatformQuotaPlatformCheckFinalStateCoversAllPlatforms(t *testing.T) {
	assertPlatformCheckFinalState(t, "user_platform_quotas_platform_check", "platform", expectedUserPlatformQuotaPlatforms)
}
