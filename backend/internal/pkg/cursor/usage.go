package cursor

import "strings"

// outputUsageEstimate 描述网关可观察到的模型输出。
// OutputTokens 用于 API usage、账号累计、指标和费用估算，包含正文与已发送的工具调用；
// VisibleTextTokens 仅表示正文，纯工具轮可以为 0。Cursor agent.v1 当前没有被可靠验证的
// 官方 token usage 字段，因此 Source 明确标记为 estimated，不能当作上游精确 usage。
type outputUsageEstimate struct {
	OutputTokens      int
	VisibleTextTokens int
	Kind              string
	Source            string
	ToolCalls         int
	// CacheReadTokens/CacheWriteTokens 是上游报告的 prompt 缓存命中/写入量。
	// Cursor agent.v1 目前不返回这两个值, 所以常态为 0; 保留字段是为了让计价
	// 链路有地方接收它们 —— 之前这里直接给 EstimateRequestCostUSD 传字面量 0,
	// 一旦上游开始返回缓存量, 缓存读也不会被扣除, 会按全价重复计费。
	CacheReadTokens  int
	CacheWriteTokens int
}

// estimateCompletionTokens 按当前系统约 3 字节/token 的估算口径计算正文 token。
// 真正没有可观察输出时返回 0，非空的极短输出仍按 1 token 计。
func estimateCompletionTokens(textLen int) int {
	if textLen <= 0 {
		return 0
	}
	outTok := textLen / 3
	if outTok < 1 {
		outTok = 1
	}
	return outTok
}

// toolCallUsageBytes 返回已经发送给下游的工具调用语义内容长度。
// 只计工具名和规范化 JSON 参数，不把调试日志、ID 或内部协议字段算入费用。
func toolCallUsageBytes(tc ToolCall) int {
	name := strings.TrimSpace(tc.Name)
	args := strings.TrimSpace(string(tc.Input))
	if args == "" {
		args = "{}"
	}
	// 参数可能来自上游的非标准 JSON；normToolArgs 会保留合法 JSON 或替换为空对象。
	args = normToolArgs(tc.Input)
	return len(name) + len(args)
}

// estimateOutputUsage 统一计算正文、工具调用和输出类型。
func estimateOutputUsage(textLen int, calls []ToolCall) outputUsageEstimate {
	visible := estimateCompletionTokens(textLen)
	totalBytes := textLen
	for _, call := range calls {
		totalBytes += toolCallUsageBytes(call)
	}
	total := estimateCompletionTokens(totalBytes)
	kind := "unknown"
	switch {
	case textLen > 0 && len(calls) > 0:
		kind = "mixed"
	case len(calls) > 0:
		kind = "tool_use"
	case textLen > 0:
		kind = "text"
	}
	return outputUsageEstimate{
		OutputTokens:      total,
		VisibleTextTokens: visible,
		Kind:              kind,
		Source:            "estimated",
		ToolCalls:         len(calls),
	}
}

// estimateOutputUsageFromLengths 供流式适配层在回调中累计工具语义长度。
func estimateOutputUsageFromLengths(textLen, toolBytes, toolCalls int) outputUsageEstimate {
	total := estimateCompletionTokens(textLen + toolBytes)
	visible := estimateCompletionTokens(textLen)
	kind := "unknown"
	switch {
	case textLen > 0 && toolCalls > 0:
		kind = "mixed"
	case toolCalls > 0:
		kind = "tool_use"
	case textLen > 0:
		kind = "text"
	}
	return outputUsageEstimate{
		OutputTokens:      total,
		VisibleTextTokens: visible,
		Kind:              kind,
		Source:            "estimated",
		ToolCalls:         toolCalls,
	}
}

// ── 业务层入口（sub2api 新增）─────────────────────────────────────────

// OutputUsage 是一次回复的输出用量估算（供流式适配层在回调中累计后调用）。
//
// ⚠️ Source 恒为 "estimated"：Cursor agent.v1 不返回任何可信的 token 用量字段，
// 这里是按约 3 字节/token 的启发式推算，不可当作上游精确用量上报。
type OutputUsage struct {
	OutputTokens      int
	VisibleTextTokens int
	Kind              string
	Source            string
	ToolCalls         int
	CacheReadTokens   int
	CacheWriteTokens  int
}

// EstimateOutputUsage 由流式回调累计出的长度推算输出用量。
// textLen 为可见文本字节数，toolBytes/toolCalls 为工具调用的语义字节数与次数。
func EstimateOutputUsage(textLen, toolBytes, toolCalls int) OutputUsage {
	e := estimateOutputUsageFromLengths(textLen, toolBytes, toolCalls)
	return OutputUsage{
		OutputTokens:      e.OutputTokens,
		VisibleTextTokens: e.VisibleTextTokens,
		Kind:              e.Kind,
		Source:            e.Source,
		ToolCalls:         e.ToolCalls,
		CacheReadTokens:   e.CacheReadTokens,
		CacheWriteTokens:  e.CacheWriteTokens,
	}
}

// ToolCallUsageBytes 返回单次工具调用计入输出用量的语义字节数。
func ToolCallUsageBytes(tc ToolCall) int { return toolCallUsageBytes(tc) }
