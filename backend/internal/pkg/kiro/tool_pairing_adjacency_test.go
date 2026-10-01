package kiro

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 当前轮引用更早轮次的 toolUse：全局 ID 集合会放行，但 Kiro 按"上一轮"计数会 400。
func TestCurrentToolResultMustAnswerPreviousAssistant(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-5",
		"messages":[
			{"role":"user","content":"q1"},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_old","name":"Read","input":{"file_path":"a"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_old","content":"A"}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_new","name":"Read","input":{"file_path":"b"}}]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_new","content":"B"},
				{"type":"tool_result","tool_use_id":"toolu_stale","content":"stale"}
			]}
		]
	}`)
	res, err := BuildKiroPayloadWithContext(body, "claude-sonnet-4.5", "", "AI_EDITOR", nil)
	require.NoError(t, err)
	results := gjson.GetBytes(res.Payload, "conversationState.currentMessage.userInputMessage.userInputMessageContext.toolResults").Array()
	require.Len(t, results, 1)
	require.Equal(t, "toolu_new", results[0].Get("toolUseId").String())
}

// history 中的 toolResult 回应的不是紧邻上一条 assistant 时必须剔除，对应的 toolUse 成为孤儿一并清理。
func TestHistoryToolResultNotAdjacentIsDropped(t *testing.T) {
	history := []KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{Content: "q1"}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{ToolUses: []KiroToolUse{{ToolUseID: "tu_a", Name: "Read"}}}},
		{UserInputMessage: &KiroUserInputMessage{Content: "unrelated"}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "ok"}},
		{UserInputMessage: &KiroUserInputMessage{
			Content:                 "late result",
			UserInputMessageContext: &KiroUserInputMessageContext{ToolResults: []KiroToolResult{{ToolUseID: "tu_a"}}},
		}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "done"}},
	}

	_, orphaned := validateToolPairing(history, nil)
	require.True(t, orphaned["tu_a"])
	require.Empty(t, history[4].UserInputMessage.UserInputMessageContext.ToolResults)

	removeOrphanedToolUses(history, orphaned)
	require.Empty(t, history[1].AssistantResponseMessage.ToolUses)
}

func TestToolPairingKeepsAdjacentPairsAndDeduplicates(t *testing.T) {
	history := []KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{Content: "q"}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{ToolUses: []KiroToolUse{{ToolUseID: "tu_1"}, {ToolUseID: "tu_2"}}}},
		{UserInputMessage: &KiroUserInputMessage{
			UserInputMessageContext: &KiroUserInputMessageContext{ToolResults: []KiroToolResult{{ToolUseID: "tu_1"}, {ToolUseID: "tu_1"}, {ToolUseID: "tu_2"}}},
		}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{ToolUses: []KiroToolUse{{ToolUseID: "tu_3"}}}},
	}
	current, orphaned := validateToolPairing(history, []KiroToolResult{{ToolUseID: "tu_3"}})
	require.Empty(t, orphaned)
	require.Len(t, history[2].UserInputMessage.UserInputMessageContext.ToolResults, 2)
	require.Len(t, current, 1)
}
