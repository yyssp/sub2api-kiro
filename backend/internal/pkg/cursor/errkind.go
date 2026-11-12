package cursor

import "strings"

// ⚠️ 本文件的判定顺序不可调换：「额度耗尽」必须先于「限流」。
// 反了会把耗尽误判成可重试的限流，重试会耗尽整个号池。
// 这是 ai2api 真实流量调试得到的结论，不是风格问题。
//
// 同理，high load 型 RESOURCE_EXHAUSTED 必须在额度关键词之前拦截：
// Cursor 用同一个错误码表示「账号额度耗尽」和「模型当前高负载」，
// 若把后者判成额度耗尽，会把可用账号的额度永久标记为 100%，误清空号池。
//
// 移植自 ai2api/internal/cursorpool/errclass.go；原先依赖的
// pool.ErrKind / ClassifyErr 已内联到本文件，使协议层零外部依赖。

// ErrKind 错误分类(单一真源): 驱动冷却/删除/禁用决策
type ErrKind int

const (
	ErrTransient ErrKind = iota // 瞬时故障(网络/5xx/模型抖动/截断): 短冷却可恢复
	ErrQuota                    // 额度耗尽/限流: 冷却(不删除, 限流号到期自动恢复)
	ErrAuth                     // 凭证失效(401/403/签名/token失效): 禁用需重登
)

// ClassifyErr 把各类错误信号归一到 ErrKind（通用兜底，内联自 ai2api ClassifyErr）
func ClassifyErr(creditErr, modelErr bool, spawnErr error, text string) ErrKind {
	low := strings.ToLower(text)
	if strings.Contains(low, "401") || strings.Contains(low, "403") ||
		strings.Contains(low, "unauthorized") || strings.Contains(low, "invalid_grant") ||
		strings.Contains(low, "invalid token") || strings.Contains(low, "signature invalid") ||
		strings.Contains(low, "token expired") {
		return ErrAuth
	}
	if creditErr {
		// 区分限流(瞬时)与真额度耗尽: 限流类关键词→瞬时, 否则→额度耗尽
		if strings.Contains(low, "too many") || strings.Contains(low, "rate limit") ||
			strings.Contains(low, "429") || strings.Contains(low, "try again") {
			return ErrTransient
		}
		return ErrQuota
	}
	return ErrTransient
}

// ClassifyCursorErr 归类 Cursor 上游错误。
// 先匹配 Cursor 特有错误码/文案(登录失效、额度耗尽、限流), 再回退到通用 ClassifyErr。
//
// ⚠️ 本函数只给出三态 ErrKind。客户端版本过旧、用户级限流、地区封锁这三类
// 不在三态里，由 service 层的分类器先用 IsOutdatedClientErr /
// IsUserRateLimitErr / IsRegionBlockedErr 拦截，拦不住的才落到这里——
// 它们的文案都会命中下面的额度或限流关键词，单靠本函数会被归错类。
func ClassifyCursorErr(text string) ErrKind {
	low := strings.ToLower(text)

	// 封号必须最先判：SUSPICIOUS_USAGE_BLOCKED 的正文常带 "usage" / "limit"
	// 一类措辞，落到额度分支只会标桶耗尽而不停号——号已经不能用了，
	// 还会被反复选中，每个请求白烧一次换号。
	if IsAccountBlockedErr(text) {
		return ErrAuth
	}

	// 以下三类既不是额度耗尽也不是凭证失效，但文案都会被后面的关键词误伤：
	//   - 版本过旧借用 ..._RATE_LIMIT 错误码，会被当成限流；
	//   - PRO 用户级限流带 "exceeded your usage limit"，会被当成额度耗尽而永久标满桶；
	//   - 地区封锁常以 "Connect error 403" 包裹，会被通用兜底的 "403" 判成凭证失效而停号。
	// 统一先截成 ErrTransient（不标桶、不停号），具体处置由 service 层细分。
	if IsOutdatedClientErr(text) || IsUserRateLimitErr(text) || IsRegionBlockedErr(text) {
		return ErrTransient
	}

	// Cursor 使用 ERROR_RESOURCE_EXHAUSTED 同时表示两种完全不同的状态：
	// 账号额度耗尽，或模型服务当前高负载。后者通常带有 high load /
	// high demand / switch model 等提示，只应短暂冷却，不能把账号的
	// Sand/GrokBot 额度永久标记为 100%，否则会把整个可用号池误清空。
	if strings.Contains(low, "resource_exhausted") &&
		(strings.Contains(low, "high load") ||
			strings.Contains(low, "high demand") ||
			strings.Contains(low, "switch to auto") ||
			strings.Contains(low, "another model")) {
		return ErrTransient
	}

	// 额度耗尽(最优先): Cursor 会用 ..._RATE_LIMIT 错误码承载"额度用尽", 真相在消息体, 例如
	// "ERROR_GPT_4_VISION_PREVIEW_RATE_LIMIT: You are out of usage - Upgrade to a paid plan to use more Grok Bot"。
	// 若先按错误码判成限流, 会既不标记额度又反复重试同一个已耗尽账号, 白白耗光换号次数(实测 5 次全废 → 503)。
	for _, k := range []string{
		"out of usage", "upgrade to a paid plan", "usage limit", "out of credits",
		"usage-based", "quota", "insufficient",
		"exceeded your", "hard limit", "spend limit", "free trial",
	} {
		if strings.Contains(low, k) {
			return ErrQuota
		}
	}
	// 凭证/登录失效 → 禁用需重登
	for _, k := range []string{
		"not_logged_in", "not logged in", "unauthenticated", "unauthorized",
		"authentication error", "error_unauthorized", "invalid_token", "invalid token",
		"token expired", "expired token", "login expired",
		"unpaid invoice", "pay your invoice", "resume requests",
	} {
		if strings.Contains(low, k) {
			return ErrAuth
		}
	}
	// 限流(瞬时可恢复): 纯限流才走这里(额度措辞已在上面拦截)
	for _, k := range []string{"rate_limit", "rate limit", "too many", "429", "try again", "slow down"} {
		if strings.Contains(low, k) {
			return ErrTransient
		}
	}
	return ClassifyErr(false, false, nil, text)
}

