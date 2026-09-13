//go:build unit

package service

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

// 终端协议错误必须与「账号故障」彻底分开。
//
// 这四个哨兵（未声明工具 / 畸形工具负载 / 流提前结束 / 请求被拒）描述的是
// 「本次请求的协议形状不可服务」。归成 upstream_transient 的后果不是多报一个
// 错误码，而是 failover 会逐个换号重试同一个确定性失败的请求——
// 一个坏请求烧穿整个号池。
func TestClassifyCursorRunError_TerminalProtocolErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "未声明工具",
			err:  cursor.ErrUndeclaredUpstreamTool,
			want: "Cursor upstream requested a tool that was not declared by this request",
		},
		{
			name: "畸形工具负载",
			err:  cursor.ErrMalformedUpstreamTool,
			want: "Cursor upstream sent a malformed tool call",
		},
		{
			name: "流提前结束",
			err:  cursor.ErrIncompleteUpstreamStream,
			want: "Cursor upstream stream ended before completion",
		},
		{
			name: "请求形状被拒",
			err:  cursor.ErrInvalidUpstreamRequest,
			want: "Cursor upstream rejected the request protocol",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyCursorRunError("claude-sonnet-4.5", tc.err)

			require.True(t, got.Terminal,
				"未标记为终端错误：failover 会换号重试一个确定性失败的请求，烧穿号池")
			require.Equal(t, cursorErrorProtocol, got.Category)
			require.Equal(t, http.StatusBadGateway, got.StatusCode,
				"应为 502（网关侧协议问题），而不是 503（上游暂时不可用）")
			require.Equal(t, tc.want, got.Message)
			require.False(t, cursorShouldDisableAccount(got), "协议错误不得停用账号")
			require.Empty(t, got.QuotaBucket, "协议错误不得标记额度桶耗尽")
		})
	}
}

// 包装过的错误也必须能识别（errors.Is 链），否则协议层一旦加一层
// fmt.Errorf 上下文，这套判定就会静默失效。
func TestClassifyCursorRunError_UnwrapsWrappedSentinels(t *testing.T) {
	wrapped := fmt.Errorf("run agent stream: %w", cursor.ErrUndeclaredUpstreamTool)
	got := classifyCursorRunError("claude-sonnet-4.5", wrapped)
	require.True(t, got.Terminal, "包装后的哨兵错误未被识别")
	require.Equal(t, http.StatusBadGateway, got.StatusCode)
}

// 回给客户端的文案必须是有界的固定串，不能带帧级内部诊断细节。
func TestClassifyCursorRunError_MessageExcludesInternalWireDetail(t *testing.T) {
	err := fmt.Errorf("%w: branch=19 native=task wire=0x1f payload=deadbeef",
		cursor.ErrUndeclaredUpstreamTool)
	got := classifyCursorRunError("claude-sonnet-4.5", err)

	require.NotContains(t, got.Message, "wire=", "内部帧细节泄漏给了客户端")
	require.NotContains(t, got.Message, "payload=", "内部帧细节泄漏给了客户端")
	require.NotContains(t, got.Message, "branch=", "内部帧细节泄漏给了客户端")
	require.Contains(t, got.Message, "not declared by this request")
	// 工具名是客户端唯一用得上的上下文（据此调整自己的工具声明），予以保留。
	require.Contains(t, got.Message, "tool: task")
}

// 非协议错误必须仍走原有的文本归类，不能被新分支吞掉。
func TestClassifyCursorRunError_FallsBackToTextClassification(t *testing.T) {
	got := classifyCursorRunError("claude-sonnet-4.5",
		errors.New("ERROR_BAD_MODEL_NAME: model name is not valid"))
	require.False(t, got.Terminal)
	require.Equal(t, cursorErrorBadModel, got.Category)
	require.Equal(t, http.StatusBadRequest, got.StatusCode)
}

// 回给客户端的 Message 必须脱敏：上游错误串可能带 URL query 上的凭证参数，
// 原样透出等于把密钥回显给调用方。
func TestClassifyCursorError_SanitizesCredentialsInMessage(t *testing.T) {
	got := classifyCursorError("claude-sonnet-4.5",
		"post https://api2.cursor.sh/x?access_token=super-secret-value&foo=1 failed: 500")

	require.NotContains(t, got.Message, "super-secret-value",
		"凭证被原样写进了回给客户端的错误消息")
	require.Contains(t, got.Message, "access_token=***")
}

// 脱敏不得改变归类结果：判定走未脱敏文本，脱敏只改写 Message。
//
// 注：sanitizeUpstreamErrorMessage 是全平台共用的实现，只覆盖 URL query 形态
// （?/& 引导的 key/client_secret/access_token/refresh_token）。散文里裸写的
// token 不在它的契约内——这里不为 Cursor 单独放宽那条共享正则，
// 只锁住「归类不受脱敏影响」这一条。
func TestClassifyCursorError_SanitizeDoesNotAffectClassification(t *testing.T) {
	got := classifyCursorError("claude-sonnet-4.5",
		"401 unauthorized: https://api2.cursor.sh/refresh?refresh_token=leaked-token")
	require.Equal(t, cursorErrorAuthError, got.Category,
		"脱敏影响了归类判定")
	require.NotContains(t, got.Message, "leaked-token")
}
