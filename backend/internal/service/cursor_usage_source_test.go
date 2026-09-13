//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// ⚠️ usage_source 回答的问题是「这笔账单是估出来的还是实测的」。
//
// Cursor 的 agent.v1 不返回任何 token 用量字段，整条链路按网关侧分词器推算。
// 不标记的话，估算行和实测行写进 usage_logs 后完全无法区分——一旦估算口径
// 出偏差（本次就修过一次中文高估 2~4 倍），没有任何办法圈出受影响的范围。

func TestUsageSource_CursorIsMarkedEstimated(t *testing.T) {
	got := usageSourceForPlatform(&Account{ID: 1, Platform: PlatformCursor})
	require.NotNil(t, got, "Cursor 按估算值计费却没有标记来源")
	require.Equal(t, usageSourceEstimated, *got)
}

// ⚠️ 其余平台必须留 nil，不能写 "upstream"。
//
// 给所有行都打标记会让这一列失去信息量：它要能直接回答「哪些账单是估出来的」，
// 而不是「哪些行被标记过」。NULL 即表示未声明，按 upstream 解读。
func TestUsageSource_OtherPlatformsStayNil(t *testing.T) {
	for _, p := range []string{PlatformAnthropic, PlatformKiro, PlatformOpenAI} {
		require.Nil(t, usageSourceForPlatform(&Account{ID: 2, Platform: p}),
			"平台 %s 不按估算计费，不应写入 usage_source", p)
	}
}

func TestUsageSource_NilAccountIsSafe(t *testing.T) {
	require.Nil(t, usageSourceForPlatform(nil))
}

// ⚠️ 返回的必须是独立指针，不能让多行共享同一个可变变量。
func TestUsageSource_ReturnsIndependentPointers(t *testing.T) {
	a := usageSourceForPlatform(&Account{ID: 3, Platform: PlatformCursor})
	b := usageSourceForPlatform(&Account{ID: 4, Platform: PlatformCursor})
	require.NotNil(t, a)
	require.NotNil(t, b)
	*a = "mutated"
	require.Equal(t, usageSourceEstimated, *b, "两行共享了同一个指针")
}
