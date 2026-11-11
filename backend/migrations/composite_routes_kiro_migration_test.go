package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// expectedCompositeRouteTargetPlatforms 是 composite_model_routes.target_platform
// 允许的平台集合，需与 service.isConcreteRequestPlatform
// （internal/service/composite_platform.go）同步。
// 此处硬编码而非 import service：migrations 是叶子包，被 repository 依赖，
// 反向 import 会成环。
// 不含 typesafe：它只由上游 241_add_typesafe_platform 引入，该迁移按文件名紧排在本 fork 的
// 241_platform_checks_include_opencode_go 之前；新库此时没有 typesafe 行，存量 fork 库上后者
// 早已应用而被跳过，随后 242 删除约束。因此 242 之前最后一次重建无需覆盖 typesafe。
var expectedCompositeRouteTargetPlatforms = []string{
	"anthropic", "antigravity", "cursor", "deepseek", "gemini", "grok", "kimi", "kiro", "minimax", "openai", "opencode_go", "zhipu",
}

const compositeRouteTargetPlatformConstraint = "composite_model_routes_target_platform_check"

// TestCompositeRoutesKiroMigration 校验 229 号迁移把 227 漏掉的 kiro 加进
// composite_model_routes.target_platform 的 CHECK 约束。
//
// 227 用 DROP + 重建的写法把国产 3 家加入该约束时，重建的列表里没有本 fork
// 独有的 kiro，导致管理端保存 target_platform='kiro' 的 composite 路由违约失败。
func TestCompositeRoutesKiroMigration(t *testing.T) {
	content, err := FS.ReadFile("229_composite_routes_add_kiro.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "DROP CONSTRAINT IF EXISTS "+compositeRouteTargetPlatformConstraint)
	require.Contains(t, sql,
		"CHECK (target_platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'kiro', 'grok', 'kimi', 'zhipu', 'deepseek'))")
}

// TestCompositeRouteTargetPlatformFinalStateCoversAllPlatforms 是防回归护栏：
// 按文件名顺序（= 迁移执行顺序）取最后一个重建该约束的迁移，断言其平台列表
// 覆盖全部允许平台。
//
// 227 的问题根因是 DROP + 重建时手抄平台列表漏了 kiro，而针对 227 自身的用例
// 只断言完整字符串匹配当时的 8 平台，无法表达「必须覆盖全集」。此用例校验终态，
// 任何后续重建该约束却漏平台的迁移都会在这里失败。
func TestCompositeRouteTargetPlatformFinalStateCoversAllPlatforms(t *testing.T) {
	assertPlatformCheckFinalState(t, compositeRouteTargetPlatformConstraint, "target_platform", expectedCompositeRouteTargetPlatforms)
}
