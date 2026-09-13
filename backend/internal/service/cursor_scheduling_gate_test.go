//go:build unit

package service

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ⚠️ 本组钉的是「三桶额度真的接进了选号」。
//
// 此前 CursorAccountUsableForModel 有完整实现和完整测试，却**零生产调用者**：
// markCursorBucketExhausted 忠实落库，选号却完全不读。结果是耗尽的桶被反复
// 选中，每个请求白烧一次 failover，直到换号预算耗尽才对用户报错。
// 这类缺陷不会让任何测试变红——数据是对的，只是没人用。

func newCursorQuotaAccount(t *testing.T, q CursorQuota) *Account {
	t.Helper()
	acc := &Account{
		ID:          1,
		Platform:    PlatformCursor,
		Type:        AccountTypeOAuth,
		Schedulable: true,
		Status:      StatusActive,
		Credentials: map[string]any{CursorCredMembership: "pro"},
	}
	writeCursorQuota(acc, q)
	return acc
}

// ⚠️ 核心断言：cursor 桶耗尽时，该桶的模型必须被选号排除。
func TestCursorRuntimeSchedulable_ExhaustedBucketIsExcluded(t *testing.T) {
	now := time.Now().UTC()
	acc := newCursorQuotaAccount(t, CursorQuota{
		Cursor:    CursorBucketQuota{State: CursorQuotaStateExhausted, Percent: 100},
		Other:     CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 0},
		GrokBot:   CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 0, Enabled: true},
		FetchedAt: &now,
	})

	s := &GatewayService{}
	require.False(t, s.isCursorRuntimeSchedulable(acc, "auto"),
		"cursor 桶已耗尽却仍被选中，每个请求都要白烧一次 failover 才发现")
}

// ⚠️ 同样关键的反面：单桶耗尽绝不能让整号不可调度。
//
// 三桶相互独立，按账号整体拦截会把可用容量凭空砍掉三分之二。
func TestCursorRuntimeSchedulable_OtherBucketsStayAvailable(t *testing.T) {
	now := time.Now().UTC()
	acc := newCursorQuotaAccount(t, CursorQuota{
		Cursor:    CursorBucketQuota{State: CursorQuotaStateExhausted, Percent: 100},
		Other:     CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 0},
		GrokBot:   CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 0, Enabled: true},
		FetchedAt: &now,
	})

	s := &GatewayService{}
	require.True(t, s.isCursorRuntimeSchedulable(acc, "claude-sonnet-4.5"),
		"cursor 桶耗尽不得影响其它桶的模型——否则可用容量凭空少掉三分之二")
}

// ⚠️ 拿不到目标模型时必须放行。
//
// 无法确定归属哪个桶，此时"猜"一个桶去拦截会把完全可用的账号误判成不可用。
func TestCursorRuntimeSchedulable_EmptyModelFallsOpen(t *testing.T) {
	now := time.Now().UTC()
	acc := newCursorQuotaAccount(t, CursorQuota{
		Cursor:    CursorBucketQuota{State: CursorQuotaStateExhausted, Percent: 100},
		Other:     CursorBucketQuota{State: CursorQuotaStateExhausted, Percent: 100},
		FetchedAt: &now,
	})

	s := &GatewayService{}
	require.True(t, s.isCursorRuntimeSchedulable(acc, ""),
		"模型未知时无法判断归属哪个桶，必须放行而不是瞎猜")
}

// ⚠️ 从未抓取过额度时必须放行（fail-open）。
//
// 反过来会导致刚导入的账号一个都调度不到：额度抓取是异步的，
// 新账号在首次抓取完成前 FetchedAt 为零值。
func TestCursorRuntimeSchedulable_NeverFetchedFallsOpen(t *testing.T) {
	acc := newCursorQuotaAccount(t, CursorQuota{})

	s := &GatewayService{}
	require.True(t, s.isCursorRuntimeSchedulable(acc, "auto"),
		"从未抓取额度的新账号必须可调度，否则导入后到首次抓取完成前完全不可用")
}

// ⚠️ 非 Cursor 账号必须原样放行：这个钩子不能影响其它平台的选号。
func TestCursorRuntimeSchedulable_NonCursorAccountUnaffected(t *testing.T) {
	s := &GatewayService{}
	for _, platform := range []string{PlatformAnthropic, PlatformKiro, PlatformGrok} {
		acc := &Account{ID: 2, Platform: platform, Type: AccountTypeOAuth, Schedulable: true, Status: StatusActive}
		require.True(t, s.isCursorRuntimeSchedulable(acc, "claude-sonnet-4.5"),
			"platform=%s 不得被 Cursor 的额度钩子影响", platform)
	}
	require.True(t, s.isCursorRuntimeSchedulable(nil, "auto"))
}

// ⚠️ 带自定义 base_url 的 Cursor APIKey 账号走用户自有网关，
// 内建三桶额度对它没有意义，必须放行。
func TestCursorRuntimeSchedulable_CustomBaseURLAccountFallsOpen(t *testing.T) {
	now := time.Now().UTC()
	acc := &Account{
		ID: 3, Platform: PlatformCursor, Type: AccountTypeAPIKey,
		Schedulable: true, Status: StatusActive,
		Credentials: map[string]any{"base_url": "https://my-gateway.example"},
	}
	writeCursorQuota(acc, CursorQuota{
		Cursor:    CursorBucketQuota{State: CursorQuotaStateExhausted, Percent: 100},
		FetchedAt: &now,
	})

	s := &GatewayService{}
	require.True(t, s.isCursorRuntimeSchedulable(acc, "auto"),
		"自有网关账号不适用内建三桶额度，拦截它等于凭空禁用一个可用账号")
}

// ⚠️ 接线守卫：钩子必须真的挂在 isAccountSchedulableForModelSelection 上。
//
// 这是模型感知选号的唯一收敛点（13 个调用点都经过它）。写好谓词却不挂上去，
// 就是本次修复之前的状态——功能完备、零效果。
func TestIsAccountSchedulableForModelSelection_ConsultsCursorQuota(t *testing.T) {
	src := readSchedulingSource(t)
	body := sliceBetween(t, src,
		"func (s *GatewayService) isAccountSchedulableForModelSelection(",
		"\nfunc (s *GatewayService) isCursorRuntimeSchedulable(")

	require.Contains(t, body, "s.isCursorRuntimeSchedulable(account, requestedModel)",
		"三桶额度必须挂进模型感知选号的收敛点，否则额度算得再准也没人读")
}

func readSchedulingSource(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("gateway_scheduling.go")
	require.NoError(t, err)
	return string(src)
}
