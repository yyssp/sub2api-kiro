package kiro

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func toolUseFrame(t *testing.T, id, name string) []byte {
	return buildEventStreamFrame(t, "toolUseEvent", map[string]any{
		"toolUseEvent": map[string]any{"toolUseId": id, "name": name, "input": `{"command":"ls ` + id + `"}`, "stop": true},
	})
}

func TestDisableParallelToolUseKeepsOnlyFirstToolUse(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","tool_choice":{"type":"auto","disable_parallel_tool_use":true},
		"tools":[{"name":"Bash","description":"run","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}}],
		"messages":[{"role":"user","content":"go"}]}`)
	res, err := BuildKiroPayloadWithContext(body, "claude-sonnet-4.5", "", "AI_EDITOR", nil)
	require.NoError(t, err)
	require.Equal(t, 1, res.Context.MaxResponseToolUses)

	stream := bytes.NewBuffer(nil)
	_, _ = stream.Write(toolUseFrame(t, "t1", "Bash"))
	_, _ = stream.Write(toolUseFrame(t, "t2", "Bash"))
	var out bytes.Buffer
	_, err = StreamEventStreamAsAnthropicWithContext(context.Background(), stream, &out, "claude-sonnet-4-5", 9, res.Context)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(out.String(), `"type":"tool_use"`))

	nonStream := bytes.NewBuffer(nil)
	_, _ = nonStream.Write(toolUseFrame(t, "t1", "Bash"))
	_, _ = nonStream.Write(toolUseFrame(t, "t2", "Bash"))
	parsed, err := ParseNonStreamingEventStreamWithContext(nonStream, "claude-sonnet-4-5", res.Context)
	require.NoError(t, err)
	tools := 0
	for _, block := range gjson.GetBytes(parsed.ResponseBody, "content").Array() {
		if block.Get("type").String() == "tool_use" {
			tools++
		}
	}
	require.Equal(t, 1, tools)
}

func TestParallelToolUseAllowedByDefault(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","tool_choice":{"type":"auto"},"messages":[{"role":"user","content":"go"}]}`)
	res, err := BuildKiroPayloadWithContext(body, "claude-sonnet-4.5", "", "AI_EDITOR", nil)
	require.NoError(t, err)
	require.Zero(t, res.Context.MaxResponseToolUses)
}

// 历史中的 web_search 调用和结果不能丢，否则模型后续轮次看不到搜过什么。
func TestAssistantServerToolBlocksRenderAsText(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[
		{"role":"user","content":"news?"},
		{"role":"assistant","content":[
			{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"go 1.27 release"}},
			{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","title":"Go 1.27","url":"https://go.dev/doc/go1.27"}]},
			{"type":"text","text":"Go 1.27 is out."}
		]},
		{"role":"user","content":"more"}
	]}`)
	res, err := BuildKiroPayloadWithContext(body, "claude-sonnet-4.5", "", "AI_EDITOR", nil)
	require.NoError(t, err)
	var assistant string
	for _, h := range gjson.GetBytes(res.Payload, "conversationState.history").Array() {
		if c := h.Get("assistantResponseMessage.content").String(); strings.Contains(c, "Go 1.27 is out") {
			assistant = c
		}
	}
	require.Contains(t, assistant, "go 1.27 release")
	require.Contains(t, assistant, "https://go.dev/doc/go1.27")
}

// Claude Code 的 system 首块是 billing-header 行，拼接时不能与下一句粘连。
func TestSystemBlocksAreJoinedWithNewline(t *testing.T) {
	system := gjson.Parse(`[{"type":"text","text":"x-anthropic-billing-header: cc_version=1"},{"type":"text","text":"You are Claude Code."}]`)
	require.Equal(t, "x-anthropic-billing-header: cc_version=1\nYou are Claude Code.", extractTextFromContentBlocks(system))
	already := gjson.Parse(`[{"type":"text","text":"a\n"},{"type":"text","text":"b"}]`)
	require.Equal(t, "a\nb", extractTextFromContentBlocks(already))
}
