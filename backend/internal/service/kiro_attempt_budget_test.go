//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestKiroAttemptBudgetTake(t *testing.T) {
	b := newKiroAttemptBudget(2)
	require.True(t, b.take())
	require.True(t, b.take())
	require.False(t, b.take())
	var nilBudget *kiroAttemptBudget
	require.True(t, nilBudget.take(), "未挂预算的调用方不受限制")
}

// 换号会重新进入转发入口，预算必须挂在 gin.Context 上跨入口共享。
func TestWithKiroAttemptBudgetSharedAcrossForwards(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	first := kiroAttemptBudgetFromContext(withKiroAttemptBudget(context.Background(), c))
	second := kiroAttemptBudgetFromContext(withKiroAttemptBudget(context.Background(), c))
	require.NotNil(t, first)
	require.Same(t, first, second)
}

// 持续 5xx 时，单次请求打到上游的次数不得超过预算。
func TestExecuteKiroUpstreamStopsAtAttemptBudget(t *testing.T) {
	prevSleep := kiroRetrySleep
	kiroRetrySleep = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { kiroRetrySleep = prevSleep })

	account := &Account{
		ID: 990001, Platform: PlatformKiro, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"kiro_api_key": "k"},
	}
	responses := make([]*http.Response, 0, 32)
	for range 32 {
		responses = append(responses, newJSONResponse(http.StatusInternalServerError, `{"message":"boom"}`))
	}
	upstream := &queuedHTTPUpstream{responses: responses}
	svc := &GatewayService{
		httpUpstream:        upstream,
		kiroCooldownStore:   &stubKiroCooldownStore{},
		tlsFPProfileService: &TLSFingerprintProfileService{},
	}
	payload, err := createTestPayload("claude-sonnet-4-6")
	require.NoError(t, err)
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := withKiroAttemptBudget(context.Background(), c)
	// 模拟 handler 多次换号：同一 gin.Context 上反复执行。
	for range 6 {
		resp, _, _ := svc.executeKiroUpstreamWithParsed(ctx, account, nil, body, "claude-sonnet-4-6", "claude-sonnet-4-6", "tok", nil)
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	require.Equal(t, kiroUpstreamAttemptBudget, len(upstream.requests))

	_, _, err = svc.executeKiroUpstreamWithParsed(ctx, account, nil, body, "claude-sonnet-4-6", "claude-sonnet-4-6", "tok", nil)
	require.ErrorIs(t, err, errKiroAttemptBudgetExhausted)
}
