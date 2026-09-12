package cursor

import (
	"encoding/json"
	"strings"
)

// 三桶额度归类 + 少量通用小helper。
// 移植自 ai2api/internal/cursorpool/manager.go（有状态号池，不整体移植）与
// handler.go / anthropic.go —— 这几个函数本身是纯函数，被协议层依赖。

// modelIsCursorBucket 判断模型是否属于 Cursor Models(auto 桶: grok/composer/vega/default/auto),
// 否则视为 Other Models(claude/gpt/gemini 等第三方模型)。
func modelIsCursorBucket(model string) bool {
	m := strings.ToLower(model)
	for _, p := range []string{"grok", "composer", "vega", "default", "auto"} {
		if strings.Contains(m, p) {
			return true
		}
	}
	return false
}

// modelIsFreePlanAutoModel reports the two aliases Cursor currently accepts
// after returning "Free plans can only use Auto". Grok, Composer, and Vega
// are Cursor-owned models but are still named models for this entitlement.
func modelIsFreePlanAutoModel(model string) bool {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "auto", "default":
		return true
	default:
		return false
	}
}

// usageBucketForModel maps a successful request to the system usage bucket
// shown on the account list. This is an attribution of requests handled by
// this gateway, not a claim about Cursor's internal debit ledger:
//   - direct Sand models, including Claude Code compatibility models -> GrokBot/Sand
//   - Cursor-owned auto/composer/vega models -> Cursor
//   - named third-party models (GPT/Gemini/etc.) -> Other
func usageBucketForModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return ""
	}
	if cursorClientTypeForModel(m) == "sand" {
		if isClaudeCodeModelName(m) && !useSandInferenceService(m) {
			return "other"
		}
		return "grokbot"
	}
	if strings.Contains(m, "grok") || strings.Contains(m, "grokbot") {
		return "grokbot"
	}
	if modelIsCursorBucket(m) {
		return "cursor"
	}
	return "other"
}

// 三桶额度的桶名常量。业务层存 extra["cursor_quota"] 时用同一套 key，
// 避免两侧字符串写法漂移（extra 是无 schema 约束的 JSONB，写错不报错）。
const (
	QuotaBucketCursor  = "cursor"
	QuotaBucketOther   = "other"
	QuotaBucketGrokBot = "grokbot"
)

// ModelToQuotaBucket 把目标模型映射到三桶之一（cursor / other / grokbot）。
//
// 调度层用它判断「该账号对本次请求的目标模型是否还有额度」。
// ⚠️ 一个账号可能 cursor 桶耗尽但 grokbot 桶可用，
// 不能因单桶耗尽就把整个账号标记为不可调度。
func ModelToQuotaBucket(model string) string {
	return usageBucketForModel(model)
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func isClaudeCodeCoreOrMarkerToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "bash", "read", "write", "edit", "ls", "glob", "grep", "delete", "agent", "task",
		"skill", "toolsearch", "deferredtoolplaceholder", "taskoutput",
		"todowrite", "todoread", "askuserquestion", "enterplanmode", "exitplanmode",
		"reportfindings", "workflow":
		return true
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), "mcp__")
}

// normToolArgs 把工具参数规整为合法 JSON 字符串(空/空白 -> "{}"),
// 避免客户端把 "" 当非法 JSON 解析失败。
func normToolArgs(in json.RawMessage) string {
	s := strings.TrimSpace(string(in))
	if s == "" {
		return "{}"
	}
	return s
}

// ── 按桶的可调度性判定（移植自 ai2api manager.go）─────────────────────
// ⚠️ 这是三桶额度与调度的衔接点：单桶耗尽不得让整个账号不可调度。
// 业务层在调度 gate 处按目标模型调用 AccountUsableForModel。

// AccountUsableForModel 判断账号对「目标模型所属的桶」是否仍可用。
func AccountUsableForModel(a Account, model string) bool {
	return AccountUsableForSurface(a, model, cursorClientTypeForModel(model))
}

