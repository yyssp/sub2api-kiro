//go:build unit

package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

// ── 请求转换：Claude Code 协议 → Cursor agent.v1 ──────────────────────
// 这些断言锁的是 Cursor 自身的协议特性，不是与其它平台的一致性。

func TestCursorRequestFromBody_FlattensConversationIntoSingleMessage(t *testing.T) {
	// agent.v1 是单轮协议：整段会话被拍平成一条 user 文本，
	// 不存在 messages 数组。
	body := []byte(`{
		"model": "claude-sonnet-4.5",
		"messages": [
			{"role": "user", "content": "第一个问题"},
			{"role": "assistant", "content": "第一个回答"},
			{"role": "user", "content": "第二个问题"}
		]
	}`)
	req := cursorRequestFromBody(body, "claude-sonnet-4.5")

	require.Contains(t, req.Message, "第一个问题")
	require.Contains(t, req.Message, "第一个回答")
	require.Contains(t, req.Message, "第二个问题")
	// ⚠️ 不得使用英文 "User:/Assistant:" 脚本格式——会让模型把拍平后的
	// 历史误判成用户粘贴的伪造 transcript 而拒答。
	require.NotContains(t, req.Message, "User:")
	require.NotContains(t, req.Message, "Assistant:")
}

func TestCursorRequestFromBody_SingleUserMessagePassesThroughVerbatim(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"你好"}]}`)
	req := cursorRequestFromBody(body, "auto")
	// 单轮不加历史边界标记，原文直传（再附带工具约束）。
	require.Contains(t, req.Message, "你好")
	require.NotContains(t, req.Message, "[已验证的会话历史开始]")
}

func TestCursorRequestFromBody_SystemIsNotSentUpstream(t *testing.T) {
	// ⚠️ 普通 Cursor 账号不支持 custom_system_prompt，上游会返回
	// invalid_argument。System 字段只保留在网关内部做 token 估算/诊断，
	// buildAgentClientMessage 不会把它编码进上游请求。
	body := []byte(`{
		"system": "你是一个严谨的助手",
		"messages": [{"role":"user","content":"hi"}]
	}`)
	req := cursorRequestFromBody(body, "auto")
	require.NotEmpty(t, req.System, "System 仍应保留供网关内部使用")

	// 断言真正发往上游的字节里不含该系统提示。
	require.NotContains(t, string(cursor.BuildUpstreamClientMessageForTest(req)),
		"你是一个严谨的助手",
		"system 绝不能出现在上游请求体中")
}

func TestCursorRequestFromBody_SystemAcceptsBlockArrayForm(t *testing.T) {
	body := []byte(`{
		"system": [{"type":"text","text":"块一"},{"type":"text","text":"块二"}],
		"messages": [{"role":"user","content":"hi"}]
	}`)
	req := cursorRequestFromBody(body, "auto")
	require.Contains(t, req.System, "块一")
	require.Contains(t, req.System, "块二")
}

func TestCursorRequestFromBody_ToolSchemaIsJSONString(t *testing.T) {
	// ToolDef.InputSchema 是 JSON 字符串而非对象。
	body := []byte(`{
		"messages": [{"role":"user","content":"hi"}],
		"tools": [{
			"name": "Read",
			"description": "读文件",
			"input_schema": {"type":"object","properties":{"path":{"type":"string"}}}
		}]
	}`)
	req := cursorRequestFromBody(body, "auto")
	require.Len(t, req.Tools, 1)
	require.Equal(t, "Read", req.Tools[0].Name)
	require.True(t, json.Valid([]byte(req.Tools[0].InputSchema)),
		"InputSchema 必须是合法 JSON 字符串")
	require.Contains(t, req.Tools[0].InputSchema, "properties")
}

func TestCursorRequestFromBody_MissingToolSchemaDegradesToEmptyObject(t *testing.T) {
	body := []byte(`{
		"messages": [{"role":"user","content":"hi"}],
		"tools": [{"name":"NoSchema"}]
	}`)
	req := cursorRequestFromBody(body, "auto")
	require.Len(t, req.Tools, 1)
	require.Equal(t, "{}", req.Tools[0].InputSchema, "缺失 schema 应退化成 {} 而不是空串")
}

func TestCursorRequestFromBody_OnlyCurrentTurnAttachmentsAreAttached(t *testing.T) {
	// ⚠️ 重放历史图片会让上游报 "Image not found"，
	// 因此只有最后一轮的附件进入 selected_context。
	img := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	body := []byte(`{"messages":[
		{"role":"user","content":[
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + img + `"}},
			{"type":"text","text":"历史轮带图"}
		]},
		{"role":"assistant","content":"好的"},
		{"role":"user","content":"当前轮无图"}
	]}`)
	req := cursorRequestFromBody(body, "auto")
	require.Empty(t, req.Images, "历史轮的图片不得重放到 selected_context")
	require.Contains(t, req.Message, "历史轮带图", "历史文本仍应保留在拍平消息里")
}

func TestCursorRequestFromBody_StripsGatewayModelPrefix(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, cursor.StripPrefix("cursor/auto"),
		cursorRequestFromBody(body, "cursor/auto").Model)
}

func TestEstimateCursorInputTokens_NeverZero(t *testing.T) {
	// 0 会让计费/限流把请求当成空请求。
	require.GreaterOrEqual(t, estimateCursorInputTokens(cursor.AgentRequest{}), 1)
	require.Greater(t,
		estimateCursorInputTokens(cursor.AgentRequest{Message: strings.Repeat("a", 300)}),
		1)
}

// ── 响应转换：Cursor 回调 → Claude Code (Anthropic) SSE ───────────────

// cursorSSERecorder 收集 emitter 产出的事件序列。
type cursorSSERecorder struct {
	events []string
	data   []map[string]any
}

func (r *cursorSSERecorder) write(event string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	r.events = append(r.events, event)
	r.data = append(r.data, decoded)
	return nil
}

func (r *cursorSSERecorder) indicesFor(event string) []float64 {
	var out []float64
	for i, e := range r.events {
		if e == event {
			if idx, ok := r.data[i]["index"].(float64); ok {
				out = append(out, idx)
			}
		}
	}
	return out
}

func newTestEmitter() (*cursorAnthropicEmitter, *cursorSSERecorder) {
	rec := &cursorSSERecorder{}
	return newCursorAnthropicEmitter(rec.write, "claude-sonnet-4.5", time.Now()), rec
}

func TestCursorEmitter_TextOnlyProducesWellFormedSequence(t *testing.T) {
	e, rec := newTestEmitter()
	e.OnText("你好")
	e.OnText("，世界")
	require.NoError(t, e.finish(ClaudeUsage{OutputTokens: 3}))

	require.Equal(t, []string{
		"message_start", "content_block_start", "content_block_delta",
		"content_block_delta", "content_block_stop", "message_delta", "message_stop",
	}, rec.events)
	require.Equal(t, "end_turn", rec.data[5]["delta"].(map[string]any)["stop_reason"])
}

func TestCursorEmitter_ToolBlocksComeAfterTextBlocks(t *testing.T) {
	// ⚠️ 这是 Cursor 特有的顺序约束：协议层 onText 实时流式回调，
	// 而 onTool 在流结束后批量补发。按到达顺序编号会产生交错且不闭合
	// 的内容块，Claude Code 解析会失败。
	e, rec := newTestEmitter()
	e.OnText("先说话")
	e.OnTool(cursor.ToolCall{ID: "toolu_1", Name: "Read", Input: json.RawMessage(`{"path":"a.go"}`)})
	require.NoError(t, e.finish(ClaudeUsage{OutputTokens: 5}))

	starts := rec.indicesFor("content_block_start")
	stops := rec.indicesFor("content_block_stop")
	require.Equal(t, []float64{0, 1}, starts, "文本块 index=0，工具块 index=1")
	require.ElementsMatch(t, starts, stops, "每个开启的块都必须闭合")

	// 文本块必须在工具块之前闭合
	require.Equal(t, float64(0), stops[0])
	require.Equal(t, "tool_use", rec.data[len(rec.data)-2]["delta"].(map[string]any)["stop_reason"])
}

func TestCursorEmitter_ThinkingBlockClosesBeforeText(t *testing.T) {
	e, rec := newTestEmitter()
	e.OnReasoning("我在想")
	e.OnText("答案是")
	require.NoError(t, e.finish(ClaudeUsage{}))

	require.ElementsMatch(t, rec.indicesFor("content_block_start"), rec.indicesFor("content_block_stop"))
	// 第一个块是 thinking
	require.Equal(t, "thinking",
		rec.data[1]["content_block"].(map[string]any)["type"])
	// 第二个块是 text
	var textBlockSeen bool
	for i, ev := range rec.events {
		if ev == "content_block_start" {
			if rec.data[i]["content_block"].(map[string]any)["type"] == "text" {
				textBlockSeen = true
			}
		}
	}
	require.True(t, textBlockSeen)
}

func TestCursorEmitter_ToolInputEmittedAsPartialJSON(t *testing.T) {
	e, rec := newTestEmitter()
	e.OnTool(cursor.ToolCall{ID: "toolu_x", Name: "Bash", Input: json.RawMessage(`{"cmd":"ls"}`)})
	require.NoError(t, e.finish(ClaudeUsage{}))

	var found bool
	for i, ev := range rec.events {
		if ev != "content_block_delta" {
			continue
		}
		delta := rec.data[i]["delta"].(map[string]any)
		if delta["type"] == "input_json_delta" {
			require.Equal(t, `{"cmd":"ls"}`, delta["partial_json"])
			found = true
		}
	}
	require.True(t, found, "工具参数应以 input_json_delta 下发")
}

func TestCursorEmitter_EmptyToolInputBecomesEmptyObject(t *testing.T) {
	// 空参数必须是 "{}"，空串会让客户端 JSON 解析失败。
	e, rec := newTestEmitter()
	e.OnTool(cursor.ToolCall{ID: "t", Name: "NoArgs", Input: json.RawMessage("")})
	require.NoError(t, e.finish(ClaudeUsage{}))

	for i, ev := range rec.events {
		if ev == "content_block_delta" {
			if delta := rec.data[i]["delta"].(map[string]any); delta["type"] == "input_json_delta" {
				require.Equal(t, "{}", delta["partial_json"])
			}
		}
	}
}

func TestCursorEmitter_MissingToolIDIsGenerated(t *testing.T) {
	e, rec := newTestEmitter()
	e.OnTool(cursor.ToolCall{Name: "NoID", Input: json.RawMessage(`{}`)})
	require.NoError(t, e.finish(ClaudeUsage{}))

	for i, ev := range rec.events {
		if ev == "content_block_start" {
			block := rec.data[i]["content_block"].(map[string]any)
			if block["type"] == "tool_use" {
				require.NotEmpty(t, block["id"], "缺失的 tool id 必须补齐")
				require.True(t, strings.HasPrefix(block["id"].(string), "toolu_"))
			}
		}
	}
}

func TestCursorEmitter_FailMidStreamDoesNotEmitMessageStop(t *testing.T) {
	// ⚠️ 已开始推流后失败，绝不能补发 message_delta/message_stop：
	// 那会让 Claude Code 把失败请求误判成正常结束的一轮（end_turn），
	// 从而丢失重试机会。
	e, rec := newTestEmitter()
	e.OnText("部分输出")
	e.failMidStream("api_error", "upstream exploded")

	require.NotContains(t, rec.events, "message_stop")
	require.NotContains(t, rec.events, "message_delta")
	require.Equal(t, "error", rec.events[len(rec.events)-1])
	// 已开启的文本块仍须闭合
	require.ElementsMatch(t, rec.indicesFor("content_block_start"), rec.indicesFor("content_block_stop"))
}

func TestCursorEmitter_FirstTokenMsNilUntilStarted(t *testing.T) {
	e, _ := newTestEmitter()
	require.Nil(t, e.firstTokenMs(), "未产出任何内容时不应上报首字时间")
	e.OnText("x")
	require.NotNil(t, e.firstTokenMs())
}

func TestCursorEmitter_UsageIsEstimatedNotZero(t *testing.T) {
	// Cursor 上游不返回可信 token 用量，这里必须给出估算值而非 0，
	// 否则计费会把整轮对话记成免费。
	e, _ := newTestEmitter()
	e.OnText(strings.Repeat("字", 100))
	u := e.usage(42)
	require.Equal(t, 42, u.InputTokens)
	require.Greater(t, u.OutputTokens, 0)
}
