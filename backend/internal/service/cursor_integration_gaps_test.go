//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 本文件守护「编译器盖不到」的那批平台钩子：map 字面量、slice 字面量、
// 带 default 的 switch、SQL 字符串。固定长度数组漏改会编译失败，这些不会——
// 它们只在运行时表现为「cursor 账号建得出来但某个功能静默不可用」。

func TestCursorIsAnAllowedQuotaPlatform(t *testing.T) {
	// 漏这个钩子：管理台给用户设 cursor 用量上限时接口返回 400，
	// 且注册预填充默认配额时没有 cursor 行 —— 等于 cursor 不限额（fail-open）。
	require.Contains(t, AllowedQuotaPlatforms, PlatformCursor)
	require.True(t, IsAllowedQuotaPlatform(PlatformCursor))
}

func TestCursorIsARegisteredMonitorProvider(t *testing.T) {
	// 漏这个钩子：创建 cursor 渠道监控时 validateProvider 直接拒绝。
	require.NoError(t, validateProvider(MonitorProviderCursor))
}

func TestMiniMaxIsARegisteredMonitorProvider(t *testing.T) {
	// minimax 此前漏注册（ent enum / 迁移 / 常量都有，唯独校验表没有）。
	require.NoError(t, validateProvider(MonitorProviderMiniMax))
}

func TestCursorIsNotProbeCapable(t *testing.T) {
	// ⚠️ cursor 绝不能进探活表：agent.v1 每次调用都消耗真实额度，
	// 拿探活去烧用户额度是不可接受的。只允许配额模式。
	_, ok := probeCapableProviders[MonitorProviderCursor]
	require.False(t, ok, "cursor 只支持配额模式，不得注册探活")
}

func TestUsageQuotaTiers_CursorThreeBuckets(t *testing.T) {
	now := time.Now().UTC()
	tiers := usageQuotaTiers(&UsageInfo{CursorQuota: &CursorQuota{
		Cursor:    CursorBucketQuota{State: CursorQuotaStateExhausted, Percent: 100},
		Other:     CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 12.5},
		GrokBot:   CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 1},
		FetchedAt: &now,
	}})

	labels := map[string]float64{}
	for _, tier := range tiers {
		require.Equal(t, "billing_period", tier.Window,
			"Cursor 按计费周期结算，不能套用滑动窗口")
		labels[tier.Label] = tier.UsedPercent
	}
	require.Len(t, tiers, 3, "三个桶各出一个 tier")
	require.InDelta(t, 100, labels["cursor"], 0.001)
	require.InDelta(t, 12.5, labels["other"], 0.001)
	require.InDelta(t, 1, labels["grokbot"], 0.001)
}

func TestUsageQuotaTiers_CursorSkipsUnknownAndFailedBuckets(t *testing.T) {
	// ⚠️ 关键断言：状态不明的桶必须被跳过，而不是输出成 "0% 已用"。
	// 输出 0% 会让监控面板把「没拿到数据」显示成「额度充足」。
	tiers := usageQuotaTiers(&UsageInfo{CursorQuota: &CursorQuota{
		Cursor:  CursorBucketQuota{State: CursorQuotaStateUnknown},
		Other:   CursorBucketQuota{State: CursorQuotaStateRequestFailed},
		GrokBot: CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 5},
	}})

	require.Len(t, tiers, 1, "unknown / request_failed 的桶不应产生 tier")
	require.Equal(t, "grokbot", tiers[0].Label)
}

func TestUsageQuotaTiers_NilCursorQuotaIsSafe(t *testing.T) {
	require.NotPanics(t, func() { _ = usageQuotaTiers(&UsageInfo{}) })
}
