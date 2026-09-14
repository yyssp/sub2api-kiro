package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClassifyKiroHTTPErrorBadRequestCategories(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "schema",
			body: `{"message":"Improperly formed request: inputSchema.properties must be an object"}`,
			want: kiroErrorBadRequestSchema,
		},
		{
			name: "tool pairing",
			body: `{"message":"tool_use must be paired with a matching tool_result"}`,
			want: kiroErrorBadRequestToolPairing,
		},
		{
			name: "invalid model id",
			body: `{"message":"invalid modelId: model not supported"}`,
			want: kiroErrorBadRequestInvalidModel,
		},
		{
			name: "invalid model upstream",
			body: `{"error":{"message":"Invalid model. Please select a different model to continue.","type":"upstream_error"}}`,
			want: kiroErrorBadRequestInvalidModel,
		},
		{
			name: "invalid model reason",
			body: `{"message":"model route unavailable","reason":"INVALID_MODEL_ID"}`,
			want: kiroErrorBadRequestInvalidModel,
		},
		{
			name: "auth",
			body: `{"error":"invalid_grant","message":"Invalid refresh token provided"}`,
			want: kiroErrorBadRequestAuth,
		},
		{
			name: "quota",
			body: `{"message":"resource has been exhausted"}`,
			want: kiroErrorBadRequestQuota,
		},
		{
			name: "unknown",
			body: `{"message":"bad request"}`,
			want: kiroErrorBadRequestUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			classification := classifyKiroHTTPError(http.StatusBadRequest, tt.body)
			require.Equal(t, tt.want, classification.Category)
			require.Equal(t, http.StatusBadRequest, classification.StatusCode)
			require.Equal(t, tt.body, classification.Message)
		})
	}
}

// TestClassifyKiroBadRequestDualErrorStrings 锁定同一个 schema 校验失败在两个端点
// 上返回的两条不同错误串都能归类为 schema 类。
//
// 证据来源：funny-vibes/agent-vibes apps/protocol-bridge/src/llm/aws/translator.ts:879-887
// —— 后端用 Smithy 校验工具 schema，q.* 端点返回 "Improperly formed request."，
// codewhisperer.*/krs 端点返回 {"message":"Invalid tool use format.",
// "reason":"REQUEST_BODY_INVALID"}。此前只覆盖了前者。
func TestClassifyKiroBadRequestDualErrorStrings(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "q endpoint plain string",
			body: `Improperly formed request.`,
			want: kiroErrorBadRequestSchema,
		},
		{
			name: "q endpoint wrapped",
			body: `{"message":"Improperly formed request."}`,
			want: kiroErrorBadRequestSchema,
		},
		{
			name: "krs endpoint full body",
			body: `{"message":"Invalid tool use format.","reason":"REQUEST_BODY_INVALID"}`,
			want: kiroErrorBadRequestSchema,
		},
		{
			name: "krs endpoint message only",
			body: `{"message":"Invalid tool use format."}`,
			want: kiroErrorBadRequestSchema,
		},
		{
			name: "krs endpoint reason only",
			body: `{"reason":"REQUEST_BODY_INVALID"}`,
			want: kiroErrorBadRequestSchema,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyKiroHTTPError(http.StatusBadRequest, tt.body)
			require.Equal(t, tt.want, got.Category)
			require.Equal(t, http.StatusBadRequest, got.StatusCode)
		})
	}
}

// TestClassifyKiroBadRequestNewStringsDoNotOverreach 保证新增的两条串不会抢走
// 其他 400 子类，也不会把无关 400 误判成 schema 类。
func TestClassifyKiroBadRequestNewStringsDoNotOverreach(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "monthly quota still wins",
			body: `{"message":"quota","reason":"MONTHLY_REQUEST_COUNT"}`,
			want: kiroErrorBadRequestQuota,
		},
		{
			name: "invalid model still wins",
			body: `{"message":"Invalid model. Please select a different model."}`,
			want: kiroErrorBadRequestInvalidModel,
		},
		{
			name: "unrelated bad request stays unknown",
			body: `{"message":"something else entirely"}`,
			want: kiroErrorBadRequestUnknown,
		},
		{
			name: "empty body stays unknown",
			body: ``,
			want: kiroErrorBadRequestUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyKiroHTTPError(http.StatusBadRequest, tt.body)
			require.Equal(t, tt.want, got.Category)
		})
	}
}

// 体积超限 400 必须被独立分类。
//
// 实测真实响应体是 {"message":"Input content length exceeds threshold."}，
// 它不含 schema/tool 等关键字，在加 looksLikeKiroOversizeError 之前会落进
// bad_request_unknown —— 既丢诊断信息，也让 on_upstream_400 行为永远触发不了。
func TestClassifyKiroOversizeBadRequest(t *testing.T) {
	c := classifyKiroHTTPError(400, `{"message":"Input content length exceeds threshold."}`)
	require.Equal(t, kiroErrorBadRequestOversize, c.Category)
	require.Equal(t, 400, c.StatusCode)
}

// 体积分类不得抢走其它 400 的归类 —— 顺序错了会把 schema 错误误判成体积问题，
// 进而触发一次毫无意义的缩减重试。
func TestClassifyKiroOversizeDoesNotShadowOtherBadRequests(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"message":"Improperly formed request."}`, kiroErrorBadRequestSchema},
		{`{"message":"Invalid tool use format.","reason":"REQUEST_BODY_INVALID"}`, kiroErrorBadRequestSchema},
		{`{"message":"something else entirely"}`, kiroErrorBadRequestUnknown},
	} {
		require.Equal(t, tc.want, classifyKiroHTTPError(400, tc.body).Category, "body=%s", tc.body)
	}
}
