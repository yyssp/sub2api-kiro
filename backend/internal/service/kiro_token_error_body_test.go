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
