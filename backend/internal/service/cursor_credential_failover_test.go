//go:build unit

package service

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// ⚠️ 本组钉的是「取不到 access token 时能否换号」。
//
// token provider 的刷新逻辑本身很谨慎（只在确认失效时才 SetError），但那份
// 谨慎只有在**失败能被调度层看见**时才有价值。此前 forwardCursorMessages 把
// provider 的 error 原样 return，handler 的 errors.As(*UpstreamFailoverError)
// 不匹配 → 当场结束请求。于是出现最违反直觉的故障：
// 号池里 9 个账号健康，只因选中的第 10 个 refresh token 过期就对用户报错。

func newCursorTestGinContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	return c
}

// 确认失效（上游明确拒绝 refresh token）→ 账号级，必须换号。
func TestCursorCredentialFailover_ConfirmedInvalidGrantSwitchesAccount(t *testing.T) {
	err := cursorCredentialFailover(
		newCursorTestGinContext(),
		&Account{ID: 7, Name: "acc-7", Platform: PlatformCursor},
		errors.New("cursor auth refresh failed: invalid_grant"),
	)

	require.NotNil(t, err)
	require.True(t, err.ShouldRetryNextAccount(),
		"凭证确认失效必须换号，否则号池里健康账号一个都用不上")
	require.True(t, err.IsCredentialFailure(),
		"必须标成凭证阶段失败，否则会被当成推理失败计入账号的上游错误率")
	require.Equal(t, GatewayFailureScopeAccount, err.Scope,
		"确认失效是这个号自己的问题，账号级上报是正确的")
}

// ⚠️ 网络抖动 / 代理 EOF / 5xx：provider 刻意**不**写 SetError。
//
// 此时若按账号级上报，等于把一次全局网络故障记成"这些账号都不健康"，
// 调度器的账号健康度被污染，故障恢复之后仍会持续避开这些号。
func TestCursorCredentialFailover_TransientDoesNotBlameAccount(t *testing.T) {
	err := cursorCredentialFailover(
		newCursorTestGinContext(),
		&Account{ID: 8, Name: "acc-8", Platform: PlatformCursor},
		errors.New("post https://cursor.example/refresh: EOF"),
	)

	require.NotNil(t, err)
	require.True(t, err.ShouldRetryNextAccount(),
		"瞬时故障也应当换号重试：下一个号可能走不同代理出口")
	require.Equal(t, GatewayFailureScopeProvider, err.Scope,
		"网络抖动不是账号的问题，记成账号级会污染调度器的账号健康度")

	// Scope=provider 时不得把失败算到账号头上。
	require.False(t, err.ShouldReportAccountScheduleFailure(),
		"provider 级凭证失败不能计入账号调度健康度")
}

// ⚠️ 绝不能把上游原始错误回给客户端：refresh 失败报文里可能带 token 片段。
func TestCursorCredentialFailover_DoesNotLeakUpstreamDetailToClient(t *testing.T) {
	err := cursorCredentialFailover(
		newCursorTestGinContext(),
		&Account{ID: 9, Platform: PlatformCursor},
		errors.New("401 unauthorized: refresh_token=secret-abc123 rejected"),
	)

	require.NotNil(t, err)
	require.NotContains(t, err.ClientMessage, "secret-abc123",
		"凭证片段绝不能出现在返回给客户端的文案里")
	require.Equal(t, cursorCredentialUnavailableClientMessage, err.ClientMessage)
}

// ⚠️ 凭证阶段不得伪造 ResponseBody。
//
// 那不是上游推理接口返回的响应体，填一个 Claude 形状的 body 会让错误透传
// 规则按"上游响应"去匹配它，可能命中与本次故障无关的规则并改写状态码。
func TestCursorCredentialFailover_HasNoFakeUpstreamBody(t *testing.T) {
	err := cursorCredentialFailover(
		newCursorTestGinContext(),
		&Account{ID: 10, Platform: PlatformCursor},
		errors.New("invalid_grant"),
	)

	require.NotNil(t, err)
	require.Empty(t, err.ResponseBody,
		"凭证阶段没有上游响应体，伪造一个会让错误透传规则误匹配")
}

// account 为 nil 时不得 panic（防御性：调用点在取 token 之前）。
func TestCursorCredentialFailover_NilAccountIsSafe(t *testing.T) {
	require.NotPanics(t, func() {
		err := cursorCredentialFailover(newCursorTestGinContext(), nil, errors.New("boom"))
		require.NotNil(t, err)
		require.True(t, err.ShouldRetryNextAccount())
	})
}

// ⚠️ 调用点接线守卫：forwardCursorMessages 必须转换后再返回。
// 直接 return err 不会有任何测试变红——请求照常返回错误，只是换号能力没了。
func TestForwardCursorMessages_ConvertsTokenErrorToFailover(t *testing.T) {
	src := readCursorRuntimeSource(t)
	body := sliceBetween(t, src,
		"func (s *GatewayService) forwardCursorMessages(",
		"\n\tprotoAccount := cursorProtocolAccount(account)")

	require.Contains(t, body, "return nil, cursorCredentialFailover(c, account, err)",
		"取 token 失败必须转成凭证级 failover 契约，否则 handler 直接结束请求，"+
			"号池里其它健康账号完全用不上")
}