// AccountUsableForSurface 在已知传输面(clientType)时做同样判断。
func AccountUsableForSurface(a Account, model, clientType string) bool {
	if accountCredentialUnavailable(a) {
		return false
	}
	capability := capabilityForSurface(a, model, clientType)
	if capability == CapabilityDeny {
		return false
	}
	if clientType == "sand" {
		// Free 套餐的真实上游响应明确为"Free plans can only use Auto"。
		// 该限制也适用于 Claude Code 的命名模型；不要因为历史 grokBot
		// 百分比尚未刷新，就把 Free 账号当成 Claude/Sand 候选。
		if isClaudeCodeModelName(model) && accountNamedModelsUnavailable(a) {
			return false
		}
		switch sandStateForAccount(a) {
		case sandStateAvailable:
			return true
		case sandStateUnknown, sandStateRequestFailed:
			// Probe/allow can discover a live entitlement; deny was handled
			// above. Explicit Sand quota/unsupported state is never retried.
			return true
		default:
			return false
		}
	}
	// Cursor 上游明确声明该套餐"only use Auto"时，只有 auto/default 两个别名可继续使用。
	// Grok、Composer、Vega 虽属于 Cursor 自有模型，仍是该套餐不允许的命名模型。
	if !modelIsFreePlanAutoModel(model) && accountNamedModelsUnavailable(a) {
		return false
	}
	// 从未抓取过额度（UsageAt 零值）时按可用处理，
	// 不能把"未知"当成 0% 或 100%。
	if a.UsageAt.IsZero() {
		return true
	}
	if modelIsCursorBucket(model) {
		return a.CursorModelsPct < 100
	}
	return a.OtherModelsPct < 100
}

// accountCredentialUnavailable 依据业务层填入的最近错误文案判断凭证是否已失效。
// ai2api 原版读 CursorAccount.LastError（JSON 号池字段）；sub2api 侧对应
// accounts.error_message 列，由业务层填进 Account.LastError。
func accountCredentialUnavailable(a Account) bool {
	low := strings.ToLower(strings.TrimSpace(a.LastError))
	if low == "" {
		return false
	}
	for _, marker := range []string{
		"认证失败", "凭证失效", "token", "登录", "logged in", "unauthenticated", "unauthorized",
		"authentication", "account closed", "account_closed",
	} {
		if strings.Contains(low, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

func accountNamedModelsUnavailable(a Account) bool {
	return a.NamedModelsUnavailable || membershipDisallowsNamedModels(a.Membership)
}

// membershipDisallowsNamedModels: Free 套餐只允许 Auto/default。
func membershipDisallowsNamedModels(membership string) bool {
	return normalizeMembership(membership) == "free"
}

// requestClientTypeForModel returns the quota/entitlement surface for one
// concrete request. Transport and model identity intentionally differ for
// Claude Code tool requests: they use AgentService/cli, not direct Sand.
func requestClientTypeForModel(model string, tools []ToolDef) string {
	if useSandInferenceServiceForRequest(model, tools) {
		return cursorClientTypeForModel(model)
	}
	return agentServiceClientTypeForModel(model)
}

// RequestToQuotaBucket 是 ModelToQuotaBucket 的「带工具」版本，业务层应优先用它。
//
// ⚠️ 同一个 Claude Code 模型名，纯文本请求走 grokbot 桶，
// 带工具/MCP/Agent 的请求走 other 桶。只按模型名判桶会记错账、
// 也会在调度 gate 上放行实际已耗尽的桶。
func RequestToQuotaBucket(model string, tools []ToolDef) string {
	bucket := usageBucketForModel(model)
	if len(tools) == 0 {
		return bucket
	}
	if requestClientTypeForModel(model, tools) == "cli" && isClaudeCodeModelName(model) {
		return QuotaBucketOther
	}
	return bucket
}

// StripPrefix 归一化对外模型名：去掉可选的 "cursor/" 前缀，
// 再解析 Claude Code 标准模型名 → Cursor 内部模型名。
// 业务层收到 parsed.Model 后先过这一层，再做桶归类与转发。
func StripPrefix(model string) string {
	raw := strings.TrimSpace(model)
	if i := strings.IndexByte(raw, '/'); i >= 0 && strings.EqualFold(raw[:i], "cursor") {
		raw = strings.TrimSpace(raw[i+1:])
	}
	if resolved, ok := ResolveClaudeCodeModel(raw); ok {
		return resolved
	}
	return normalizeCursorModel(raw)
}
