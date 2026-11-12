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
	// cursorErrorProtocol 是终端协议错误：请求的协议形状本身不可服务。
	// 既不是账号故障，也不可重试——换号只会把同一个坏请求打给整个号池。
	cursorErrorProtocol = "protocol_error"
	// cursorErrorOutdatedClient 是网关配置的 Cursor 客户端版本被上游整体拒绝。
	// 号池里每个账号都会得到同样的结果：不换号、不写任何账号状态，只能由
	// 管理员更新 CURSOR_CLIENT_VERSION。
	cursorErrorOutdatedClient = "outdated_client"
	// cursorErrorUserRateLimit 是账号自身的用户级/套餐级限流：该账号短暂冷却后
	// 自动恢复，不是额度耗尽（不标桶），也不是号池瞬时故障（不同号重试）。
	cursorErrorUserRateLimit = "user_rate_limited"
	// cursorErrorRegionBlocked 是账号出口地区被上游拒绝：换一个出口不同的账号
	// 可能就能服务，该账号对该模型冷却，提示检查代理出口。
	cursorErrorRegionBlocked = "region_blocked"
)

// cursorOutdatedClientMessage 是版本过旧时对外的固定文案。
//
// ⚠️ 不透传上游原文：上游原文是写给桌面端用户的「请升级 Cursor」，对网关的
// 调用方毫无意义；真正需要动作的是网关管理员。
const cursorOutdatedClientMessage = "Cursor upstream rejected the gateway's client version; " +
	"the administrator must update CURSOR_CLIENT_VERSION"

// cursorRegionBlockedMessage 是地区封锁时对外的固定文案。
const cursorRegionBlockedMessage = "Cursor upstream rejected this account's network region; " +
	"check the egress region of the account's proxy"

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
//
// 后续按真实上游错误码补充的三类（不在原五条断言之内）：
//
//	outdated_client   → 502 api_error（固定文案，不换号）
//	user_rate_limited → 429 rate_limit_error（账号冷却 + 换号）
//	region_blocked    → 403 permission_error（账号×模型冷却 + 换号）
type cursorErrorClassification struct {
	Category   string
	StatusCode int
	ErrorType  string
	Message    string
	// Kind 是协议层的三态归类，供调度层决定冷却还是停用。
	Kind cursor.ErrKind
	// QuotaBucket 非空时，表示只应把该桶标记为耗尽，而不是整号停用。
	QuotaBucket string
	// Terminal 表示这是终端协议错误：不是账号问题，换号重试无意义。
	// 调度层据此跳过 failover，并且不得把它计入账号失败。
	Terminal bool
	// CooldownModel 是需要按「账号 × 模型」冷却时的模型键（目前只有地区封锁使用）。
	// 取值为 forwardCursorMessages 传入的 mappedModel，即账号级映射之后的模型名——
	// 与选号时 modelRateLimitKeysForRequest 的 GetMappedModel(requestedModel) 同一口径，
	// 写进去的冷却才会被选号读到。
	CooldownModel string
}

// classifyCursorRunError 是 RunAgentStream 返回错误的归类入口。
//
// ⚠️ 必须先判终端协议错误，再退回文本归类。这四个哨兵错误
// （未声明工具 / 畸形工具负载 / 流提前结束 / 请求被拒）表达的是
// 「本次请求的协议形状不可服务」，与账号健康度无关：
//   - 归成 ErrTransient 会让 failover 逐个换号重试同一个确定性失败的请求，
//     单个坏请求烧穿整个号池；
//   - 状态码也该是 502（网关侧协议问题）而不是 503（上游暂时不可用）；
//   - 原始错误串带帧级内部细节（branch=/native=/wire=），不能直接回给客户端。
//
//nolint:unused // compatibility wrapper used by package-level protocol tests.
func classifyCursorRunError(model string, err error) cursorErrorClassification {
	return classifyCursorRunErrorForRequest(model, nil, err)
}

func classifyCursorRunErrorForRequest(model string, tools []cursor.ToolDef, err error) cursorErrorClassification {
	if err == nil {
		return cursorErrorClassification{}
	}
	if cursor.IsTerminalProtocolError(err) {
		return cursorErrorClassification{
			Category:   cursorErrorProtocol,
			StatusCode: http.StatusBadGateway,
			ErrorType:  "api_error",
			Message:    cursor.TerminalProtocolPublicMessage(err),
			// ErrKind 只有三态且没有「与账号无关」这一档，这里取零值 ErrTransient
			// 仅仅是占位。真正的处置依据是 Terminal=true：
			// cursorShouldDisableAccount 不会停号（只有 ErrAuth 才停），
			// recordCursorFailure 也不得把它计入账号失败。
			Kind:     cursor.ErrTransient,
			Terminal: true,
		}
	}
	return classifyCursorErrorForRequest(model, tools, err.Error())
}

