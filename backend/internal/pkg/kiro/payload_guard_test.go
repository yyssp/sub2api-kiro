package kiro

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// userMsg 构造一条「干净」的 User 消息（不带 toolResults），即合法切点。
func userMsg(text string) KiroHistoryMessage {
	return KiroHistoryMessage{UserInputMessage: &KiroUserInputMessage{Content: text}}
}

// userMsgWithToolResult 构造一条回应 toolUse 的 User 消息，**不是**合法切点。
func userMsgWithToolResult(text, toolUseID string) KiroHistoryMessage {
	return KiroHistoryMessage{UserInputMessage: &KiroUserInputMessage{
		Content: text,
		UserInputMessageContext: &KiroUserInputMessageContext{
			ToolResults: []KiroToolResult{{
				ToolUseID: toolUseID,
				Status:    "success",
				Content:   []KiroTextContent{{Text: "ok"}},
			}},
		},
	}}
}

func assistantMsg(text string) KiroHistoryMessage {
	return KiroHistoryMessage{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: text}}
}

func assistantMsgWithToolUse(text, toolUseID string) KiroHistoryMessage {
	return KiroHistoryMessage{AssistantResponseMessage: &KiroAssistantResponseMessage{
		Content: text,
		ToolUses: []KiroToolUse{{
			ToolUseID: toolUseID,
			Name:      "read_file",
			Input:     map[string]any{"path": "/a"},
		}},
	}}
}

func marshalPayload(t *testing.T, p *KiroPayload) []byte {
	t.Helper()
	b, err := json.Marshal(p)
	require.NoError(t, err)
	return b
}

func newPayload(history []KiroHistoryMessage) *KiroPayload {
	return &KiroPayload{ConversationState: KiroConversationState{
		ChatTriggerType: "MANUAL",
		ConversationID:  "conv-test",
		CurrentMessage: KiroCurrentMessage{
			UserInputMessage: KiroUserInputMessage{Content: "current"},
		},
		History: history,
	}}
}

// A-26（回归保护）：未超阈值时必须**完全不改动**。
// 这是整个守卫最重要的性质 —— 绝大多数请求走这条路径。
func TestEnforceKiroPayloadSizeLeavesSmallPayloadUntouched(t *testing.T) {
	history := []KiroHistoryMessage{userMsg("hi"), assistantMsg("hello")}
	payload := newPayload(history)
	original := marshalPayload(t, payload)

	got, result, err := enforceKiroPayloadSize(payload, original)
	require.NoError(t, err)
	require.Equal(t, original, got, "未超限时字节必须逐字节不变")
	require.False(t, result.Trimmed)
	require.False(t, result.StillOversized)
	require.Zero(t, result.DroppedItems)
	require.Len(t, payload.ConversationState.History, 2)
}

// A-21：超阈值的历史必须被裁到阈值内。
func TestEnforceKiroPayloadSizeTrimsOversizedHistory(t *testing.T) {
	t.Setenv("KIRO_MAX_PAYLOAD_BYTES", "8000")

	blob := strings.Repeat("x", 1000)
	var history []KiroHistoryMessage
	for i := 0; i < 40; i++ {
		history = append(history, userMsg(blob), assistantMsg(blob))
	}
	payload := newPayload(history)
	original := marshalPayload(t, payload)
	require.Greater(t, len(original), 8000, "前置条件：构造的负载必须真的超限")

	got, result, err := enforceKiroPayloadSize(payload, original)
	require.NoError(t, err)
	require.True(t, result.Trimmed)
	require.False(t, result.StillOversized)
	require.LessOrEqual(t, len(got), 8000)
	require.Greater(t, result.DroppedItems, 0)
	require.Less(t, len(payload.ConversationState.History), len(history))

	// 裁剪后的字节必须仍是合法 JSON，且 currentMessage 不受影响。
	var parsed KiroPayload
	require.NoError(t, json.Unmarshal(got, &parsed))
	require.Equal(t, "current", parsed.ConversationState.CurrentMessage.UserInputMessage.Content)
}

// A-22：**永不从 tool_use/toolResult 对中间切开**。
// 切点只能落在不带 toolResults 的 User 消息上。
func TestEnforceKiroPayloadSizeNeverCutsThroughToolPair(t *testing.T) {
	t.Setenv("KIRO_MAX_PAYLOAD_BYTES", "6000")

	blob := strings.Repeat("y", 800)
	var history []KiroHistoryMessage
	for i := 0; i < 20; i++ {
		id := "tu_" + strings.Repeat("a", i+1)
		history = append(history,
			userMsg(blob),
			assistantMsgWithToolUse(blob, id),
			userMsgWithToolResult(blob, id),
			assistantMsg(blob),
		)
	}
	payload := newPayload(history)
	got, result, err := enforceKiroPayloadSize(payload, marshalPayload(t, payload))
	require.NoError(t, err)
	require.True(t, result.Trimmed)

	var parsed KiroPayload
	require.NoError(t, json.Unmarshal(got, &parsed))
	requireNoOrphanedToolPairs(t, parsed.ConversationState.History)
}

