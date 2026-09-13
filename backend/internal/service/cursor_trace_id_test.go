//go:build unit

package service

import (
	"context"
	"os"
	"regexp"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

// ⚠️ Cursor 协议层的所有上游调试日志（logAgentDebug）都以 traceID 为前缀，
// 而 traceID 优先取 AgentRequest.TraceID。这个字段在接入初期从未被赋值，
// 于是每条 [agent] 日志都打 trace=none：
//
//   - 并发请求的日志在同一个进程里完全交织，无法归属到具体请求；
//   - 网关侧日志用的是 ctxkey.RequestID（也是响应头 X-Request-Id），
//     两侧对不上，线上拿到一个 request_id 也没法查上游发生了什么。
//
// 本组测试钉住「网关请求 ID 必须传进协议层」这条链路。它是纯字符串赋值，
// 编译器管不住：漏掉只会让日志静默退化，功能测试全绿。

func TestCursorTraceIDFromContext_UsesGatewayRequestID(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, "req-abc-123")
	require.Equal(t, "req-abc-123", cursorTraceIDFromContext(ctx),
		"协议层 trace 必须复用网关请求 ID，否则两侧日志无法关联")
}

// ⚠️ 取不到时必须返回空串而不是造一个新 ID。
//
// 协议层对空串有明确语义：agent.go 会回退到 ctx trace，再回退到 debug 模式下
// 的随机 UUID。这里自作主张生成 ID 会顶掉那条回退链，且生成的 ID 与网关侧
// 任何记录都对不上，比 trace=none 更有误导性。
func TestCursorTraceIDFromContext_EmptyWhenAbsent(t *testing.T) {
	require.Empty(t, cursorTraceIDFromContext(context.Background()))
	require.Empty(t, cursorTraceIDFromContext(nil))
}

// ⚠️ 类型不匹配时不能 panic。ctxkey.RequestID 理论上只由中间件写入 string，
// 但 context 是无类型的，任何人都能塞进别的类型。
func TestCursorTraceIDFromContext_WrongTypeIsSafe(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, 12345)
	require.Empty(t, cursorTraceIDFromContext(ctx))
}

// ⚠️ 必须 trim：中间件的 normalizeCorrelationID 之外，ctx 也可能被其它路径
// 写入带空白的值。带空白的 trace 会让协议层的空串判定失效（" " != ""），
// 于是既不回退、又打出一个空白前缀。
// ⚠️ 上面的用例只覆盖 helper 本身；helper 全绿而调用点被删掉，
// 链路一样是断的（TraceID 恒为空）。真正的 forwardCursorMessages 需要
// token provider 与真实上游，为一行赋值把那套机具搭起来不成比例，
// 因此在源码层面钉住调用点——与 usage_logs 列对齐护栏同一思路。
func TestForwardCursorMessages_AssignsTraceIDFromContext(t *testing.T) {
	src, err := os.ReadFile("cursor_runtime.go")
	require.NoError(t, err)

	// (?m) + 行首只允许制表符：注释掉的赋值以 "//" 开头，不能算数。
	require.Regexp(t,
		regexp.MustCompile(`(?m)^\t+agentReq\.TraceID\s*=\s*cursorTraceIDFromContext\(ctx\)`),
		string(src),
		"forwardCursorMessages 必须把网关请求 ID 赋给 AgentRequest.TraceID："+
			"漏掉会让协议层日志退回 trace=none，且与网关日志无法关联")
}

func TestCursorTraceIDFromContext_TrimsWhitespace(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, "  req-padded  ")
	require.Equal(t, "req-padded", cursorTraceIDFromContext(ctx))

	blank := context.WithValue(context.Background(), ctxkey.RequestID, "   ")
	require.Empty(t, cursorTraceIDFromContext(blank),
		"全空白必须归一成空串，否则协议层的回退链不会触发")
}
