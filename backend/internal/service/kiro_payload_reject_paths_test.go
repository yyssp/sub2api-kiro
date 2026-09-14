//go:build unit

package service

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	kiropkg "github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// behavior=reject 的 413 映射必须覆盖**所有**转发入口。
//
// 2026-09-14 交互式 CLI 实测踩到的真实缺口：413 映射只加在非流式分支，
// 而 Claude Code CLI 只走流式 —— 用户看到的是
// "API Error: 502 Upstream request failed"，被引去排查上游，
// 而真正原因是本地策略拒绝 + 请求太大。
//
// curl 默认非流式、单测也只断言非流式，三者一致地掩盖了这个缺口。
// 因此这里显式锁住"判别逻辑对 error 链有效"，并逐个入口回归。
func TestKiroPayloadTooLargeDetailRecognizesWrappedError(t *testing.T) {
	base := &kiropkg.ErrKiroPayloadTooLarge{Weight: 1_500_000, Limit: 1_300_000}

	weight, limit, ok := kiroPayloadTooLargeDetail(base)
	require.True(t, ok)
	require.Equal(t, 1_500_000, weight)
	require.Equal(t, 1_300_000, limit)

	// 错误在向上冒泡时会被包装，判别必须走 errors.As 而不是类型断言。
	wrapped := fmt.Errorf("build kiro payload: %w", base)
	weight, limit, ok = kiroPayloadTooLargeDetail(wrapped)
	require.True(t, ok, "被 %%w 包装后仍必须能识别")
	require.Equal(t, 1_500_000, weight)
	require.Equal(t, 1_300_000, limit)

	_, _, ok = kiroPayloadTooLargeDetail(errors.New("some upstream failure"))
	require.False(t, ok, "普通错误不得被误判成体积拒绝")

	_, _, ok = kiroPayloadTooLargeDetail(nil)
	require.False(t, ok)
}

// 文案必须同源：三个入口各写一份迟早漂移。
func TestKiroPayloadTooLargeMessageMentionsBothNumbers(t *testing.T) {
	msg := kiroPayloadTooLargeMessage(1_500_000, 1_300_000)
	require.Contains(t, msg, "1500000")
	require.Contains(t, msg, "1300000")
	require.Contains(t, msg, "too large")
}

// Anthropic 原生入口（流式与非流式共用这个 helper）：必须回 413 而非 502。
func TestRespondKiroPayloadTooLargeWrites413(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	handled := respondKiroPayloadTooLarge(c,
		fmt.Errorf("wrapped: %w", &kiropkg.ErrKiroPayloadTooLarge{Weight: 99, Limit: 50}))

	require.True(t, handled, "必须声明已处理，调用方据此跳过通用 502 分支")
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code,
		"回 502 会把用户引去排查上游，而请求根本没发出去")
	require.Contains(t, rec.Body.String(), "invalid_request_error")
	require.NotContains(t, rec.Body.String(), "Upstream request failed")
}

// 非体积错误必须原样放行给通用分支处理，否则会把上游故障误报成 413。
func TestRespondKiroPayloadTooLargePassesThroughOtherErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	handled := respondKiroPayloadTooLarge(c, errors.New("connection reset by peer"))

	require.False(t, handled)
	require.Equal(t, http.StatusOK, rec.Code, "不得写入任何响应")
	require.Empty(t, rec.Body.String())
}

// OpenAI 兼容入口（chat/completions 与 responses）用的是各自的 error writer，
// 但判据和文案必须与原生入口同源。
func TestOpenAICompatEntriesUseSameTooLargeDetail(t *testing.T) {
	err := fmt.Errorf("build: %w", &kiropkg.ErrKiroPayloadTooLarge{Weight: 777, Limit: 500})

	weight, limit, ok := kiroPayloadTooLargeDetail(err)
	require.True(t, ok)

	gin.SetMode(gin.TestMode)

	recCC := httptest.NewRecorder()
	cCC, _ := gin.CreateTestContext(recCC)
	writeGatewayCCError(cCC, http.StatusRequestEntityTooLarge, "invalid_request_error",
		kiroPayloadTooLargeMessage(weight, limit))
	require.Equal(t, http.StatusRequestEntityTooLarge, recCC.Code)
	require.Contains(t, recCC.Body.String(), "777")

	recR := httptest.NewRecorder()
	cR, _ := gin.CreateTestContext(recR)
	writeResponsesError(cR, http.StatusRequestEntityTooLarge, "invalid_request_error",
		kiroPayloadTooLargeMessage(weight, limit))
	require.Equal(t, http.StatusRequestEntityTooLarge, recR.Code)
	require.Contains(t, recR.Body.String(), "777")
}
