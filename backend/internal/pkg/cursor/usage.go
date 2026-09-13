package cursor

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/anthropictokenizer"
)

// estimateCompletionTokens 按约 3 字节/token 的启发式口径估算 token 数。
//
// ⚠️ 只能用于工具调用参数（JSON/ASCII），不能用于模型正文。
// 正文必须走 EstimateOutputUsageFromText 的真实分词：字节口径会把中文
// 高估近 4 倍。正文侧曾经用它计费，是已修复的计费缺陷。
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

// ── 业务层入口（sub2api 新增）─────────────────────────────────────────

// OutputUsage 是一次回复的输出用量估算（供流式适配层在回调中累计后调用）。
//
// ⚠️ Source 恒为 "estimated"：Cursor agent.v1 不返回任何 token 用量字段，
// 整条链路由网关侧推算，不可当作上游精确用量上报。正文走 anthropictokenizer
// 真实分词，工具调用参数走字节启发式（见 estimateCompletionTokens）。
//
// OutputTokens 包含正文与已发送的工具调用，用于计费与账号累计；
// VisibleTextTokens 仅表示正文，纯工具轮为 0。
type OutputUsage struct {
	OutputTokens      int
	VisibleTextTokens int
	Kind              string
	Source            string
	ToolCalls         int
	// CacheReadTokens/CacheWriteTokens 是上游报告的 prompt 缓存命中/写入量。
	// Cursor agent.v1 目前不返回这两个值，所以常态为 0；保留字段是为了让计价
	// 链路有地方接收它们 —— 之前这里直接给 EstimateRequestCostUSD 传字面量 0，
	// 一旦上游开始返回缓存量，缓存读也不会被扣除，会按全价重复计费。
	CacheReadTokens  int
	CacheWriteTokens int
}

// ToolCallUsageBytes 返回单次工具调用计入输出用量的语义字节数。
func ToolCallUsageBytes(tc ToolCall) int { return toolCallUsageBytes(tc) }

// EstimateOutputUsageFromText 由**完整输出原文**推算输出用量。
//
// ⚠️ 正文必须走 anthropictokenizer 真实分词，不能用 len/3 之类的字节启发式。
// 按字节除 3 会把中文算成约 1 token/字，真实值约 0.25 token/字——中文输出
// 被系统性多计费近 4 倍。（曾经的 EstimateOutputUsage 就是这么算的，已删除。）
//
// ⚠️ 必须传完整文本，不能在流式回调里逐块调用再求和：BPE 合并跨越片段
// 边界，逐块计数会把每个边界都变成 token 边界，细粒度 delta 下高估 300%+。
// 调用方（emitter）因此缓冲原文，在流结束后一次性调用。
//
// toolBytes 仍按字节口径：工具调用参数是 JSON/ASCII，len/3 在该分布上
// 是合理近似，且不受中文畸变影响。
func EstimateOutputUsageFromText(text string, toolBytes, toolCalls int) OutputUsage {
	visible := anthropictokenizer.CountTokens(text)
	if visible == 0 && text != "" {
		visible = 1
	}
	total := visible + estimateCompletionTokens(toolBytes)
	kind := "unknown"
	switch {
	case text != "" && toolCalls > 0:
		kind = "mixed"
	case toolCalls > 0:
		kind = "tool_use"
	case text != "":
		kind = "text"
	}
	return OutputUsage{
		OutputTokens:      total,
		VisibleTextTokens: visible,
		Kind:              kind,
		Source:            "estimated",
		ToolCalls:         toolCalls,
	}
}