// isBadModelErr 判断是否为"模型名无效"类永久错误(换号无用, 应直接返回给客户端而非反复换号刷屏)。
func IsBadModelErr(text string) bool {
	low := strings.ToLower(text)
	for _, k := range []string{"bad_model_name", "model name is not valid", "model not found", "not a valid model", "invalid model", "model_not_found", "unknown model"} {
		if strings.Contains(low, k) {
			return true
		}
	}
	return false
}

// isNamedModelUnavailableErr 判断账号套餐是否明确只允许 Auto。
// 只匹配上游稳定的能力错误，不把普通 429/限流或模型不存在误记为账号能力限制。
func IsNamedModelUnavailableErr(text string) bool {
	low := strings.ToLower(text)
	return strings.Contains(low, "named models unavailable") &&
		(strings.Contains(low, "only use auto") || strings.Contains(low, "free plan"))
}

// IsOutdatedClientErr 判断上游是否以「客户端版本过旧」拒绝了请求。
//
// ⚠️ Cursor 会借用 ERROR_GPT_4_VISION_PREVIEW_RATE_LIMIT 这个错误码承载
// "Update Required — Your version of Cursor is no longer supported"。
// 它和真正的 Grok Bot 额度耗尽共用错误码，区别只在正文：带 update required
// 的绝不是额度问题。这是网关配置的客户端版本号整体失效，号池里每个账号的
// 结果都一样——换号只会把整个号池烧一遍，标桶会把健康账号误标成耗尽。
func IsOutdatedClientErr(text string) bool {
	low := strings.ToLower(text)
	return strings.Contains(low, "outdated_client") ||
		strings.Contains(low, "update required") ||
		strings.Contains(low, "version of cursor is no longer supported")
}

// IsAccountBlockedErr 判断账号是否已被 Cursor 封禁或关闭。
//
// 两个错误码都意味着这个账号本身已不可用（风控封禁 / 账号注销），
// 与额度、限流无关，只能停号等人工处理。
func IsAccountBlockedErr(text string) bool {
	low := strings.ToLower(text)
	return strings.Contains(low, "suspicious_usage_blocked") ||
		strings.Contains(low, "account_closed")
}

// IsUserRateLimitErr 判断是否为账号自身的用户级/套餐级限流。
//
// 覆盖 ERROR_PRO_USER_RATE_LIMIT_EXCEEDED / ERROR_FREE_USER_RATE_LIMIT_EXCEEDED /
// ERROR_USER_RATE_LIMIT_EXCEEDED（统一以 user_rate_limit_exceeded 识别），
// 以及命名模型/API 配额独立的 ERROR_API_RATE_LIMIT_EXCEEDED。
//
// ⚠️ 它不是额度耗尽：限流窗口过去账号就恢复，标桶耗尽会让账号在
// 下次额度刷新前一直不可调度。它也不是号池瞬时故障：同一账号立刻重试
// 必然再撞墙，必须让该账号短暂冷却、换号继续。
func IsUserRateLimitErr(text string) bool {
	low := strings.ToLower(text)
	return strings.Contains(low, "user_rate_limit_exceeded") ||
		strings.Contains(low, "api_rate_limit_exceeded")
}

// IsRegionBlockedErr 判断是否为地区/国家维度的访问限制。
//
// 地区封锁取决于账号出口 IP（即账号绑定的代理），换一个出口不同的账号
// 可能就能服务。识别规则对齐社区实现：要么带 UNSUPPORTED_REGION 错误码，
// 要么「地点词」与「否定词」同时出现。
//
// ⚠️ 只出现地点词不算：区域故障的措辞（如 "regional endpoint unavailable"）
// 同样含 region，但那是上游瞬时故障，归成地区封锁会把健康账号冷却掉。
func IsRegionBlockedErr(text string) bool {
	low := strings.ToLower(text)
	if strings.Contains(low, "unsupported_region") {
		return true
	}
	place := strings.Contains(low, "country") ||
		strings.Contains(low, "region") ||
		strings.Contains(low, "territor") ||
		strings.Contains(text, "国家") ||
		strings.Contains(text, "区域") ||
		strings.Contains(text, "地区")
	if !place {
		return false
	}
	return strings.Contains(low, "not available") ||
		strings.Contains(low, "unsupported") ||
		strings.Contains(low, "not supported") ||
		strings.Contains(low, "restricted") ||
		strings.Contains(low, "blocked") ||
		strings.Contains(text, "不支持")
}
