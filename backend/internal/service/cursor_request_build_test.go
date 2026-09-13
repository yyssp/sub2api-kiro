//go:build unit

package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

func cursorAgentRequestWithMessage(msg string) cursor.AgentRequest {
	return cursor.AgentRequest{Message: msg}
}

// 本文件守护 Anthropic 请求体 -> cursor.AgentRequest 的构造。
// 这些字段漏填都不会报错，只在真实对话里表现为难以归因的行为异常。

func TestCursorRequestFromBody_PopulatesReadFileContentForNativeEdit(t *testing.T) {
	// ⚠️ 这是硬失败路径，不是降级：Cursor 原生 Edit 只回传新内容，
	// completeNativeEditInput 必须靠 ReadFileContent 补出 old_string，
	// 查不到就抛 ErrMalformedUpstreamTool，整个请求直接失败。
	body := []byte(`{
		"model": "claude-sonnet-4-5",
		"messages": [
			{"role": "user", "content": "read it"},
			{"role": "assistant", "content": [
				{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/tmp/a.go"}}
			]},
			{"role": "user", "content": [
				{"type":"tool_result","tool_use_id":"t1","content":"1\tpackage main\n2\tfunc main(){}"}
			]}
		]
	}`)

	req := cursorRequestFromBody(body, "claude-sonnet-4-5")
	require.NotEmpty(t, req.ReadFileContent, "Read 的结果必须进 ReadFileContent，否则原生 Edit 必失败")
	// 行号前缀要被剥掉，留下的是文件真实内容。
	require.Equal(t, "package main\nfunc main(){}", req.ReadFileContent["/tmp/a.go"])
}

func TestCursorRequestFromBody_IgnoresErroredReadResult(t *testing.T) {
	// is_error 的 tool_result 不能当作文件内容——否则 Edit 会拿错误文本
	// 当 old_string 去改文件。
	body := []byte(`{
		"messages": [
			{"role": "assistant", "content": [
				{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/tmp/a.go"}}
			]},
			{"role": "user", "content": [
				{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"1\tENOENT"}
			]}
		]
	}`)
	require.Empty(t, cursorRequestFromBody(body, "claude-sonnet-4-5").ReadFileContent)
}

func TestCursorRequestFromBody_IgnoresNonLineNumberedReadResult(t *testing.T) {
	// 不符合 Claude Code Read 行号格式的文本一律不采纳。
	body := []byte(`{
		"messages": [
			{"role": "assistant", "content": [
				{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/tmp/a.go"}}
			]},
			{"role": "user", "content": [
				{"type":"tool_result","tool_use_id":"t1","content":"just some prose"}
			]}
		]
	}`)
	require.Empty(t, cursorRequestFromBody(body, "claude-sonnet-4-5").ReadFileContent)
}

// TestCursorRequestFromBody_ReadsEffortFromOutputConfig 断言 effort 字段被
// 读取并送进 applyClaudeEffortModel。
//
// ⚠️ 无法在单测里断言「effort=high 产出 -thinking-high」：
// applyClaudeEffortModel 刻意只从**实时模型清单**里选精确档位，清单为空时
// （单测环境）它对任何输入都是 no-op——这是"绝不就近降级/升级档位"的设计。
// 所以这里断言的是接线本身没断：解析顺序为 StripPrefix -> effort（与 ai2api
// 的 anthropic.go:470-471 一致），且非法 effort 不会破坏模型名。
func TestCursorRequestFromBody_ReadsEffortFromOutputConfig(t *testing.T) {
	base := cursorRequestFromBody(
		[]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "claude-sonnet-4-5").Model

	for _, effort := range []string{"high", "low", "max", "not-a-tier"} {
		body := []byte(`{"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"` + effort + `"}}`)
		got := cursorRequestFromBody(body, "claude-sonnet-4-5").Model
		require.NotEmpty(t, got)
		// 清单为空时必须原样返回基础模型，绝不捏造一个不存在的档位变体。
		require.Equal(t, base, got,
			"没有实时模型清单时不得凭空造出 thinking 变体（effort=%s）", effort)
	}
}