// A-23：找不到干净切点时**宁可不裁**。
// 全是 toolResult 消息 → 无处可切 → 原样放行并标记 StillOversized。
func TestEnforceKiroPayloadSizeRefusesToCutWhenNoCleanPoint(t *testing.T) {
	t.Setenv("KIRO_MAX_PAYLOAD_BYTES", "2000")

	blob := strings.Repeat("z", 900)
	history := []KiroHistoryMessage{userMsg(blob)}
	for i := 0; i < 6; i++ {
		id := "tu_" + strings.Repeat("b", i+1)
		history = append(history,
			assistantMsgWithToolUse(blob, id),
			userMsgWithToolResult(blob, id),
		)
	}
	payload := newPayload(history)
	original := marshalPayload(t, payload)

	got, result, err := enforceKiroPayloadSize(payload, original)
	require.NoError(t, err)
	require.False(t, result.Trimmed, "没有干净切点时不得强行裁剪")
	require.True(t, result.StillOversized)
	require.Equal(t, original, got)
	require.Len(t, payload.ConversationState.History, len(history))
}

// A-24：裁剪后 history 必须以 User 开头，且无孤儿 toolResult/toolUse。
func TestEnforceKiroPayloadSizeKeepsHistoryWellFormed(t *testing.T) {
	t.Setenv("KIRO_MAX_PAYLOAD_BYTES", "5000")

	blob := strings.Repeat("w", 700)
	var history []KiroHistoryMessage
	for i := 0; i < 15; i++ {
		id := "tu_" + strings.Repeat("c", i+1)
		history = append(history,
			userMsg(blob),
			assistantMsgWithToolUse(blob, id),
			userMsgWithToolResult(blob, id),
		)
	}
	payload := newPayload(history)
	got, result, err := enforceKiroPayloadSize(payload, marshalPayload(t, payload))
	require.NoError(t, err)
	require.True(t, result.Trimmed)

	var parsed KiroPayload
	require.NoError(t, json.Unmarshal(got, &parsed))
	trimmed := parsed.ConversationState.History
	require.NotEmpty(t, trimmed)
	require.NotNil(t, trimmed[0].UserInputMessage, "history 必须以 User 消息开头")
	requireNoOrphanedToolPairs(t, trimmed)
}

// A-25：裁到只剩不可再裁、但仍超限时 → 软失败放行，**不报错**。
// 我们的阈值是社区经验值，不该比上游更严格地拒绝用户请求。
func TestEnforceKiroPayloadSizeSoftFailsWhenStillOversized(t *testing.T) {
	t.Setenv("KIRO_MAX_PAYLOAD_BYTES", "100")

	history := []KiroHistoryMessage{
		userMsg(strings.Repeat("q", 500)),
		assistantMsg(strings.Repeat("q", 500)),
		userMsg(strings.Repeat("q", 500)),
	}
	payload := newPayload(history)
	got, result, err := enforceKiroPayloadSize(payload, marshalPayload(t, payload))

	require.NoError(t, err, "超限是软失败，不能返回 error")
	require.NotEmpty(t, got, "必须仍返回可发送的负载")
	require.True(t, result.StillOversized)
	require.Greater(t, result.FinalBytes, result.LimitBytes)
}

// 阈值可配置：环境变量非法或缺失时回落到默认值。
func TestKiroPayloadSizeLimitEnvOverride(t *testing.T) {
	require.Equal(t, kiroDefaultMaxPayloadBytes, kiroPayloadSizeLimit())

	t.Setenv("KIRO_MAX_PAYLOAD_BYTES", "12345")
	require.Equal(t, 12345, kiroPayloadSizeLimit())

	t.Setenv("KIRO_MAX_PAYLOAD_BYTES", "not-a-number")
	require.Equal(t, kiroDefaultMaxPayloadBytes, kiroPayloadSizeLimit())

	t.Setenv("KIRO_MAX_PAYLOAD_BYTES", "-5")
	require.Equal(t, kiroDefaultMaxPayloadBytes, kiroPayloadSizeLimit())
}

// 切点选择的单元级断言：索引 0 不是合法切点（会让调用方死循环）。
func TestNextKiroHistoryCutPointNeverReturnsZero(t *testing.T) {
	history := []KiroHistoryMessage{userMsg("a"), userMsg("b")}
	require.Equal(t, 1, nextKiroHistoryCutPoint(history))

	require.Zero(t, nextKiroHistoryCutPoint([]KiroHistoryMessage{userMsg("only")}))
	require.Zero(t, nextKiroHistoryCutPoint(nil))
}

// requireNoOrphanedToolPairs 断言 history 中每个 toolResult 都能找到对应的
// toolUse，且每个 toolUse 都有对应的 toolResult。
func requireNoOrphanedToolPairs(t *testing.T, history []KiroHistoryMessage) {
	t.Helper()
	toolUses := map[string]bool{}
	toolResults := map[string]bool{}
	for _, h := range history {
		if h.AssistantResponseMessage != nil {
			for _, tu := range h.AssistantResponseMessage.ToolUses {
				toolUses[tu.ToolUseID] = true
			}
		}
		if h.UserInputMessage != nil && h.UserInputMessage.UserInputMessageContext != nil {
			for _, tr := range h.UserInputMessage.UserInputMessageContext.ToolResults {
				toolResults[tr.ToolUseID] = true
			}
		}
	}
	for id := range toolResults {
		require.True(t, toolUses[id], "孤儿 toolResult %q：找不到对应的 toolUse", id)
	}
	for id := range toolUses {
		require.True(t, toolResults[id], "孤儿 toolUse %q：找不到对应的 toolResult", id)
	}
}
