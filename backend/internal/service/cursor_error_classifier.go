package service

import (
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

// Cursor 错误分类。取值域刻意与 Kiro 的 kiroError* 保持同构，
// 便于上层用同一套「是否换号重试 / 是否停用」的处置逻辑。
const (
	cursorErrorBadModel          = "bad_model"
	cursorErrorNamedModelDenied  = "named_model_unavailable"
	cursorErrorQuotaExhausted    = "quota_exhausted"
	cursorErrorAuthError         = "auth_error"
	cursorErrorUpstreamTransient = "upstream_transient"
)

// cursorErrorClassification 是一次 Cursor 上游失败的归类结果。
//
// StatusCode/ErrorType 直接对应返回给客户端的 Anthropic 错误语义，
// 由 ai2api 真实流量调试得出（原 retry_failure_test.go 的五条断言）：
//
//	badModel=true               → 400 invalid_request_error
//	namedModelsUnavailable=true → 400 invalid_request_error（提示改用 Auto/default）
//	kind=ErrQuota               → 429 rate_limit_error
//	kind=ErrAuth                → 503 api_error
//	kind=ErrTransient           → 503 api_error
type cursorErrorClassification struct {
	Category   string
	StatusCode int
	ErrorType  string
	Message    string
	// Kind 是协议层的三态归类，供调度层决定冷却还是停用。
	Kind cursor.ErrKind
	// QuotaBucket 非空时，表示只应把该桶标记为耗尽，而不是整号停用。
	QuotaBucket string
}

// classifyCursorError 把上游错误文本归类。
//
// ⚠️ 判定顺序不可调换，与协议层 errkind.go 的注释是同一条约束：
// 模型类错误必须先于额度/限流判定——Cursor 用同一段文案同时表达
// 「该模型不可用」与「额度耗尽」，顺序反了会把换个模型就能恢复的请求
// 判成额度耗尽，进而误标记整桶额度为 100%。
//
// model 参数用于把额度耗尽归到正确的桶（三桶相互独立，单桶耗尽不得整号停摆）。
func classifyCursorError(model, text string) cursorErrorClassification {
	trimmed := strings.TrimSpace(text)

	// 1) 模型不可用：换模型可恢复，不得计入额度。
	if cursor.IsBadModelErr(trimmed) {
		return cursorErrorClassification{
			Category:   cursorErrorBadModel,
			StatusCode: http.StatusBadRequest,
			ErrorType:  "invalid_request_error",
			Message:    trimmed,
			Kind:       cursor.ErrTransient,
		}
	}

	// 2) 套餐不允许命名模型（Free 套餐只能用 Auto/default）：
	//    这是账号能力问题，不是额度问题，也不该重试同一个模型。
	if cursor.IsNamedModelUnavailableErr(trimmed) {
		return cursorErrorClassification{
			Category:   cursorErrorNamedModelDenied,
			StatusCode: http.StatusBadRequest,
			ErrorType:  "invalid_request_error",
			Message:    trimmed,
			Kind:       cursor.ErrTransient,
		}
	}

	switch kind := cursor.ClassifyCursorErr(trimmed); kind {
	case cursor.ErrQuota:
		return cursorErrorClassification{
			Category:    cursorErrorQuotaExhausted,
			StatusCode:  http.StatusTooManyRequests,
			ErrorType:   "rate_limit_error",
			Message:     trimmed,
			Kind:        kind,
			QuotaBucket: cursor.ModelToQuotaBucket(cursor.StripPrefix(model)),
		}
	case cursor.ErrAuth:
		return cursorErrorClassification{
			Category:   cursorErrorAuthError,
			StatusCode: http.StatusServiceUnavailable,
			ErrorType:  "api_error",
			Message:    trimmed,
			Kind:       kind,
		}
	default:
		return cursorErrorClassification{
			Category:   cursorErrorUpstreamTransient,
			StatusCode: http.StatusServiceUnavailable,
			ErrorType:  "api_error",
			Message:    trimmed,
			Kind:       kind,
		}
	}
}

// cursorShouldDisableAccount 判断是否应把账号整体停用。
//
// ⚠️ 只有凭证失效才停号。额度耗尽绝不停号——它只该让对应的那个桶
// 标记为 exhausted，账号对其它桶的模型仍然可调度。
func cursorShouldDisableAccount(c cursorErrorClassification) bool {
	return c.Kind == cursor.ErrAuth
}
