package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsKiroTokenErrorBody(t *testing.T) {
	tokenErrors := []string{
		`{"message":"The bearer token included in the request is invalid."}`,
		`{"message":"token expired"}`,
		`{"__type":"com.amazon.coral.service#ExpiredTokenException"}`,
		`{"message":"Invalid token"}`,
		`{"__type":"UnauthorizedException"}`,
	}
	for _, body := range tokenErrors {
		require.True(t, isKiroTokenErrorBody([]byte(body)), body)
	}

	// 与 token 无关的 403 不得触发强制刷新。
	otherErrors := []string{
		`{"message":"User is not authorized to make this call."}`,
		`{"message":"Invalid profile ARN"}`,
		`{"message":"Invalid model. Please select a different model to continue."}`,
		`{"message":"Access denied: request expired"}`,
	}
	for _, body := range otherErrors {
		require.False(t, isKiroTokenErrorBody([]byte(body)), body)
	}
}

func TestIsKiroAccountBlockedResponse(t *testing.T) {
	blocked := []struct {
		status int
		body   string
	}{
		{403, `{"reason":"TEMPORARILY_SUSPENDED"}`},
		{403, `{"__type":"AccountSuspendedException"}`},
		{403, `{"message":"Your User ID is suspended"}`},
		{403, `{"message":"We have locked your account"}`},
		{423, `{}`},
	}
	for _, c := range blocked {
		require.True(t, isKiroAccountBlockedResponse(c.status, []byte(c.body)), "%d %s", c.status, c.body)
		require.Equal(t, kiroErrorSuspended, classifyKiroHTTPError(c.status, c.body).Category)
	}
	require.False(t, isKiroAccountBlockedResponse(403, []byte(`{"message":"User is not authorized to make this call."}`)))
	// 封禁措辞只对 403 生效：429 的风控提示按普通限流冷却，与 2ue_kiro.rs 默认行为一致。
	require.False(t, isKiroAccountBlockedResponse(429, []byte(`{"message":"suspicious activity, temporarily suspended"}`)))
}
