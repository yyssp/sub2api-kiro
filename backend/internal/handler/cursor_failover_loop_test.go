package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/stretchr/testify/require"
)

// ⚠️ 本组测试跑的是**真实的调度循环**（FailoverState.HandleFailoverError），
// 而不是只断言 UpstreamFailoverError 的字段形状。
//
// 契约字段填对、但调度循环并不按预期反应，是完全可能的：ShouldRetryNextAccount
// 只看 NextAccountAction，而 Scope/Stage 影响的是账号健康度归因。service 层的
// 单测只能证明"字段是这个值"，证明不了"换号真的发生了"。
//
// 这里用 service 包导出的构造结果驱动 handler 的循环，覆盖 Cursor 三类典型失败。

// cursorQuotaFailover 模拟 Cursor 额度耗尽产出的契约（对应 cursorFailoverError
// 在 Kind=ErrQuota 分支的输出）。
func cursorQuotaFailover() *service.UpstreamFailoverError {
	return &service.UpstreamFailoverError{
		StatusCode:               http.StatusTooManyRequests,
		NextAccountAction:        service.NextAccountRetry,
		Scope:                    service.GatewayFailureScopeAccount,
		SafeToFailoverAfterWrite: true,
	}
}

// cursorTerminalFailover 模拟终端协议错误产出的契约。
func cursorTerminalFailover() *service.UpstreamFailoverError {
	return &service.UpstreamFailoverError{
		StatusCode:        http.StatusBadGateway,
		NextAccountAction: service.NextAccountStop,
		Scope:             service.GatewayFailureScopeRequest,
	}
}

// cursorCredentialFailover 模拟取 token 失败产出的凭证级契约。
func cursorCredentialFailoverErr(scope service.GatewayFailureScope) *service.UpstreamFailoverError {
	return &service.UpstreamFailoverError{
		Stage:             service.GatewayFailureStageAccountAuth,
		Scope:             scope,
		NextAccountAction: service.NextAccountRetry,
		ClientStatusCode:  http.StatusServiceUnavailable,
	}
}

// 额度耗尽必须真的换号，并把该账号加入排除集——否则调度器会立刻把它选回来。
func TestCursorFailoverLoop_QuotaExhaustedActuallySwitchesAccount(t *testing.T) {
	fs := NewFailoverState(3, false)
	unscheduler := &mockTempUnscheduler{}

	action := fs.HandleFailoverError(
		context.Background(), unscheduler, 101, service.PlatformCursor, 0, cursorQuotaFailover())

	require.Equal(t, FailoverContinue, action,
		"额度耗尽必须继续换号，而不是结束请求")
	require.Contains(t, fs.FailedAccountIDs, int64(101),
		"失败账号必须进排除集，否则下一轮选号会立刻把同一个已耗尽的号选回来，形成活锁")
	require.Equal(t, 1, fs.SwitchCount)

	// ⚠️ 额度耗尽不是"临时性可同账号重试"错误，不得触发临时封禁：
	// 桶耗尽有自己的 quota 状态与恢复时间，再叠加一层封禁会让账号
	// 在额度恢复后仍然被摘掉。
	require.Empty(t, unscheduler.calls,
		"额度耗尽不应触发 TempUnschedule，配额桶状态已经表达了恢复时机")
}

// ⚠️ 这是最关键的一条：终端协议错误必须**立即结束**，一个号都不许换。
//
// 否则一个形状不合法的请求会被逐个打给号池里的每一个账号，
// 每个号都白记一次失败——单个坏请求烧穿整个号池。
func TestCursorFailoverLoop_TerminalErrorStopsImmediately(t *testing.T) {
	fs := NewFailoverState(5, false)
	unscheduler := &mockTempUnscheduler{}

	action := fs.HandleFailoverError(
		context.Background(), unscheduler, 202, service.PlatformCursor, 0, cursorTerminalFailover())

	require.Equal(t, FailoverExhausted, action,
		"终端协议错误必须立即结束，换号只会把同一个确定性失败打给整个号池")
	require.Equal(t, 0, fs.SwitchCount,
		"终端错误不得消耗任何 failover 预算")
	require.NotContains(t, fs.FailedAccountIDs, int64(202),
		"请求形状问题不该把账号记成失败——账号是健康的")
}

// 凭证失效（账号级）必须换号，让号池里其它健康账号接管。
func TestCursorFailoverLoop_CredentialFailureSwitchesToHealthyAccount(t *testing.T) {
	fs := NewFailoverState(3, false)
	unscheduler := &mockTempUnscheduler{}

	err := cursorCredentialFailoverErr(service.GatewayFailureScopeAccount)
	action := fs.HandleFailoverError(
		context.Background(), unscheduler, 303, service.PlatformCursor, 0, err)

	require.Equal(t, FailoverContinue, action,
		"凭证失效必须换号：这正是号池存在的意义")
	require.True(t, err.IsCredentialFailure())
	require.True(t, err.ShouldReportAccountScheduleFailure(),
		"账号级凭证失效应当计入该账号的调度健康度")
}

// ⚠️ provider 级凭证失败（网络抖动）同样换号，但不得归因到账号。
//
// 归因错了的后果不是当次请求失败，而是调度器长期避开这批完全健康的账号。
func TestCursorFailoverLoop_TransientCredentialFailureDoesNotBlameAccount(t *testing.T) {
	fs := NewFailoverState(3, false)
	unscheduler := &mockTempUnscheduler{}

	err := cursorCredentialFailoverErr(service.GatewayFailureScopeProvider)
	action := fs.HandleFailoverError(
		context.Background(), unscheduler, 404, service.PlatformCursor, 0, err)

	require.Equal(t, FailoverContinue, action)
	require.False(t, err.ShouldReportAccountScheduleFailure(),
		"网络抖动导致的凭证失败不得计入账号健康度，否则故障恢复后仍会持续避开这些号")
}

// 换号次数必须受 MaxSwitches 约束：连续失败时最终要结束，不能无限换下去。
func TestCursorFailoverLoop_ExhaustsAfterMaxSwitches(t *testing.T) {
	fs := NewFailoverState(2, false)
	unscheduler := &mockTempUnscheduler{}

	require.Equal(t, FailoverContinue, fs.HandleFailoverError(
		context.Background(), unscheduler, 1, service.PlatformCursor, 0, cursorQuotaFailover()))
	require.Equal(t, FailoverContinue, fs.HandleFailoverError(
		context.Background(), unscheduler, 2, service.PlatformCursor, 0, cursorQuotaFailover()))

	// 第三次：SwitchCount 已达上限，必须结束而不是继续消耗号池。
	require.Equal(t, FailoverExhausted, fs.HandleFailoverError(
		context.Background(), unscheduler, 3, service.PlatformCursor, 0, cursorQuotaFailover()),
		"换号次数用尽后必须结束请求，否则一个持续失败的请求会耗尽整个号池")
}

// ⚠️ 客户端已断开时不得再换号：用已取消的 context 重新选号必然失败，
// 还会被误报成"账号耗尽"（502），污染错误看板。
func TestCursorFailoverLoop_CanceledClientStopsFailover(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fs := NewFailoverState(3, false)
	action := fs.HandleFailoverError(
		ctx, &mockTempUnscheduler{}, 505, service.PlatformCursor, 0, cursorQuotaFailover())

	require.Equal(t, FailoverCanceled, action,
		"客户端已断开时继续换号只会产生必然失败的请求，并误报成账号耗尽")
}
