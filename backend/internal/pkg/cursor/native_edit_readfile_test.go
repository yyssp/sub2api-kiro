//go:build unit

package cursor

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件守护 ReadFileContent 表的构造与它对原生 Edit 的作用。
//
// ⚠️ 这条链路的失败模式是**硬失败**而不是降级：Cursor 原生 Edit（protobuf
// branch 12）只回传文件新内容，不回传 old_string。completeNativeEditInput
// 必须从 ReadFileContent 里查出旧内容才能拼成 Claude Code 的 Edit 合同；
// 查不到就返回 false，调用方抛 ErrMalformedUpstreamTool 终止整个请求。
// 这张表为空时，每一次原生 Edit 都会让整轮对话直接报错。

func TestCompleteNativeEditInput_NilMapAlwaysFails(t *testing.T) {
	// 固化"漏填 ReadFileContent = 原生 Edit 必失败"这一事实，
	// 防止有人认为这张表可选。
	raw := json.RawMessage(`{"file_path":"/tmp/a.go","new_string":"new"}`)
	_, ok := completeNativeEditInput(raw, nil)
	require.False(t, ok, "没有 ReadFileContent 时原生 Edit 无法补出 old_string")
}

func TestCompleteNativeEditInput_SucceedsWithReadFileContent(t *testing.T) {
	files := map[string]string{"/tmp/a.go": "old body"}
	raw := json.RawMessage(`{"file_path":"/tmp/a.go","new_string":"new body"}`)

	out, ok := completeNativeEditInput(raw, files)
	require.True(t, ok)

	var got map[string]string
	require.NoError(t, json.Unmarshal(out, &got))
	require.Equal(t, "old body", got["old_string"], "old_string 必须来自 Read 的历史内容")
	require.Equal(t, "new body", got["new_string"])
	require.Equal(t, "/tmp/a.go", got["file_path"])
}

func TestParseAnthropicReadFileContents_StripsLineNumbers(t *testing.T) {
	msgs := []AnthropicRawMessage{
		{Role: "assistant", Content: json.RawMessage(
			`[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/x/y.go"}}]`)},
		{Role: "user", Content: json.RawMessage(
			`[{"type":"tool_result","tool_use_id":"t1","content":"1\tline one\n2\tline two"}]`)},
	}
	files := ParseAnthropicReadFileContents(msgs)
	require.Equal(t, "line one\nline two", files["/x/y.go"])
}

func TestParseAnthropicReadFileContents_RejectsErroredResult(t *testing.T) {
	// ⚠️ 把错误文本当成文件旧内容，会让 Edit 基于错误的 old_string 改文件。
	msgs := []AnthropicRawMessage{
		{Role: "assistant", Content: json.RawMessage(
			`[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/x/y.go"}}]`)},
		{Role: "user", Content: json.RawMessage(
			`[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"1\tENOENT"}]`)},
	}
	require.Empty(t, ParseAnthropicReadFileContents(msgs))
}

func TestParseAnthropicReadFileContents_RejectsNonReadToolResult(t *testing.T) {
	// 只有 Read 的结果才是文件内容；Bash 输出等一律不能采纳。
	msgs := []AnthropicRawMessage{
		{Role: "assistant", Content: json.RawMessage(
			`[{"type":"tool_use","id":"t1","name":"Bash","input":{"file_path":"/x/y.go"}}]`)},
		{Role: "user", Content: json.RawMessage(
			`[{"type":"tool_result","tool_use_id":"t1","content":"1\tsome output"}]`)},
	}
	require.Empty(t, ParseAnthropicReadFileContents(msgs))
}

func TestParseAnthropicReadFileContents_RejectsUnnumberedText(t *testing.T) {
	msgs := []AnthropicRawMessage{
		{Role: "assistant", Content: json.RawMessage(
			`[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/x/y.go"}}]`)},
		{Role: "user", Content: json.RawMessage(
			`[{"type":"tool_result","tool_use_id":"t1","content":"no line numbers here"}]`)},
	}
	require.Empty(t, ParseAnthropicReadFileContents(msgs))
}

func TestParseAnthropicReadFileContents_UnmatchedIDIsIgnored(t *testing.T) {
	// tool_result 的 id 对不上任何 Read 调用时不能采纳。
	msgs := []AnthropicRawMessage{
		{Role: "assistant", Content: json.RawMessage(
			`[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/x/y.go"}}]`)},
		{Role: "user", Content: json.RawMessage(
			`[{"type":"tool_result","tool_use_id":"OTHER","content":"1\tbody"}]`)},
	}
	require.Empty(t, ParseAnthropicReadFileContents(msgs))
}

func TestParseAnthropicReadFileContents_LaterReadWins(t *testing.T) {
	// 同一文件被读两次时，后一次的内容才是最新的旧内容。
	msgs := []AnthropicRawMessage{
		{Role: "assistant", Content: json.RawMessage(
			`[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/x/y.go"}}]`)},
		{Role: "user", Content: json.RawMessage(
			`[{"type":"tool_result","tool_use_id":"t1","content":"1\tv1"}]`)},
		{Role: "assistant", Content: json.RawMessage(
			`[{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"/x/y.go"}}]`)},
		{Role: "user", Content: json.RawMessage(
			`[{"type":"tool_result","tool_use_id":"t2","content":"1\tv2"}]`)},
	}
	require.Equal(t, "v2", ParseAnthropicReadFileContents(msgs)["/x/y.go"])
}

// TestReadThenEditRoundTrip 串起完整链路：历史里的 Read -> 构表 -> 原生 Edit 补全。
func TestReadThenEditRoundTrip(t *testing.T) {
	msgs := []AnthropicRawMessage{
		{Role: "assistant", Content: json.RawMessage(
			`[{"type":"tool_use","id":"r1","name":"Read","input":{"file_path":"/repo/m.go"}}]`)},
		{Role: "user", Content: json.RawMessage(
			`[{"type":"tool_result","tool_use_id":"r1","content":"1\tpackage main"}]`)},
	}

	files := ParseAnthropicReadFileContents(msgs)
	out, ok := completeNativeEditInput(
		json.RawMessage(`{"file_path":"/repo/m.go","new_string":"package app"}`), files)
	require.True(t, ok, "读过的文件必须能完成原生 Edit 补全")

	var got map[string]string
	require.NoError(t, json.Unmarshal(out, &got))
	require.Equal(t, "package main", got["old_string"])
}