func TestCursorRequestFromBody_EffortIsWiredNotDropped(t *testing.T) {
	// 直接验证协议层入口可用且顺序正确：先 StripPrefix，再套 effort。
	stripped := cursor.StripPrefix("claude-sonnet-4-5")
	require.Equal(t, stripped, cursor.ApplyClaudeEffortModel(stripped, ""),
		"空 effort 必须是 no-op")
	require.Equal(t, stripped,
		cursorRequestFromBody([]byte(`{"messages":[{"role":"user","content":"hi"}]}`),
			"claude-sonnet-4-5").Model,
		"无 effort 时最终模型应等于 StripPrefix 的结果")
}

func TestCursorRequestFromBody_MaxModeFollowsModelSuffix(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	require.True(t, cursorRequestFromBody(body, "claude-sonnet-4-5-max").MaxMode)
	require.False(t, cursorRequestFromBody(body, "claude-sonnet-4-5").MaxMode)
}

func TestCursorRequestFromBody_ToolTurnsSurviveInMessage(t *testing.T) {
	// 回归守护：工具轮曾被整轮丢弃，导致模型重复调用同一个工具。
	body := []byte(`{
		"messages": [
			{"role": "user", "content": "read it"},
			{"role": "assistant", "content": [
				{"type":"tool_use","id":"toolu_X","name":"Read","input":{"file_path":"a.go"}}
			]},
			{"role": "user", "content": [
				{"type":"tool_result","tool_use_id":"toolu_X","content":"FILE_BODY"}
			]}
		]
	}`)
	msg := cursorRequestFromBody(body, "claude-sonnet-4-5").Message
	require.Contains(t, msg, "toolu_X", "工具调用与结果必须靠 id 出现在拍平后的历史里")
	require.Contains(t, msg, "FILE_BODY")
}

func TestCursorRequestFromBody_SystemIsNotEmptyButStaysLocal(t *testing.T) {
	// system 参与网关内部估算，但不会编码给上游（普通账号不支持
	// custom_system_prompt）。这里只断言它被解析出来了。
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"system":"BE_TERSE"}`)
	require.Contains(t, cursorRequestFromBody(body, "claude-sonnet-4-5").System, "BE_TERSE")
}

func TestEstimateCursorInputTokens_CJKIsNotOverCounted(t *testing.T) {
	// ⚠️ 中文是本项目的主要使用场景，高估会直接变成系统性多计费。
	// 历史上这里踩过两次坑，都被「看起来合理」的启发式骗过：
	//   len()/3      -> 100 token（约 1 token/字，高估 4 倍）
	//   字符加权 /4  -> 50 token（高估 2 倍）
	// 真实 BPE 分词：100 个「中」约 25 token。只有真分词器能给出这个值。
	cjk := strings.Repeat("中", 100)
	got := estimateCursorInputTokens(cursorAgentRequestWithMessage(cjk))
	require.InDelta(t, 25, got, 3)
	require.Less(t, got, 50, "不应退回任何按长度的启发式口径（字符加权或 len/3）")
}

func TestEstimateCursorInputTokens_ASCII(t *testing.T) {
	// 纯 ASCII 下真实分词与旧启发式恰好接近（约 4 字符/token），
	// 这正是旧口径能长期蒙混过关的原因——它只在英文上对。
	ascii := strings.Repeat("a", 400)
	require.InDelta(t, 100, estimateCursorInputTokens(cursorAgentRequestWithMessage(ascii)), 5)
}

func TestEstimateCursorInputTokens_EmptyIsAtLeastOne(t *testing.T) {
	require.GreaterOrEqual(t, estimateCursorInputTokens(cursorAgentRequestWithMessage("")), 1)
}
