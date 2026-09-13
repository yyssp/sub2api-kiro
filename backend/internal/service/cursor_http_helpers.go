package service

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

// Cursor 账号的 credentials 字段名（accounts.credentials JSONB）。
// 全仓库统一走这些常量，不要裸写字符串字面量。
const (
	CursorCredAccessToken  = "access_token"  // 纯 JWT，api2.cursor.sh Bearer
	CursorCredRefreshToken = "refresh_token" // deep-login 兑换所得，用于自动续期
	CursorCredSession      = "session"       // uid::JWT，cursor.com 面板 cookie 值
	CursorCredMachineID    = "machine_id"    // 设备指纹种子，参与 x-cursor-checksum
	CursorCredEmail        = "email"
	CursorCredMembership   = "membership" // 套餐，决定命名模型/Sand 能力
)

// CursorExtraQuotaKey 是三桶额度在 accounts.extra 里的唯一 key（决策 D1：extra JSONB）。
//
// 选 extra 而非加列，是为了不改 ent schema、不做 migration——直接上游
// (nianzs) 高频修改 account.go 与 schema，加列的合并成本远高于收益。
// 与 Kiro 的 extra["kiro_credit_unit_price_usd"] 是同一种先例。
const CursorExtraQuotaKey = "cursor_quota"

// 三桶额度的五态。与协议层 entitlement.go 的 sandState* 取值域保持一致：
// 必须区分「字段未返回(unknown)」与「确认耗尽(exhausted)」——
// 把 unknown 当成耗尽会误清空号池，当成可用则会反复撞墙。
const (
	CursorQuotaStateUnknown       = "unknown"
	CursorQuotaStateAvailable     = "available"
	CursorQuotaStateExhausted     = "exhausted"
	CursorQuotaStateUnavailable   = "unavailable"
	CursorQuotaStateRequestFailed = "request_failed"
)

// CursorBucketQuota 是单个额度桶的状态快照。
type CursorBucketQuota struct {
	State   string  `json:"state"`             // 五态之一
	Percent float64 `json:"percent"`           // 使用率 0-100
	Enabled bool    `json:"enabled,omitempty"` // 仅 grokbot 桶有意义
}

// CursorQuota 是一个 Cursor 账号的三桶额度快照。
//
// ⚠️ 三桶相互独立：账号可能 cursor 桶耗尽但 grokbot 桶仍可用。
// 判断可调度性必须按「目标模型属于哪个桶」，见 CursorAccountUsableForModel。
type CursorQuota struct {
	Cursor    CursorBucketQuota `json:"cursor"`
	Other     CursorBucketQuota `json:"other"`
	GrokBot   CursorBucketQuota `json:"grokbot"`
	FetchedAt *time.Time        `json:"fetched_at,omitempty"` // nil 表示从未抓取，不可当作 0% 可用
}

// readCursorQuota 是 extra["cursor_quota"] 的**唯一读口**。
//
// ⚠️ 全仓库禁止裸写 account.Extra["cursor_quota"]。extra 是无 schema 约束的
// JSONB，字段名写错不报错、只在运行时静默失效，只能靠单一读写口约束。
//
// 经 JSONB 往返后数值可能是 float64 / json.Number / string，这里统一容错解析；
// 解析不出的桶退化为 unknown 而不是 0%（0% 会被误判成"额度充足"）。
func readCursorQuota(account *Account) CursorQuota {
	q := CursorQuota{
		Cursor:  CursorBucketQuota{State: CursorQuotaStateUnknown},
		Other:   CursorBucketQuota{State: CursorQuotaStateUnknown},
		GrokBot: CursorBucketQuota{State: CursorQuotaStateUnknown},
	}
	if account == nil || account.Extra == nil {
		return q
	}
	raw, ok := account.Extra[CursorExtraQuotaKey]
	if !ok || raw == nil {
		return q
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return q
	}
	q.Cursor = readCursorBucket(m, cursor.QuotaBucketCursor)
	q.Other = readCursorBucket(m, cursor.QuotaBucketOther)
	q.GrokBot = readCursorBucket(m, cursor.QuotaBucketGrokBot)
	if ts := parseCursorTime(m["fetched_at"]); ts != nil {
		q.FetchedAt = ts
	}
	return q
}

// writeCursorQuota 是 extra["cursor_quota"] 的**唯一写口**。
func writeCursorQuota(account *Account, q CursorQuota) {
	if account == nil {
		return
	}
	if account.Extra == nil {
		account.Extra = make(map[string]any)
	}
	payload := map[string]any{
		cursor.QuotaBucketCursor:  writeCursorBucket(q.Cursor),
		cursor.QuotaBucketOther:   writeCursorBucket(q.Other),
		cursor.QuotaBucketGrokBot: writeCursorBucket(q.GrokBot),
	}
	if q.FetchedAt != nil {
		payload["fetched_at"] = q.FetchedAt.UTC().Format(time.RFC3339)
	}
	account.Extra[CursorExtraQuotaKey] = payload
}