// classifyCursorError 把上游错误文本归类。
//
// ⚠️ 判定顺序不可调换，与协议层 errkind.go 的注释是同一条约束：
// 模型类错误必须先于额度/限流判定——Cursor 用同一段文案同时表达
// 「该模型不可用」与「额度耗尽」，顺序反了会把换个模型就能恢复的请求
// 判成额度耗尽，进而误标记整桶额度为 100%。
//
// model 参数用于把额度耗尽归到正确的桶（三桶相互独立，单桶耗尽不得整号停摆）。
//
// ⚠️ Message 必须经 sanitizeUpstreamErrorMessage：它会被直接写进 SSE error 帧和
// 非流式 JSON body 回给客户端。上游错误串里可能带 URL query 上的凭证参数，
// 原样透出等于把密钥回显给调用方。判定仍用未脱敏文本——脱敏只改写敏感参数值，
// 但没必要让归类规则依赖脱敏后的形状。
func classifyCursorError(model, text string) cursorErrorClassification {
	return classifyCursorErrorForRequest(model, nil, text)
}

func classifyCursorErrorForRequest(model string, tools []cursor.ToolDef, text string) cursorErrorClassification {
	raw := strings.TrimSpace(text)
	trimmed := sanitizeUpstreamErrorMessage(raw)

	// 0) 客户端版本过旧：必须先于一切判定。
	//
	// 上游借用 ..._RATE_LIMIT 错误码承载它，正文还可能带 "usage" 一类措辞，
	// 落到后面任何一个分支都是错的：判成限流会换号把整个号池烧一遍，判成额度
	// 会把健康账号的桶标满。号池里每个账号结果都一样，这是网关自身的配置问题。
	if cursor.IsOutdatedClientErr(raw) {
		return cursorErrorClassification{
			Category:   cursorErrorOutdatedClient,
			StatusCode: http.StatusBadGateway,
			ErrorType:  "api_error",
			Message:    cursorOutdatedClientMessage,
			Kind:       cursor.ErrTransient,
		}
	}

	// 1) 模型不可用：换模型可恢复，不得计入额度。
	if cursor.IsBadModelErr(raw) {
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
	if cursor.IsNamedModelUnavailableErr(raw) {
		return cursorErrorClassification{
			Category:   cursorErrorNamedModelDenied,
			StatusCode: http.StatusBadRequest,
			ErrorType:  "invalid_request_error",
			Message:    trimmed,
			Kind:       cursor.ErrTransient,
		}
	}

	// 3) 用户级/套餐级限流：必须先于额度关键词。
	//
	// ERROR_PRO_USER_RATE_LIMIT_EXCEEDED 的正文带 "exceeded your usage limit"，
	// 落到额度分支会把桶永久标满——而它只是一个会自动结束的限流窗口。
	// 处置是「该账号短暂冷却 + 换号」，见 cursorUserRateLimitCooldown。
	if cursor.IsUserRateLimitErr(raw) {
		return cursorErrorClassification{
			Category:   cursorErrorUserRateLimit,
			StatusCode: http.StatusTooManyRequests,
			ErrorType:  "rate_limit_error",
			Message:    trimmed,
			Kind:       cursor.ErrTransient,
		}
	}

	// 4) 地区封锁：必须先于通用兜底——它常被包成 "Connect error 403"，
	// 通用兜底会因 "403" 判成凭证失效而把健康账号停掉。
	if cursor.IsRegionBlockedErr(raw) {
		return cursorErrorClassification{
			Category:      cursorErrorRegionBlocked,
			StatusCode:    http.StatusForbidden,
			ErrorType:     "permission_error",
			Message:       cursorRegionBlockedMessage,
			Kind:          cursor.ErrTransient,
			CooldownModel: strings.TrimSpace(model),
		}
	}

	switch kind := cursor.ClassifyCursorErr(raw); kind {
	case cursor.ErrQuota:
		return cursorErrorClassification{
			Category:    cursorErrorQuotaExhausted,
			StatusCode:  http.StatusTooManyRequests,
			ErrorType:   "rate_limit_error",
			Message:     trimmed,
			Kind:        kind,
			QuotaBucket: cursor.RequestToQuotaBucket(cursor.StripPrefix(model), tools),
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
