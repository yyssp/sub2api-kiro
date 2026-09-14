package kiro

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// history 内部不得留下"孤儿 toolResult"（有 toolResult、但配对的 toolUse 已被裁掉）。
//
// 注意：这是**纵深防御**，不是线上 400 的成因。
// 实测表明 nextKiroHistoryCutPoint 会主动避开带 toolResult 的 user 消息，
// 常规形态下 history 内部并不会产生这种孤儿；线上 400 的真正成因见下一个用例
// （当前轮 toolResult 失配）。但 alignKiroHistoryToUser 会无条件丢弃开头的
// Assistant 消息，一旦切点形态变化就可能打破配对，因此这道清理必须保留。
//
// 既有的 removeOrphanedToolUses 只处理反方向（toolUse 没有 toolResult），
// 对本方向完全无效。
func TestTrimDoesNotLeaveOrphanedToolResult(t *testing.T) {
	// history 构造：切点会落到 index 1（Assistant 带 toolUse），
	// align 随后丢弃它，index 2 的 toolResult 就变成孤儿。
	history := []KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{Content: "第一轮提问"}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{
			Content:  "我来调用工具",
			ToolUses: []KiroToolUse{{ToolUseID: "tu_orphan", Name: "inventory_scan"}},
		}},
		{UserInputMessage: &KiroUserInputMessage{
			Content: "",
			UserInputMessageContext: &KiroUserInputMessageContext{
				ToolResults: []KiroToolResult{{ToolUseID: "tu_orphan", Status: "success"}},
			},
		}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "工具结果已收到"}},
		{UserInputMessage: &KiroUserInputMessage{Content: "第二轮提问"}},
	}

	// 与 payload_guard.go 裁剪循环里的收敛顺序保持一致。
	trimmed := history[1:]
	_, orphanedUses := validateToolPairing(trimmed, nil)
	removeOrphanedToolUses(trimmed, orphanedUses)
	trimmed = alignKiroHistoryToUser(trimmed)
	removeOrphanedToolResults(trimmed)

	// 收集裁剪后仍存在的 toolUse / toolResult ID。
	useIDs := map[string]bool{}
	resultIDs := map[string]bool{}
	for _, h := range trimmed {
		if h.AssistantResponseMessage != nil {
			for _, tu := range h.AssistantResponseMessage.ToolUses {
				useIDs[tu.ToolUseID] = true
			}
		}
		if h.UserInputMessage != nil && h.UserInputMessage.UserInputMessageContext != nil {
			for _, tr := range h.UserInputMessage.UserInputMessageContext.ToolResults {
				resultIDs[tr.ToolUseID] = true
			}
		}
	}

	for id := range resultIDs {
		require.Truef(t, useIDs[id],
			"toolResult %q 的配对 toolUse 已被裁掉，上游会直接回 400；"+
				"裁剪后必须清理孤儿 toolResult", id)
	}
}

// 线上 400 的真正成因：**当前轮**的 toolResult 失去配对的 toolUse。
//
// 当前轮的 toolResults 在 processMessages 阶段就依据未裁剪的 history
// 校验过了（translator.go 的 validateToolPairing），而体积守卫随后才裁剪 history。
// 配对的 toolUse 被切走后没有任何环节重新校验 —— 上游稳定回 400，
// 且位置恒为 messages.4.content（当前轮的位置），与实测完全一致。
//
// 修复策略是**补回 toolUse** 而不是删掉 toolResult：
// 当前轮的工具输出正是模型此刻推理的依据，删掉等于让它凭空失忆。
func TestTrimKeepsCurrentTurnToolResultPaired(t *testing.T) {
	bulk := strings.Repeat("x", 6000)
	var history []KiroHistoryMessage
	for i := 0; i < 10; i++ {
		history = append(history,
			KiroHistoryMessage{UserInputMessage: &KiroUserInputMessage{
				Content: fmt.Sprintf("第 %d 轮提问 %s", i, bulk)}},
			KiroHistoryMessage{AssistantResponseMessage: &KiroAssistantResponseMessage{
				Content: "回答 " + bulk}},
		)
	}
	// 最后一轮：assistant 发起 toolUse，其结果挂在 CurrentMessage 上。
	history = append(history, KiroHistoryMessage{
		AssistantResponseMessage: &KiroAssistantResponseMessage{
			Content:  "调用工具",
			ToolUses: []KiroToolUse{{ToolUseID: "tu_cur", Name: "inventory_scan"}},
		}})

	payload := &KiroPayload{ConversationState: KiroConversationState{
		ConversationID: "conv-current",
		CurrentMessage: KiroCurrentMessage{UserInputMessage: KiroUserInputMessage{
			Content: "Tool results provided.",
			UserInputMessageContext: &KiroUserInputMessageContext{
				ToolResults: []KiroToolResult{{ToolUseID: "tu_cur", Status: "success"}},
			}}},
		History: history,
	}}

	raw, err := json.Marshal(payload)
	require.NoError(t, err)

	out, result, err := enforceKiroPayloadSizeWithConfig(payload, raw,
		KiroPayloadGuardConfig{MaxWeight: 20000})
	require.NoError(t, err)
	require.True(t, result.Trimmed, "用例前提：必须真的走到 history_trim")

	var got KiroPayload
	require.NoError(t, json.Unmarshal(out, &got))

	live := map[string]bool{}
	for _, h := range got.ConversationState.History {
		if h.AssistantResponseMessage != nil {
			for _, tu := range h.AssistantResponseMessage.ToolUses {
				live[tu.ToolUseID] = true
			}
		}
	}

	ctx := got.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	require.NotNil(t, ctx)
	require.Len(t, ctx.ToolResults, 1,
		"当前轮的工具输出是模型推理依据，不该被丢弃")
	require.Truef(t, live[ctx.ToolResults[0].ToolUseID],
		"当前轮 toolResult %q 在裁剪后失去配对 toolUse，"+
			"上游会回 \"toolResult blocks ... exceeds ... toolUse blocks\" 400",
		ctx.ToolResults[0].ToolUseID)
}