func readCursorBucket(m map[string]any, key string) CursorBucketQuota {
	b := CursorBucketQuota{State: CursorQuotaStateUnknown}
	raw, ok := m[key]
	if !ok || raw == nil {
		return b
	}
	bm, ok := raw.(map[string]any)
	if !ok {
		return b
	}
	if s, ok := bm["state"].(string); ok {
		if st := normalizeCursorQuotaState(s); st != "" {
			b.State = st
		}
	}
	if pct, ok := parseCursorFloat(bm["percent"]); ok {
		b.Percent = pct
	}
	if en, ok := bm["enabled"].(bool); ok {
		b.Enabled = en
	}
	return b
}

func writeCursorBucket(b CursorBucketQuota) map[string]any {
	state := normalizeCursorQuotaState(b.State)
	if state == "" {
		state = CursorQuotaStateUnknown
	}
	out := map[string]any{"state": state, "percent": b.Percent}
	if b.Enabled {
		out["enabled"] = true
	}
	return out
}

// normalizeCursorQuotaState 只接受已知五态，未知取值返回 ""（由调用方退化成 unknown）。
func normalizeCursorQuotaState(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case CursorQuotaStateAvailable:
		return CursorQuotaStateAvailable
	case CursorQuotaStateExhausted:
		return CursorQuotaStateExhausted
	case CursorQuotaStateUnavailable:
		return CursorQuotaStateUnavailable
	case CursorQuotaStateRequestFailed:
		return CursorQuotaStateRequestFailed
	case CursorQuotaStateUnknown:
		return CursorQuotaStateUnknown
	}
	return ""
}

// parseCursorFloat 容忍 JSONB 往返后的多种数值形态。
//
// ⚠️ NaN/Inf 一律视为解析失败：NaN 的任何比较都为 false，会让
// `CursorModelsPct < 100` 恒假，把账号静默判成不可调度；
// 退化成"未解析出"再由调用方保持 unknown 才是安全方向。
func parseCursorFloat(v any) (float64, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case float32:
		f = float64(n)
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case json.Number:
		parsed, err := n.Float64()
		if err != nil {
			return 0, false
		}
		f = parsed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0, false
		}
		f = parsed
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func parseCursorTime(v any) *time.Time {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return nil
	}
	ts, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return nil
	}
	return &ts
}

// isCursorDirectModeAccount 是转发分发点的判定钩子（照 isKiroDirectModeAccount 的形状）。
func isCursorDirectModeAccount(account *Account) bool {
	if account == nil || account.Platform != PlatformCursor {
		return false
	}
	switch account.Type {
	case AccountTypeOAuth:
		return true
	case AccountTypeAPIKey:
		// 配了自定义 base_url 视为走用户自有网关，不走内建 Cursor 直连链路。
		return strings.TrimSpace(account.GetCredential("base_url")) == ""
	}
	return false
}

// cursorProtocolAccount 把 sub2api 的 Account 投影成协议层的 cursor.Account。
//
// ⚠️ 运行时状态（冷却、失败计数）一律来自 accounts 表既有列，不从 credentials 读；
// 三桶额度来自 extra（经 readCursorQuota），不在 credentials 里另存一份。
// 两套状态并存会导致调度器读列、转发逻辑读 JSONB，行为不一致。
// cursorAccountProxyURL 返回账号绑定的代理地址，未绑定时返回空串（直连）。
// 与 kiroProxyURL 保持同一判定：ProxyID 与 Proxy 都在才算数——只有 ProxyID
// 说明关联没被预加载，此时取值会 panic。
func cursorAccountProxyURL(account *Account) string {
	if account != nil && account.ProxyID != nil && account.Proxy != nil {
		return account.Proxy.URL()
	}
	return ""
}

func cursorProtocolAccount(account *Account) cursor.Account {
	if account == nil {
		return cursor.Account{}
	}
	q := readCursorQuota(account)
	out := cursor.Account{
		ID:           account.ID,
		Email:        strings.TrimSpace(account.GetCredential(CursorCredEmail)),
		AccessToken:  strings.TrimSpace(account.GetCredential(CursorCredAccessToken)),
		RefreshToken: strings.TrimSpace(account.GetCredential(CursorCredRefreshToken)),
		Session:      strings.TrimSpace(account.GetCredential(CursorCredSession)),
		MachineID:    strings.TrimSpace(account.GetCredential(CursorCredMachineID)),
		Membership:   strings.TrimSpace(account.GetCredential(CursorCredMembership)),

		CursorModelsPct: q.Cursor.Percent,
		OtherModelsPct:  q.Other.Percent,
		// ⚠️ 状态必须一并投影：只传百分比时，抓取失败的桶（Percent 零值）
		// 会被判成 100% 空闲，并复活刚被标记耗尽的桶。
		CursorModelsState: q.Cursor.State,
		OtherModelsState:  q.Other.State,
		GrokBotState:      q.GrokBot.State,
		GrokBotPct:        q.GrokBot.Percent,
		GrokBotEnabled:    q.GrokBot.Enabled,

		// 运行时状态取自既有列
		Disabled:  !account.Schedulable || account.Status == StatusError,
		LastError: account.ErrorMessage,

		// 账号级代理：Cursor 按设备指纹 + 出口 IP 做风控，整池共用一个出口
		// 容易被连带判定异常。与其它平台一致，只在 ProxyID/Proxy 都在时才取。
		ProxyURL: cursorAccountProxyURL(account),
	}
	if q.FetchedAt != nil {
		out.UsageAt = *q.FetchedAt
	}
	out.AccountExpiry = cursorAccessTokenExpiry(out.AccessToken)
	return out
}

