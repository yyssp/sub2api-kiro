//go:build unit

package service

import (
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// ⚠️ 本组钉的是「谁来写响应体」这个边界。
//
// cursor_failover.go 把归类翻译成调度契约，但契约只有在**调用点正确接线**时
// 才有意义。两个调用点各有一个不会被编译器发现的坑：
//
//   - 非流式：自己写 c.JSON 后再返回 failover 错误 → handler 换号成功时
//     客户端会先收到一份错误体、再收到正常结果，两份 body 拼在一条响应里；
//   - 流式：心跳已经写过字节，handler 必然走 handleFailoverExhausted 并**自己**
//     补一帧 SSE error；此处再 failMidStream 就是第二帧 error。
//
// 这两种都不会让任何测试挂掉——响应照常返回 200，只是内容是坏的。

// ⚠️ handler 判定能否换号看的是 c.Writer.Size() 变化，不是 SafeToFailoverAfterWrite
// （后者只有 OpenAI 侧 handler 读）。非流式路径必须一个字节都不写，
// 否则 failover 直接失效：一个坏号会把请求打死在自己身上，号池形同虚设。
func TestBlockCursorMessages_DoesNotWriteBodyOnUpstreamError(t *testing.T) {
	src := readCursorRuntimeSource(t)
	body := sliceBetween(t, src, "func (s *GatewayService) blockCursorMessages(", "\n// recordCursorFailure")

	require.NotRegexp(t, regexp.MustCompile(`(?m)^\t+c\.JSON\(classified\.StatusCode`), body,
		"非流式失败路径不得自己写响应体：handler 会在 failover 用尽后渲染，"+
			"这里先写一份会让客户端收到两份 body")

	require.Regexp(t, regexp.MustCompile(`(?m)^\t\treturn nil, cursorFailoverError\(classified, false\)`), body,
		"非流式失败必须返回 failover 契约错误，否则 errors.As 不匹配，"+
			"账号不进冷却、也不会换号")
}

// ⚠️ 流式路径：已推出真实内容时必须自己闭合块（handler 只会追加 error 帧，
// 不会闭合 content_block），未推出时必须交给 handler 渲染。
// 合并成一条分支必然在某一侧出错。
func TestStreamCursorMessages_SplitsErrorHandlingByRealOutput(t *testing.T) {
	src := readCursorRuntimeSource(t)

	require.Regexp(t, regexp.MustCompile(`if realOutput := emitter\.realOutputWritten\(\); realOutput \{`), src,
		"流式失败必须按是否已推出真实内容分流")

	// 已有输出：闭合块 + 自己发 error（handler 不会闭合块）。
	require.Regexp(t, regexp.MustCompile(`(?m)^\t\t\temitter\.failMidStream\(classified\.ErrorType, classified\.Message\)`), src,
		"已推出真实内容时必须由 emitter 闭合已开的块，否则客户端停在永不收尾的块上")

	// 仅心跳：不得再发一帧 error，交给 handler。
	require.Regexp(t, regexp.MustCompile(`(?m)^\t\treturn nil, cursorFailoverError\(classified, false\)`), src,
		"仅写过心跳注释时必须返回 failover 契约错误交给 handler 渲染")
}

// ⚠️ realOutputWritten 必须持锁读 started。
//
// OnText/OnReasoning/OnTool 由上游流的 goroutine 回调，心跳 goroutine 也读它。
// 裸读 e.started 是数据竞争：-race 下必挂，生产里表现为偶发走错收尾分支
// （该闭合块时没闭合，或者多发一帧 error）。
func TestCursorEmitterRealOutputWritten_ReadsUnderLock(t *testing.T) {
	src := readCursorRuntimeSource(t)
	fn := sliceBetween(t, src, "func (e *cursorAnthropicEmitter) realOutputWritten() bool {", "\nfunc (e *cursorAnthropicEmitter) usage(")

	require.Contains(t, fn, "e.mu.Lock()",
		"realOutputWritten 必须持锁读 started：回调来自上游流 goroutine，裸读是数据竞争")
	require.Contains(t, fn, "defer e.mu.Unlock()")
}

// 心跳注释不算真实输出：只写过 ": processing" 时 started 仍为 false，
// 此时收尾必须走「交给 handler」那条。反过来，ensureStarted 之后必须为 true。
func TestCursorEmitterRealOutputWritten_HeartbeatIsNotRealOutput(t *testing.T) {
	e := newCursorAnthropicEmitter(func(string, any) error { return nil }, "claude-sonnet-4", time.Now())
	require.False(t, e.realOutputWritten(),
		"尚未推出任何语义事件时必须为 false，否则仅有心跳的失败会被当成已推流，"+
			"白白放弃一次本可以成功的换号")

	require.NoError(t, e.ensureStarted())
	require.True(t, e.realOutputWritten(),
		"message_start 已发出即为真实输出：此后换号会让客户端收到第二份 message_start")
}

// ⚠️ Cursor 的错误文案只能靠 ResponseBody 到达客户端。
//
// handleFailoverExhausted 只认两个来源：错误透传规则（按 ResponseBody 匹配）
// 和 mapUpstreamError(StatusCode) 的固定兜底文案。ClientMessage 看着通用，
// 实际只在 OpenAI/Grok/Antigravity 的专有分支被读取。
// 不填 ResponseBody，上游明确的 "usage limit reached" 会被降级成
// "Upstream service temporarily unavailable"。
func TestCursorFailoverError_BodyCarriesClassifiedMessageToClient(t *testing.T) {
	err := cursorFailoverError(cursorErrorClassification{
		Category:   cursorErrorQuotaExhausted,
		StatusCode: 429,
		ErrorType:  "rate_limit_error",
		Message:    "usage limit reached",
	}, false)

	require.NotNil(t, err)
	require.NotEmpty(t, err.ResponseBody,
		"ResponseBody 为空时，handler 只能回退到固定兜底文案，上游的真实原因彻底丢失")

	// 形状必须能被 extractUpstreamErrorMessage 解出（它按 error.message 取值）。
	require.Equal(t, "usage limit reached",
		gjson.GetBytes(err.ResponseBody, "error.message").String(),
		"错误体必须是 Claude 形状 {\"error\":{\"message\":...}}，否则 handler 解不出文案")
	require.Equal(t, "rate_limit_error", gjson.GetBytes(err.ResponseBody, "error.type").String())
	require.Equal(t, "usage limit reached", ExtractUpstreamErrorMessage(err.ResponseBody),
		"必须能被网关通用提取函数解出：这是文案实际到达客户端的那条路径")
}

func readCursorRuntimeSource(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("cursor_runtime.go")
	require.NoError(t, err)
	return string(src)
}

func sliceBetween(t *testing.T, src, start, end string) string {
	t.Helper()
	i := regexp.MustCompile(regexp.QuoteMeta(start)).FindStringIndex(src)
	require.NotNil(t, i, "未找到起始锚点 %q，函数可能已重命名", start)
	rest := src[i[1]:]
	j := regexp.MustCompile(regexp.QuoteMeta(end)).FindStringIndex(rest)
	require.NotNil(t, j, "未找到结束锚点 %q", end)
	return rest[:j[0]]
}
