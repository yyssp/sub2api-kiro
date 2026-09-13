//go:build unit

package cursor

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件守护 Anthropic 消息 -> Cursor ChatMessage 的转换。
//
// 背景：最初的实现直接用 ParseContentBlocks 处理每一轮消息，而它只读顶层
// "text" 字段。tool_use 块（只有 name/id/input）与 tool_result 块（文本嵌在
// content 里）都会被解析成空串，于是整轮被上层当作空消息丢弃——模型看不到
// 自己上轮调用过什么工具、也看不到返回值，表现为无限重复调用同一个工具。
// agentic 场景下这是致命的，且不会有任何报错。

func TestParseAnthropicMessage_ToolUseIsNotDropped(t *testing.T) {
	raw := json.RawMessage(`[{"type":"tool_use","id":"toolu_01","name":"Read","input":{"file_path":"/a.go"}}]`)

	msgs := ParseAnthropicMessage("assistant", raw)
	require.NotEmpty(t, msgs, "tool_use 轮绝不能被丢弃")
	require.Equal(t, "assistant", msgs[0].Role)
	// 工具名、id、参数都必须保留——id 用于多轮配对，缺了模型无法把结果对上调用。
	require.Contains(t, msgs[0].Content, "Read")
	require.Contains(t, msgs[0].Content, "toolu_01")
	require.Contains(t, msgs[0].Content, "file_path")
}

func TestParseAnthropicMessage_ToolResultIsNotDropped(t *testing.T) {
	raw := json.RawMessage(`[{"type":"tool_result","tool_use_id":"toolu_01","content":[{"type":"text","text":"package main"}]}]`)

	msgs := ParseAnthropicMessage("user", raw)
	require.NotEmpty(t, msgs, "tool_result 轮绝不能被丢弃")
	require.Contains(t, msgs[0].Content, "toolu_01")
	require.Contains(t, msgs[0].Content, "package main")
}

func TestParseAnthropicMessage_ToolResultContentAsPlainString(t *testing.T) {
	// tool_result.content 也可能是裸字符串而不是块数组。
	raw := json.RawMessage(`[{"type":"tool_result","tool_use_id":"t9","content":"done"}]`)
	msgs := ParseAnthropicMessage("user", raw)
	require.Len(t, msgs, 1)
	require.Contains(t, msgs[0].Content, "done")
}

func TestParseAnthropicMessage_ToolResultsComeBeforeUserText(t *testing.T) {
	// ⚠️ 顺序是语义的一部分：工具结果属于"上一轮的回应"，必须排在本轮
	// 用户新输入之前，否则拍平后的历史时序错乱。
	raw := json.RawMessage(`[
		{"type":"tool_result","tool_use_id":"t1","content":"RESULT_A"},
		{"type":"text","text":"USER_ASK"}
	]`)

	msgs := ParseAnthropicMessage("user", raw)
	require.Len(t, msgs, 2)
	require.Contains(t, msgs[0].Content, "RESULT_A")
	require.Contains(t, msgs[1].Content, "USER_ASK")
}

func TestParseAnthropicMessage_MultipleToolResultsEachBecomeAMessage(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"tool_result","tool_use_id":"t1","content":"A"},
		{"type":"tool_result","tool_use_id":"t2","content":"B"}
	]`)
	msgs := ParseAnthropicMessage("user", raw)
	require.Len(t, msgs, 2)
	require.Contains(t, msgs[0].Content, "t1")
	require.Contains(t, msgs[1].Content, "t2")
}

func TestParseAnthropicMessage_AssistantTextAndToolUseShareOneMessage(t *testing.T) {
	// 助手一轮里的解说文本 + 工具调用是同一轮，不能拆成两条。
	raw := json.RawMessage(`[
		{"type":"text","text":"let me read it"},
		{"type":"tool_use","id":"t1","name":"Read","input":{}}
	]`)
	msgs := ParseAnthropicMessage("assistant", raw)
	require.Len(t, msgs, 1)
	require.Contains(t, msgs[0].Content, "let me read it")
	require.Contains(t, msgs[0].Content, "Read")
}

func TestParseAnthropicMessage_PlainStringContent(t *testing.T) {
	msgs := ParseAnthropicMessage("user", json.RawMessage(`"hello"`))
	require.Len(t, msgs, 1)
	require.Equal(t, "hello", msgs[0].Content)
}

func TestParseAnthropicMessage_EmptyContentIsDropped(t *testing.T) {
	require.Empty(t, ParseAnthropicMessage("user", json.RawMessage(`"   "`)))
	require.Empty(t, ParseAnthropicMessage("assistant", json.RawMessage(`[]`)))
}

func TestParseAnthropicMessage_EmptyRoleDefaultsToUser(t *testing.T) {
	msgs := ParseAnthropicMessage("", json.RawMessage(`"hi"`))
	require.Len(t, msgs, 1)
	require.Equal(t, "user", msgs[0].Role)
}

func TestParseAnthropicMessage_ImageBlockStillParses(t *testing.T) {
	// 加了 tool 分支后不能回归掉附件解析。
	// 1x1 PNG。
	raw := json.RawMessage(`[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="}}]`)
	msgs := ParseAnthropicMessage("user", raw)
	require.Len(t, msgs, 1)
	require.Len(t, msgs[0].Images, 1, "image 块必须仍被解析为附件")
}

// TestParseAnthropicMessage_FullAgenticTurnSurvivesFlattening 串起真实的
// Claude Code agentic 轮次：user 提问 -> assistant 调工具 -> user 回结果。
// 拍平后三轮的关键信息都必须出现在最终的单轮文本里。
func TestParseAnthropicMessage_FullAgenticTurnSurvivesFlattening(t *testing.T) {
	var all []ChatMessage
	all = append(all, ParseAnthropicMessage("user", json.RawMessage(`"read main.go"`))...)
	all = append(all, ParseAnthropicMessage("assistant", json.RawMessage(
		`[{"type":"tool_use","id":"toolu_A","name":"Read","input":{"file_path":"main.go"}}]`))...)
	all = append(all, ParseAnthropicMessage("user", json.RawMessage(
		`[{"type":"tool_result","tool_use_id":"toolu_A","content":"package main"}]`))...)

	require.Len(t, all, 3, "三轮都必须保留")

	flat := BuildAgentMessage(all, nil)
	require.Contains(t, flat, "read main.go")
	require.Contains(t, flat, "Read", "历史里必须能看到调用过 Read")
	require.Contains(t, flat, "toolu_A", "调用与结果必须靠 id 配对")
	require.Contains(t, flat, "package main", "工具返回值必须出现在历史里")
	require.True(t, strings.Contains(flat, "历史"), "多轮应使用中文历史边界标记")
}

// TestParseContentBlocksDoesNotHandleToolBlocks 固化 ParseContentBlocks 的
// 已知局限，防止有人再次误用它来处理整轮消息。
func TestParseContentBlocksDoesNotHandleToolBlocks(t *testing.T) {
	text, _, _ := ParseContentBlocks(json.RawMessage(
		`[{"type":"tool_use","id":"t1","name":"Read","input":{}}]`))
	require.Empty(t, strings.TrimSpace(text),
		"ParseContentBlocks 按设计不处理工具块——处理整轮消息请用 ParseAnthropicMessage")
}