// cursorAccessTokenExpiry 返回 access token 自身的到期时刻，直接从 JWT exp 反解。
//
// ⚠️ 绝不能改用 accounts.expires_at：那一列的语义是「账号/订阅有效期」，
// 由管理端手工录入（admin_account.go），并且被 AutoPauseExpiredAccounts 扫描——
// 该扫描对 auto_pause_on_expired=TRUE（列默认值）的账号做 schedulable=FALSE。
// Cursor 的 access token 只有几小时寿命，一旦把它的 exp 写进那一列，
// 整个 Cursor 号池会在几小时内被自动暂停扫描全量停用。
//
// 反过来也不能沿用那一列做刷新判定：导入路径从不写它（cursor_oauth_service.go
// 解析出的 exp 只用于导入预览展示），恒为 NULL ⇒ AccountExpiry 恒为零值 ⇒
// AuthRefreshDue/AuthRefreshExpired 恒为 false ⇒ 主动续期永不触发，
// 只能等 401 才被动刷新。token 就在 credentials 里，直接反解才是唯一自洽的来源。
func cursorAccessTokenExpiry(accessToken string) time.Time {
	if strings.TrimSpace(accessToken) == "" {
		return time.Time{}
	}
	// 解析不出 exp 时返回零值：协议层对零值的约定是「有效期未知，不主动刷新，
	// 等 401 再强刷」，比瞎猜一个到期时间安全。
	return cursor.JWTExpiry(accessToken)
}

// CursorAccountUsableForModel 是三桶额度与调度的衔接点。
//
// ⚠️ 单桶耗尽 ≠ 整号不可调度。调用方必须传入本次请求的目标模型，
// 由协议层按「模型属于哪个桶」判断；拿不到目标模型时不要用本函数
// 一刀切标记账号不可用（退化策略见 cursor_error_classifier.go）。
func CursorAccountUsableForModel(account *Account, model string) bool {
	if account == nil {
		return false
	}
	return cursor.AccountUsableForModel(cursorProtocolAccount(account), cursor.StripPrefix(model))
}

// ValidateCursorQuotaFromExtra 校验管理端传入的 extra["cursor_quota"] 形状。
// 照 ValidateKiroCreditUnitPriceFromExtra 的先例：extra 无 schema 约束，
// 只能在入口处挡住明显错误的结构，避免写进去后静默失效。
func ValidateCursorQuotaFromExtra(extra map[string]any) error {
	if extra == nil {
		return nil
	}
	raw, ok := extra[CursorExtraQuotaKey]
	if !ok || raw == nil {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("%s must be an object", CursorExtraQuotaKey)
	}
	for _, bucket := range []string{cursor.QuotaBucketCursor, cursor.QuotaBucketOther, cursor.QuotaBucketGrokBot} {
		braw, ok := m[bucket]
		if !ok || braw == nil {
			continue
		}
		bm, ok := braw.(map[string]any)
		if !ok {
			return fmt.Errorf("%s.%s must be an object", CursorExtraQuotaKey, bucket)
		}
		if s, ok := bm["state"].(string); ok && normalizeCursorQuotaState(s) == "" {
			return fmt.Errorf("%s.%s.state %q is not a known state", CursorExtraQuotaKey, bucket, s)
		}
		if praw, ok := bm["percent"]; ok && praw != nil {
			pct, ok := parseCursorFloat(praw)
			if !ok {
				return fmt.Errorf("%s.%s.percent must be a number", CursorExtraQuotaKey, bucket)
			}
			// ⚠️ 必须显式挡 NaN/Inf：strconv.ParseFloat("NaN") 成功返回，
			// 而 NaN 的任何比较都为 false，既能穿过下面的区间校验，
			// 又会让 CursorModelsPct < 100 恒为 false，把账号静默判成不可调度。
			if math.IsNaN(pct) || math.IsInf(pct, 0) {
				return fmt.Errorf("%s.%s.percent must be a finite number", CursorExtraQuotaKey, bucket)
			}
			if pct < 0 || pct > 100 {
				return fmt.Errorf("%s.%s.percent must be within [0,100]", CursorExtraQuotaKey, bucket)
			}
		}
	}
	return nil
}
