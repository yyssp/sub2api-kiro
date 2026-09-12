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
func ClassifyCursorErr(text string) ErrKind {
	low := strings.ToLower(text)

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
