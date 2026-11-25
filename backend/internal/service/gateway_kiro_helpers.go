package service

import (
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

const (
	// upstreamCreditsRequestHeader lets a downstream gateway opt in to receiving
	// the upstream credits a Kiro request consumed. Without it the response
	// stays unchanged and the internal credits field is stripped.
	upstreamCreditsRequestHeader = "X-Sub2API-Upstream-Credits"
	// upstreamCreditsUsageField is the public usage field the credits are
	// exposed under on Anthropic Messages and OpenAI Responses usage when opted in.
	upstreamCreditsUsageField = "upstream_credits"
)

// upstreamCreditsRequested reports whether the caller asked for the upstream
// credits to be included in the response usage block.
func upstreamCreditsRequested(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(c.GetHeader(upstreamCreditsRequestHeader))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// withResponsesUpstreamCredits adds the Kiro credits a request consumed to the
// usage object at usagePath of an OpenAI Responses payload ("usage" for a
// response body, "response.usage" for a terminal stream event). The payload is
// returned unchanged unless the caller opted in and credits were reported, so
// requests served by non-Kiro accounts never carry the field.
func withResponsesUpstreamCredits(c *gin.Context, payload []byte, usagePath string, credits float64) []byte {
	if credits <= 0 || !upstreamCreditsRequested(c) || !gjson.GetBytes(payload, usagePath).IsObject() {
		return payload
	}
	next, err := sjson.SetBytes(payload, usagePath+"."+upstreamCreditsUsageField, credits)
	if err != nil {
		return payload
	}
	return next
}

// isResponsesTerminalEvent reports whether a Responses stream event carries the
// final response, and with it the final usage.
func isResponsesTerminalEvent(eventType string) bool {
	switch eventType {
	case "response.completed", "response.incomplete", "response.failed":
		return true
	}
	return false
}

func kiroCreditsFromUsageGJSON(usage gjson.Result) float64 {
	if !usage.Exists() {
		return 0
	}
	for _, key := range []string{"_sub2api_kiro_credits", "kiro_credits", "kiroCredits", "credits", "creditsUsed", "creditUsage"} {
		if v := usage.Get(key); v.Exists() && v.Float() > 0 {
			return v.Float()
		}
	}
	return 0
}

// upstreamUsageIsBillingScale 判断上游 usage 里那三个 token 字段是不是「计费口径」。
//
// Kiro 系上游（以及转发它的中继）会在 usage 里带一组 kiro_* 字段：
//
//	kiro_actual_input_tokens    真实 prompt token 数
//	kiro_billable_input_tokens  计费 token 数
//	kiro_excluded_input_tokens  上游自己折掉、不计费的那部分
//
// 而 input_tokens + cache_read + cache_creation 恒等于 **billable**，不等于 actual。
// 2026-09-16 对某中继实测：actual=30174、billable=10841、excluded=19333，
// 三个字段之和只有真实 prompt 的三分之一。这种数值不能当真实 token 用 ——
// 采信它等于把上下文规模和计费一起缩到三分之一。
//
// 判据用 excluded>0 / actual>billable，而不是「有没有 kiro_ 前缀字段」：
// excluded==0 时 billable 就等于真实 prompt，那份 usage 是可以采信的，
// 不该因为上游顺带带了几个耗时字段就整份作废。
func upstreamUsageIsBillingScale(usage gjson.Result) bool {
	if !usage.Exists() {
		return false
	}
	if v := usage.Get("kiro_excluded_input_tokens"); v.Exists() && v.Int() > 0 {
		return true
	}
	actual, billable := usage.Get("kiro_actual_input_tokens"), usage.Get("kiro_billable_input_tokens")
	return actual.Exists() && billable.Exists() && billable.Int() > 0 && actual.Int() > billable.Int()
}

// upstreamUsageMapIsBillingScale 是 map 版本，判据与 upstreamUsageIsBillingScale 一致。
// 通用 SSE 路径已经把事件解成了 map，再序列化回 JSON 只为过一遍 gjson 是白花开销。
func upstreamUsageMapIsBillingScale(usage map[string]any) bool {
	if len(usage) == 0 {
		return false
	}
	if v, ok := parseSSEUsageInt(usage["kiro_excluded_input_tokens"]); ok && v > 0 {
		return true
	}
	actual, hasActual := parseSSEUsageInt(usage["kiro_actual_input_tokens"])
	billable, hasBillable := parseSSEUsageInt(usage["kiro_billable_input_tokens"])
	return hasActual && hasBillable && billable > 0 && actual > billable
}

// upstreamCacheUsageIsTrustworthy 决定 preserve_upstream_cache_usage 这一轮能不能
// 真的让位给上游 usage：开关开着还不够，那份 usage 还得是真实 token 口径。
//
// 判定为计费口径时会打一条**限频** warn。这个开关是运维在 UI 上显式打开的，
// 静默忽略等于让人对着不生效的开关调参数；但它每轮请求都会命中，不限频会刷爆日志。
func upstreamCacheUsageIsTrustworthy(preserve, billingScale bool) bool {
	if !preserve {
		return false
	}
	if !billingScale {
		return true
	}
	logPreserveUpstreamCacheRefused()
	return false
}

const preserveUpstreamCacheWarnInterval = time.Minute

var preserveUpstreamCacheWarn struct {
	mu         sync.Mutex
	lastAt     time.Time
	suppressed int
}

func logPreserveUpstreamCacheRefused() {
	now := time.Now()
	t := &preserveUpstreamCacheWarn
	t.mu.Lock()
	if !t.lastAt.IsZero() && now.Sub(t.lastAt) < preserveUpstreamCacheWarnInterval {
		t.suppressed++
		t.mu.Unlock()
		return
	}
	suppressed := t.suppressed
	t.suppressed = 0
	t.lastAt = now
	t.mu.Unlock()

	logger.L().Warn("cache usage: preserve_upstream_cache_usage ignored because upstream usage is billing-scale",
		zap.String("reason", "kiro_excluded_input_tokens>0 or kiro_actual>kiro_billable: the three token fields sum to billable tokens, not real prompt tokens"),
		zap.Int("suppressed_since_last_log", suppressed),
		zap.Duration("log_interval", preserveUpstreamCacheWarnInterval),
	)
}
