//go:build unit

package service

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
	"github.com/stretchr/testify/require"
)

// ⚠️ 本组测试钉住的是 Cursor 失败时的**调度契约**。
//
// handler 的 failover 循环靠 errors.As(*UpstreamFailoverError) 识别可切换的
// 上游失败。Cursor 接入初期一律返回 fmt.Errorf，于是：
//
//   - 上游 429/凭证失效不会触发换号，客户端直接吃到错误；
//   - 账号不会进冷却，下一个请求还会选中同一个坏号，循环踩雷；
//   - 号池里其它健康账号完全用不上。
//
// 这些都不是编译期能发现的：返回 error 接口，类型不匹配只是"没进那个分支"。

func TestCursorFailoverError_QuotaExhaustedSwitchesAccount(t *testing.T) {
	classified := cursorErrorClassification{
		Category:    cursorErrorQuotaExhausted,
		StatusCode:  http.StatusTooManyRequests,
		ErrorType:   "rate_limit_error",
		Message:     "usage limit reached",
		Kind:        cursor.ErrQuota,
		QuotaBucket: cursor.QuotaBucketCursor,
	}

	err := cursorFailoverError(classified, false)
	require.NotNil(t, err)
	require.True(t, err.ShouldRetryNextAccount(),
		"额度耗尽必须换号：当前号该桶已满，换个号就能继续服务")
	require.Equal(t, http.StatusTooManyRequests, err.StatusCode)

	// ⚠️ 额度耗尽不得在同一账号上重试：桶已经满了，重试必然再失败，
	// 只是白白延长客户端等待。
	require.False(t, err.RetryableOnSameAccount,
		"额度耗尽在同账号重试必然再次失败")
}

func TestCursorFailoverError_AuthFailureSwitchesAccount(t *testing.T) {
	err := cursorFailoverError(cursorErrorClassification{
		Category:   cursorErrorAuthError,
		StatusCode: http.StatusServiceUnavailable,
		ErrorType:  "api_error",
		Message:    "unauthorized",
		Kind:       cursor.ErrAuth,
	}, false)

	require.NotNil(t, err)
	require.True(t, err.ShouldRetryNextAccount(),
		"凭证失效必须换号：该号已被 SetError 停用，继续用它只会连续失败")
	require.False(t, err.RetryableOnSameAccount,
		"凭证失效在同账号重试无意义")
}

// ⚠️ 这是本组最关键的一条：终端协议错误绝不能换号。
//
// 这四个哨兵（未声明工具 / 畸形工具负载 / 流提前结束 / 请求被拒）表达的是
// 「这个请求的协议形状不可服务」，与账号健康无关。换号重试会让同一个
// 确定性失败的请求被逐个打给号池里的每一个账号——单个坏请求烧穿整个号池，
// 并且每个号都会记一次失败。
func TestCursorFailoverError_TerminalProtocolErrorDoesNotSwitchAccount(t *testing.T) {
	err := cursorFailoverError(cursorErrorClassification{
		Category:   cursorErrorProtocol,
		StatusCode: http.StatusBadGateway,
		ErrorType:  "api_error",
		Message:    "malformed tool payload",
		Kind:       cursor.ErrTransient,
		Terminal:   true,
	}, false)

	require.NotNil(t, err)
	require.False(t, err.ShouldRetryNextAccount(),
		"终端协议错误换号会让一个坏请求烧穿整个号池")
	require.False(t, err.RetryableOnSameAccount,
		"终端协议错误是确定性失败，同账号重试同样无意义")
}

// ⚠️ 模型不可用 / 套餐不允许命名模型：换号也不会变好。
//
// 这是「这个模型对这个套餐不可用」，号池里同套餐的账号结果完全一样；
// 而且它是 400，属于请求问题，不该消耗 failover 预算。
func TestCursorFailoverError_BadModelDoesNotBurnAccountPool(t *testing.T) {
	for _, category := range []string{cursorErrorBadModel, cursorErrorNamedModelDenied} {
		err := cursorFailoverError(cursorErrorClassification{
			Category:   category,
			StatusCode: http.StatusBadRequest,
			ErrorType:  "invalid_request_error",
			Message:    "model not available",
			Kind:       cursor.ErrTransient,
		}, false)

		require.NotNil(t, err, "category=%s", category)
		require.False(t, err.ShouldRetryNextAccount(),
			"category=%s：模型类错误换号结果完全一样，不该消耗号池", category)
	}
}

// 上游瞬时故障（5xx/网络抖动）应当换号：下一个账号可能落在健康的上游实例上。
func TestCursorFailoverError_TransientSwitchesAccount(t *testing.T) {
	err := cursorFailoverError(cursorErrorClassification{
		Category:   cursorErrorUpstreamTransient,
		StatusCode: http.StatusServiceUnavailable,
		ErrorType:  "api_error",
		Message:    "upstream unavailable",
		Kind:       cursor.ErrTransient,
	}, false)

	require.NotNil(t, err)
	require.True(t, err.ShouldRetryNextAccount())
}

// ⚠️ 流已经推出真实内容后，绝不能标记为"写出后仍可切换"。
//
// 客户端已经收到 message_start / content_block_delta，换号重来会让它
// 在同一个流里收到第二份 message_start——协议被破坏，且前半段内容已无法撤回。
func TestCursorFailoverError_NotSafeToFailoverAfterRealOutput(t *testing.T) {
	err := cursorFailoverError(cursorErrorClassification{
		Category:   cursorErrorUpstreamTransient,
		StatusCode: http.StatusServiceUnavailable,
		Kind:       cursor.ErrTransient,
	}, true)

	require.NotNil(t, err)
	require.False(t, err.SafeToFailoverAfterWrite,
		"已推出真实内容还允许切换，会让客户端在一个流里收到两份 message_start")
}

// ⚠️ 反过来：只写了心跳注释时必须允许切换。
//
// A2 的保活心跳写的是 SSE 注释（": processing"），注释按规范被客户端忽略，
// 不构成任何语义输出。若因为"已经写过字节"就放弃切换，等于首字前的
// 任何一次上游抖动都直接失败——而保活心跳恰恰让这种情况变常见。
func TestCursorFailoverError_SafeToFailoverAfterHeartbeatOnly(t *testing.T) {
	err := cursorFailoverError(cursorErrorClassification{
		Category:   cursorErrorUpstreamTransient,
		StatusCode: http.StatusServiceUnavailable,
		Kind:       cursor.ErrTransient,
	}, false)

	require.NotNil(t, err)
	require.True(t, err.SafeToFailoverAfterWrite,
		"只写过 SSE 注释时必须允许切换，否则心跳反而让可恢复的抖动变成失败")
}

// 归类为 nil（无错误）时不得凭空造出一个 failover 错误。
func TestCursorFailoverError_EmptyClassificationIsNil(t *testing.T) {
	require.Nil(t, cursorFailoverError(cursorErrorClassification{}, false))
}
