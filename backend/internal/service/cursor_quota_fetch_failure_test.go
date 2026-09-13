//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ⚠️ 本组钉的是「抓取失败的桶不得被当成可用」。
//
// 缺陷链路（每一环都已单独核实）：
//  1. CursorUsageFetcher.Fetch 的两个端点各自独立，只有**双双失败**才返回 error；
//     单个端点失败时返回 nil error，该桶为 {State: request_failed, Percent: 0}。
//  2. 调用方据此 writeCursorQuota + 落库，且 FetchedAt 被无条件赋值。
//  3. 投影 UsageAt = *q.FetchedAt → 非零，于是协议层
//     「UsageAt.IsZero() 则放行」的 fail-open 分支被绕过。
//  4. 协议层只看 CursorModelsPct < 100 判定可用 → 0 < 100 → 判为"可用"。
//
// 后果：一个我们**根本没读到**的桶被当成 100% 空闲。更糟的是它会悄悄复活
// markCursorBucketExhausted 刚标成耗尽的桶——先标 100 耗尽，下一轮抓取失败
// 写回 Percent=0，账号重新被选中，每个请求白烧一次 failover。
//
// 这正是 cursor.Account.UsageAt 注释里写明的原则：
// "零值表示从未抓取，不可当作 0% 可用"——request_failed 同属"没读到"。

func newCursorFetchFailureAccount(t *testing.T, q CursorQuota) *Account {
	t.Helper()
	acc := &Account{
		ID:          7,
		Platform:    PlatformCursor,
		Type:        AccountTypeOAuth,
		Schedulable: true,
		Status:      StatusActive,
		Credentials: map[string]any{CursorCredMembership: "pro"},
	}
	writeCursorQuota(acc, q)
	return acc
}

// ⚠️ 核心断言：cursor 桶抓取失败时不得判为可用。
func TestCursorAccountUsableForModel_RequestFailedBucketIsNotUsable(t *testing.T) {
	now := time.Now().UTC()
	acc := newCursorFetchFailureAccount(t, CursorQuota{
		// period 端点挂了：Percent 保持零值，State 记录真实原因。
		Cursor:    CursorBucketQuota{State: CursorQuotaStateRequestFailed},
		Other:     CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 10},
		GrokBot:   CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 0, Enabled: true},
		FetchedAt: &now,
	})

	require.False(t, CursorAccountUsableForModel(acc, "auto"),
		"抓取失败的桶 Percent=0，绝不能当成 0%% 已用：我们根本没读到它的真实用量")
}

// ⚠️ 抓取失败不得复活刚被标记耗尽的桶。
//
// 这是上面那条的时间维度：先 429 标耗尽（Percent=100），
// 随后一次部分失败的抓取把它写回 Percent=0，账号凭空"恢复"。
func TestCursorAccountUsableForModel_FetchFailureDoesNotResurrectExhausted(t *testing.T) {
	now := time.Now().UTC()
	acc := newCursorFetchFailureAccount(t, CursorQuota{
		Cursor:    CursorBucketQuota{State: CursorQuotaStateRequestFailed},
		FetchedAt: &now,
	})

	require.False(t, CursorAccountUsableForModel(acc, "composer"),
		"一次失败的抓取不得把耗尽的桶复活成可用")
}

// ⚠️ unknown（上游没返回该字段）同属"没读到"，一样不能当可用。
func TestCursorAccountUsableForModel_UnknownBucketIsNotUsable(t *testing.T) {
	now := time.Now().UTC()
	acc := newCursorFetchFailureAccount(t, CursorQuota{
		Cursor:    CursorBucketQuota{State: CursorQuotaStateUnknown},
		FetchedAt: &now,
	})

	require.False(t, CursorAccountUsableForModel(acc, "auto"),
		"unknown 表示上游未返回该字段，同样不是 0%% 已用")
}

// ⚠️ 反面护栏：真正读到的 available 桶必须照常可用。
//
// 少了这条，把判定改成"一律不可用"也能让上面三条变绿——
// 那会让所有 Cursor 账号彻底调度不到。
func TestCursorAccountUsableForModel_AvailableBucketStaysUsable(t *testing.T) {
	now := time.Now().UTC()
	acc := newCursorFetchFailureAccount(t, CursorQuota{
		Cursor:    CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 30},
		Other:     CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 40},
		FetchedAt: &now,
	})

	require.True(t, CursorAccountUsableForModel(acc, "auto"),
		"确实读到且未耗尽的桶必须可用")
}

// ⚠️ 从未抓取过仍必须放行：新导入账号在首次抓取完成前 FetchedAt 为零值。
//
// 这条与上面三条张力相反，一起钉住才能防止"把未知一律判死"的过度修复。
func TestCursorAccountUsableForModel_NeverFetchedStillFallsOpen(t *testing.T) {
	acc := newCursorFetchFailureAccount(t, CursorQuota{})

	require.True(t, CursorAccountUsableForModel(acc, "auto"),
		"从未抓取（FetchedAt 为零）必须 fail-open，否则新账号导入后完全不可用")
}
