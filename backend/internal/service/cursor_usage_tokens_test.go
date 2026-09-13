//go:build unit

package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/anthropictokenizer"
	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

// ⚠️ 本文件守住的是「计费口径」而不是某个函数的实现细节。
// token 估算偏差不会让任何测试变红、不会报错、不会告警——
// 它只会安静地按错误金额向用户收费，直到有人去对账才暴露。

// 输出侧中文同样不得被高估。
//
// 旧口径 len/3 把中文按 3 字节/字算成约 1 token/字，是真实值的近 4 倍。
func TestEstimateOutputUsageFromText_CJKIsNotOverCounted(t *testing.T) {
	cjk := strings.Repeat("中", 100)
	got := cursor.EstimateOutputUsageFromText(cjk, 0, 0)
	require.InDelta(t, 25, got.OutputTokens, 3)
	require.Less(t, got.OutputTokens, len(cjk)/3,
		"不应退回 len/3 口径：中文会被多计费近 4 倍")
}

// ⚠️ 这是本次改动最核心的约束：token 必须对完整文本整体计数。
//
// BPE 的合并跨越片段边界。若在流式回调里逐块调用分词器再求和，
// 每个片段边界都会变成 token 边界——Cursor 的 text_delta 粒度很细，
// 实测按单字符切分时高估 300%+，比被替换掉的 len/3 还糟。
// emitter 因此必须缓冲原文、在流末一次性计数。
func TestEstimateOutputUsageFromText_MustCountWholeTextNotPerChunk(t *testing.T) {
	full := "帮我重构这段代码，注意保持向后兼容性。Please refactor and keep the API stable."

	whole := cursor.EstimateOutputUsageFromText(full, 0, 0).OutputTokens

	// 模拟「逐块计数再求和」的错误做法（单字符粒度）。
	perChunk := 0
	for _, r := range full {
		perChunk += cursor.EstimateOutputUsageFromText(string(r), 0, 0).OutputTokens
	}

	require.Greater(t, perChunk, whole*2,
		"前提校验：逐块累加确实会严重高估，否则本测试失去意义")
	require.Equal(t, anthropictokenizer.CountTokens(full), whole,
		"整体计数必须等于分词器对完整文本的结果")
}

// 非空输出至少记 1 token：0 会让计费把有内容的回复当成空回复。
func TestEstimateOutputUsageFromText_NonEmptyIsAtLeastOne(t *testing.T) {
	require.GreaterOrEqual(t, cursor.EstimateOutputUsageFromText("​", 0, 0).OutputTokens, 1)
	require.Equal(t, 0, cursor.EstimateOutputUsageFromText("", 0, 0).OutputTokens,
		"真正的空输出仍应为 0")
}

// 工具调用必须计入 OutputTokens，且纯工具轮的可见正文为 0。
func TestEstimateOutputUsageFromText_ToolCallsCounted(t *testing.T) {
	got := cursor.EstimateOutputUsageFromText("", 300, 2)
	require.Greater(t, got.OutputTokens, 0, "纯工具轮不得记 0 输出")
	require.Equal(t, 0, got.VisibleTextTokens, "纯工具轮没有可见正文")
	require.Equal(t, "tool_use", got.Kind)
	require.Equal(t, 2, got.ToolCalls)
}

func TestEstimateOutputUsageFromText_KindClassification(t *testing.T) {
	require.Equal(t, "text", cursor.EstimateOutputUsageFromText("hi", 0, 0).Kind)
	require.Equal(t, "mixed", cursor.EstimateOutputUsageFromText("hi", 10, 1).Kind)
	require.Equal(t, "unknown", cursor.EstimateOutputUsageFromText("", 0, 0).Kind)
}

// Source 恒为 estimated：Cursor agent.v1 不返回可信 usage，
// 一旦被当成上游精确值上报，对账时无法区分估算与实测。
func TestEstimateOutputUsageFromText_SourceAlwaysEstimated(t *testing.T) {
	require.Equal(t, "estimated", cursor.EstimateOutputUsageFromText("hi", 0, 0).Source)
}

// ── 计费缓冲的内存上限 ───────────────────────────────────────────────

// 缓冲必须封顶：不封顶时超长回复会让单请求驻留内存无界增长。
func TestAppendUsageText_CapsMemory(t *testing.T) {
	var buf strings.Builder
	chunk := strings.Repeat("a", 8192)
	for i := 0; i < 1000; i++ { // 远超 1MB 上限
		appendUsageText(&buf, chunk)
	}
	require.LessOrEqual(t, buf.Len(), cursorUsageTextCap, "缓冲超出上限，存在 OOM 风险")
	require.Greater(t, buf.Len(), 0)
}

// ⚠️ 截断必须落在 UTF-8 字符边界：切在多字节字符中间会留下半个序列。
func TestAppendUsageText_TruncatesOnRuneBoundary(t *testing.T) {
	var buf strings.Builder
	// 填到距上限只剩 2 字节，再追加一个 3 字节汉字。
	appendUsageText(&buf, strings.Repeat("a", cursorUsageTextCap-2))
	appendUsageText(&buf, "中")

	require.True(t, strings.ToValidUTF8(buf.String(), "�") == buf.String(),
		"缓冲里出现了半截 UTF-8 序列")
	require.LessOrEqual(t, buf.Len(), cursorUsageTextCap)
}

func TestAppendUsageText_StopsAppendingOnceFull(t *testing.T) {
	var buf strings.Builder
	appendUsageText(&buf, strings.Repeat("a", cursorUsageTextCap))
	before := buf.Len()
	appendUsageText(&buf, "more")
	require.Equal(t, before, buf.Len(), "已满后不应再追加")
}
