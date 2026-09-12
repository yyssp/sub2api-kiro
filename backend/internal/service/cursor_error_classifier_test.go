package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

// 本文件复现原 ai2api retry_failure_test.go 的五条断言。
// 它们来自真实流量调试，不是风格选择——改动前请先确认上游行为真的变了。
func TestClassifyCursorError_PreservedHTTPSemantics(t *testing.T) {
	tests := []struct {
		name       string
		model      string
		text       string
		wantCat    string
		wantStatus int
		wantType   string
		wantKind   cursor.ErrKind
	}{
		{
			name:       "模型名无效 → 400 invalid_request_error",
			model:      "claude-sonnet-4.5",
			text:       "ERROR_BAD_MODEL_NAME: model name is not valid",
			wantCat:    cursorErrorBadModel,
			wantStatus: http.StatusBadRequest,
			wantType:   "invalid_request_error",
			wantKind:   cursor.ErrTransient,
		},
		{
			name:       "套餐不允许命名模型 → 400 invalid_request_error",
			model:      "claude-sonnet-4.5",
			text:       "Named models unavailable - Free plans can only use Auto",
			wantCat:    cursorErrorNamedModelDenied,
			wantStatus: http.StatusBadRequest,
			wantType:   "invalid_request_error",
			wantKind:   cursor.ErrTransient,
		},
		{
			name:       "额度耗尽 → 429 rate_limit_error",
			model:      "auto",
			text:       "ERROR_GPT_4_VISION_PREVIEW_RATE_LIMIT: You are out of usage - Upgrade to a paid plan to use more Grok Bot",
			wantCat:    cursorErrorQuotaExhausted,
			wantStatus: http.StatusTooManyRequests,
			wantType:   "rate_limit_error",
			wantKind:   cursor.ErrQuota,
		},
		{
			name:       "凭证失效 → 503 api_error",
			model:      "auto",
			text:       "ERROR_UNAUTHORIZED: not logged in",
			wantCat:    cursorErrorAuthError,
			wantStatus: http.StatusServiceUnavailable,
			wantType:   "api_error",
			wantKind:   cursor.ErrAuth,
		},
		{
			name:       "瞬时故障 → 503 api_error",
			model:      "auto",
			text:       "connection reset by peer",
			wantCat:    cursorErrorUpstreamTransient,
			wantStatus: http.StatusServiceUnavailable,
			wantType:   "api_error",
			wantKind:   cursor.ErrTransient,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyCursorError(tt.model, tt.text)
			require.Equal(t, tt.wantCat, got.Category)
			require.Equal(t, tt.wantStatus, got.StatusCode)
			require.Equal(t, tt.wantType, got.ErrorType)
			require.Equal(t, tt.wantKind, got.Kind)
		})
	}
}

func TestClassifyCursorError_HighLoadIsNotQuotaExhaustion(t *testing.T) {
	// ⚠️ Cursor 用同一个 RESOURCE_EXHAUSTED 表示「账号额度耗尽」和
	// 「模型当前高负载」。把后者判成额度耗尽会把可用账号的额度永久
	// 标记为 100%，误清空整个号池。
	got := classifyCursorError("auto",
		"ERROR_RESOURCE_EXHAUSTED: We're experiencing high load right now, please switch to Auto or try another model")
	require.Equal(t, cursorErrorUpstreamTransient, got.Category)
	require.Equal(t, cursor.ErrTransient, got.Kind)
	require.Empty(t, got.QuotaBucket, "高负载不得标记任何额度桶")
	require.False(t, cursorShouldDisableAccount(got))
}

func TestClassifyCursorError_QuotaMapsToCorrectBucket(t *testing.T) {
	// 额度耗尽必须归到目标模型所属的桶，否则会把无关的桶误标成耗尽。
	tests := map[string]string{
		"auto":              cursor.QuotaBucketCursor,
		"claude-sonnet-4.5": cursor.QuotaBucketGrokBot,
		"gpt-5":             cursor.QuotaBucketOther,
	}
	for model, wantBucket := range tests {
		t.Run(model, func(t *testing.T) {
			got := classifyCursorError(model, "You are out of usage - Upgrade to a paid plan")
			require.Equal(t, cursorErrorQuotaExhausted, got.Category)
			require.Equal(t, wantBucket, got.QuotaBucket)
		})
	}
}

func TestCursorShouldDisableAccount_OnlyOnAuthFailure(t *testing.T) {
	// ⚠️ 额度耗尽绝不停号：它只该把对应的桶标记为 exhausted，
	// 账号对其它桶的模型仍然可调度。
	quota := classifyCursorError("auto", "You are out of usage - Upgrade to a paid plan")
	require.False(t, cursorShouldDisableAccount(quota), "额度耗尽不得停用账号")

	transient := classifyCursorError("auto", "connection reset by peer")
	require.False(t, cursorShouldDisableAccount(transient), "瞬时故障不得停用账号")

	badModel := classifyCursorError("auto", "invalid model")
	require.False(t, cursorShouldDisableAccount(badModel), "模型问题不得停用账号")

	auth := classifyCursorError("auto", "ERROR_UNAUTHORIZED: not logged in")
	require.True(t, cursorShouldDisableAccount(auth), "凭证失效应停用账号")
}

func TestClassifyCursorError_BadModelTakesPriorityOverQuotaWording(t *testing.T) {
	// 判定顺序断言：同一段文案同时含「模型无效」与额度措辞时，
	// 必须判成模型问题（换模型可恢复），而不是额度耗尽（会误标满桶）。
	got := classifyCursorError("auto", "invalid model: you are out of usage for this model")
	require.Equal(t, cursorErrorBadModel, got.Category)
	require.Empty(t, got.QuotaBucket)
}
