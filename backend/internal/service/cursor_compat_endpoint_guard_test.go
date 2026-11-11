//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Cursor 直连账号的凭证只对 Cursor 有效。OpenAI 兼容入口（/v1/chat/completions、
// /v1/responses）与 count_tokens 都会把请求转成 Anthropic Messages 走通用链路；
// 不拦截的话会带着 Cursor token 发往 api.anthropic.com——凭证泄露，且上游 401
// 会被误记成账号故障。合并前就存在这条路径，此测试锁住修复。
func TestCursorAccountNeverSentToAnthropicFromCompatEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newUpstream := func() *httpUpstreamRecorder {
		mk := func() *http.Response {
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{},
				Body: io.NopCloser(bytes.NewReader([]byte(`{"error":"should not be reached"}`)))}
		}
		return &httpUpstreamRecorder{responses: []*http.Response{mk(), mk()}}
	}
	account := &Account{
		ID: 901, Name: "cursor", Platform: PlatformCursor, Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{CursorCredAccessToken: "header.payload.sig"},
	}
	cases := []struct {
		name string
		path string
		body string
		call func(*GatewayService, *gin.Context, []byte) error
	}{
		{"chat completions", "/v1/chat/completions",
			`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`,
			func(s *GatewayService, c *gin.Context, b []byte) error {
				_, err := s.ForwardAsChatCompletions(context.Background(), c, account, b, &ParsedRequest{})
				return err
			}},
		{"responses", "/v1/responses",
			`{"model":"claude-sonnet-4-6","input":"hi","stream":false}`,
			func(s *GatewayService, c *gin.Context, b []byte) error {
				_, err := s.ForwardAsResponses(context.Background(), c, account, b, &ParsedRequest{})
				return err
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newUpstream()
			svc := &GatewayService{httpUpstream: upstream, tlsFPProfileService: &TLSFingerprintProfileService{},
				cursorTokenProvider: &CursorTokenProvider{}}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader([]byte(tc.body)))
			c.Request.Header.Set("Content-Type", "application/json")

			err := tc.call(svc, c, []byte(tc.body))
			require.Empty(t, upstream.requests, "Cursor 凭证不得发往任何非 Cursor 上游")
			var failover *UpstreamFailoverError
			require.True(t, errors.As(err, &failover), "应返回 failover，让调度换一个能服务此协议的账号: %v", err)
			require.Equal(t, anthropicMessagesForeignOAuthReason, failover.Reason)
		})
	}

	t.Run("count_tokens", func(t *testing.T) {
		upstream := newUpstream()
		svc := &GatewayService{httpUpstream: upstream, tlsFPProfileService: &TLSFingerprintProfileService{}}
		body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello cursor"}]}`)
		parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(body))

		require.NoError(t, svc.ForwardCountTokens(context.Background(), c, account, parsed))
		require.Empty(t, upstream.requests, "count_tokens 对 Cursor 只能本地估算")
		require.Equal(t, http.StatusOK, rec.Code)
		require.Contains(t, rec.Body.String(), `"input_tokens"`)
	})
}
