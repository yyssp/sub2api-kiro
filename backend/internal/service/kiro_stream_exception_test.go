//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	kiropkg "github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func parseKiroSSEErrorEvent(t *testing.T, event string) gjson.Result {
	t.Helper()
	require.True(t, strings.HasPrefix(event, "event: error\ndata: "))
	data := strings.TrimSuffix(strings.TrimPrefix(event, "event: error\ndata: "), "\n\n")
	require.True(t, gjson.Valid(data), data)
	return gjson.Parse(data)
}

func TestKiroStreamErrorEventMapsExceptionTypes(t *testing.T) {
	cases := map[string]string{
		"ThrottlingException":            "rate_limit_error",
		"ServiceUnavailableException":    "overloaded_error",
		"ContentLengthExceededException": "invalid_request_error",
		"ValidationException":            "invalid_request_error",
		"InternalServerException":        "api_error",
	}
	for exceptionType, want := range cases {
		err := fmt.Errorf("wrap: %w", &kiropkg.KiroStreamException{ExceptionType: exceptionType, Message: "arn:aws:secret-detail"})
		got := parseKiroSSEErrorEvent(t, kiroStreamErrorEvent(err))
		require.Equal(t, want, got.Get("error.type").String(), exceptionType)
		require.NotContains(t, got.Get("error.message").String(), "arn:aws", "上游原文可能含账号细节，不得下发")
	}
}

func TestKiroStreamErrorEventContentLengthUsesPromptTooLong(t *testing.T) {
	got := parseKiroSSEErrorEvent(t, kiroStreamErrorEvent(&kiropkg.KiroStreamException{ExceptionType: "ContentLengthExceededException"}))
	require.Contains(t, got.Get("error.message").String(), "prompt is too long")
}

func TestKiroStreamErrorEventGenericError(t *testing.T) {
	got := parseKiroSSEErrorEvent(t, kiroStreamErrorEvent(errors.New("read: connection reset")))
	require.Equal(t, "api_error", got.Get("error.type").String())
	require.Equal(t, "stream interrupted", got.Get("error.message").String())
}

func TestKiroUpstreamPromptTooLongMessageIsClaudeCodeCompatible(t *testing.T) {
	require.True(t, strings.HasPrefix(kiroUpstreamPromptTooLongMessage(), "prompt is too long"))
}

// 上游体积超限 400 必须改写成 Claude Code 可识别的 prompt is too long，
// 否则 CLI 不会自动 compact，用户会卡在同一个超限请求上。
func TestHandleKiroHTTPErrorOversizeRewritesToPromptTooLong(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	account := &Account{ID: 44, Platform: PlatformKiro, Type: AccountTypeOAuth}
	svc := &GatewayService{accountRepo: &recordingKiroTempUnschedRepo{}}
	resp := newJSONResponse(http.StatusBadRequest, `{"message":"Input content length exceeds threshold."}`)

	err := svc.handleKiroHTTPError(context.Background(), resp, c, account, "claude-sonnet-4.5", []byte(`{"model":"claude-sonnet-4-5"}`))
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	body := gjson.Parse(rec.Body.String())
	require.Equal(t, "invalid_request_error", body.Get("error.type").String())
	require.True(t, strings.HasPrefix(body.Get("error.message").String(), "prompt is too long"), rec.Body.String())
}
