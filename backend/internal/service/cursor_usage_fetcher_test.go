//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

// 本文件断言三桶额度的**状态判定**，这是 Cursor 接入里最容易出错的一块：
// 把「上游没返回字段」判成「额度耗尽」会误清空号池；
// 反过来判成「可用」会让请求反复撞上游 quota 错误。

func TestCursorBucketFromPercent_RequestFailedIsNotZeroPercent(t *testing.T) {
	// ⚠️ 端点请求失败时，PeriodUsage 是零值：AutoPercent=0。
	// 若不先看 OK，就会把"没拿到"读成"0% 已用 → 额度充足"，
	// 把一个已耗尽的账号重新放回调度池。
	b := cursorBucketFromPercent(cursor.PeriodUsage{OK: false}, 0)
	require.Equal(t, CursorQuotaStateRequestFailed, b.State)
	require.NotEqual(t, CursorQuotaStateAvailable, b.State)
}

func TestCursorBucketFromPercent_AvailableAndExhausted(t *testing.T) {
	ok := cursor.PeriodUsage{OK: true}

	avail := cursorBucketFromPercent(ok, 42.5)
	require.Equal(t, CursorQuotaStateAvailable, avail.State)
	require.InDelta(t, 42.5, avail.Percent, 0.001)

	exhausted := cursorBucketFromPercent(ok, 100)
	require.Equal(t, CursorQuotaStateExhausted, exhausted.State)

	// 超过 100% 同样是耗尽，不是"回绕"。
	over := cursorBucketFromPercent(ok, 137)
	require.Equal(t, CursorQuotaStateExhausted, over.State)
}

func TestCursorBucketFromSand_UnknownWhenUpstreamOmitsFields(t *testing.T) {
	// 200 但没有 entitlement 字段 → unknown，不能捏造成 "0% 且不可用"。
	b := cursorBucketFromSand(cursor.SandUsage{OK: true})
	require.Equal(t, CursorQuotaStateUnknown, b.State)
	require.False(t, b.Enabled)
}

func TestCursorBucketFromSand_ExhaustedWinsOverHasAvailable(t *testing.T) {
	// ⚠️ 已知上游 bug：额度到 100% 时仍可能返回 hasAvailableUsage=true。
	// 百分比是硬上限，必须优先判 exhausted——这个修正在协议层
	// EffectiveState() 里，service 层复用它而不是重新推导。
	b := cursorBucketFromSand(cursor.SandUsage{
		OK:                  true,
		UsagePercent:        100,
		UsagePercentPresent: true,
		HasAvailable:        true,
		HasAvailablePresent: true,
	})
	require.Equal(t, CursorQuotaStateExhausted, b.State)
	require.False(t, b.Enabled, "耗尽的桶不应标记为 enabled")
}

func TestCursorBucketFromSand_AvailableSetsEnabled(t *testing.T) {
	b := cursorBucketFromSand(cursor.SandUsage{
		OK:                  true,
		UsagePercent:        10,
		UsagePercentPresent: true,
		HasAvailable:        true,
		HasAvailablePresent: true,
	})
	require.Equal(t, CursorQuotaStateAvailable, b.State)
	require.True(t, b.Enabled)
	require.InDelta(t, 10, b.Percent, 0.001)
}

func TestCursorBucketFromSand_RequestFailed(t *testing.T) {
	b := cursorBucketFromSand(cursor.SandUsage{OK: false})
	require.Equal(t, CursorQuotaStateRequestFailed, b.State)
}

func TestCursorQuotaIsStale_NeverFetchedIsStale(t *testing.T) {
	// ⚠️ FetchedAt=nil 表示"从未抓取"，绝不能当成"刚抓过且 0% 可用"。
	require.True(t, CursorQuotaIsStale(CursorQuota{}, time.Now()))
}

func TestCursorQuotaIsStale_FreshAndExpired(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-time.Minute)
	old := now.Add(-2 * cursorQuotaStaleAfter)

	require.False(t, CursorQuotaIsStale(CursorQuota{FetchedAt: &fresh}, now))
	require.True(t, CursorQuotaIsStale(CursorQuota{FetchedAt: &old}, now))
}

func TestCursorUsageFetcher_RejectsMissingToken(t *testing.T) {
	f := &CursorUsageFetcher{client: sharedCursorClient()}
	q, err := f.Fetch(t.Context(), &Account{ID: 1, Platform: PlatformCursor}, "")
	require.Error(t, err)
	// 即便失败也要返回完整三桶结构，调用方才能区分"哪个桶没拿到"。
	require.Equal(t, CursorQuotaStateUnknown, q.Cursor.State)
	require.Equal(t, CursorQuotaStateUnknown, q.Other.State)
	require.Equal(t, CursorQuotaStateUnknown, q.GrokBot.State)
}

func TestCursorUsageFetcher_NilAccount(t *testing.T) {
	f := &CursorUsageFetcher{client: sharedCursorClient()}
	_, err := f.Fetch(t.Context(), nil, "tok")
	require.Error(t, err)
}

// TestCursorQuotaRoundTripsThroughExtra 串起「拉取 → 写 extra → 读回 → 判可调度」
// 的完整链路，确认三桶状态经 JSONB 往返后不会退化。
func TestCursorQuotaRoundTripsThroughExtra(t *testing.T) {
	now := time.Now().UTC()
	acc := &Account{ID: 5, Platform: PlatformCursor, Type: AccountTypeOAuth, Schedulable: true, Status: StatusActive}

	writeCursorQuota(acc, CursorQuota{
		Cursor:    CursorBucketQuota{State: CursorQuotaStateExhausted, Percent: 100},
		Other:     CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 3},
		GrokBot:   CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 1, Enabled: true},
		FetchedAt: &now,
	})

	got := readCursorQuota(acc)
	require.Equal(t, CursorQuotaStateExhausted, got.Cursor.State)
	require.Equal(t, CursorQuotaStateAvailable, got.Other.State)
	require.True(t, got.GrokBot.Enabled)
	require.NotNil(t, got.FetchedAt)

	// ⚠️ 关键断言：cursor 桶耗尽**不等于**整号不可调度。
	// 走 other 桶的模型必须仍然可调度，否则一个桶耗尽就废掉整个账号。
	require.False(t, CursorAccountUsableForModel(acc, "auto"),
		"cursor 桶已耗尽，auto 不应可调度")
	require.True(t, CursorAccountUsableForModel(acc, "gpt-5"),
		"other 桶仍可用，不能因 cursor 桶耗尽就整号停用")
}
