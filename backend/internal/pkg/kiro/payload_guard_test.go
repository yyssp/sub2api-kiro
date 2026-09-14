package kiro

import (
	"encoding/json"
	"strconv"
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
	t.Setenv("KIRO_MAX_PAYLOAD_WEIGHT", "8000")

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
	t.Setenv("KIRO_MAX_PAYLOAD_WEIGHT", "6000")

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
	t.Setenv("KIRO_MAX_PAYLOAD_WEIGHT", "2000")

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
	t.Setenv("KIRO_MAX_PAYLOAD_WEIGHT", "5000")

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
	t.Setenv("KIRO_MAX_PAYLOAD_WEIGHT", "100")

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
	require.Greater(t, result.FinalWeight, result.LimitWeight)
}

// 阈值可配置：环境变量非法或缺失时回落到默认值。
func TestKiroPayloadSizeLimitEnvOverride(t *testing.T) {
	require.Equal(t, kiroDefaultMaxPayloadWeight, kiroPayloadSizeLimit())

	t.Setenv("KIRO_MAX_PAYLOAD_WEIGHT", "12345")
	require.Equal(t, 12345, kiroPayloadSizeLimit())

	t.Setenv("KIRO_MAX_PAYLOAD_WEIGHT", "not-a-number")
	require.Equal(t, kiroDefaultMaxPayloadWeight, kiroPayloadSizeLimit())

	t.Setenv("KIRO_MAX_PAYLOAD_WEIGHT", "-5")
	require.Equal(t, kiroDefaultMaxPayloadWeight, kiroPayloadSizeLimit())
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

// PayloadTrimStats 必须能跨包读出裁剪结果。
//
// 背景：kiroPayloadTrimResult 是包内类型，嵌在导出的 KiroRequestContext 里。
// 跨包调用方（internal/service）拿不到字段，导致守卫是否生效在线上无法观测 ——
// 「裁剪成功」和「本来就没超限」外部表现完全一样（都是 200）。
func TestPayloadTrimStatsExposedAcrossPackages(t *testing.T) {
	// 显式压低阈值：默认阈值(加权 130 万)下这个负载根本不会触发守卫，
	// 而本用例断言的是「统计值能跨包读出」，与具体阈值无关。
	t.Setenv("KIRO_MAX_PAYLOAD_WEIGHT", "200000")

	blob := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 180)
	var msgs []map[string]any
	for i := 0; i < 60; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": blob})
		msgs = append(msgs, map[string]any{"role": "assistant", "content": "ok"})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "done?"})
	body, err := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-5-20250929", "max_tokens": 100, "messages": msgs})
	require.NoError(t, err)

	res, err := BuildKiroPayloadWithContext(body, "claude-sonnet-4.5", "arn:test", "AI_EDITOR", nil)
	require.NoError(t, err)

	s := res.Context.PayloadTrimStats()
	require.True(t, s.Triggered(), "超限负载必须触发守卫")
	require.True(t, s.Trimmed, "无可压缩内容时必须落到裁剪")
	require.False(t, s.StillOversized)
	require.False(t, s.Rejected)
	require.Greater(t, s.OriginalWeight, s.LimitWeight, "原始体积应超限")
	require.LessOrEqual(t, s.FinalWeight, s.LimitWeight, "裁剪后必须在限内")
	require.Greater(t, s.DroppedItems, 0, "必须丢弃了历史条目")
	require.Contains(t, s.Stages, "history_trim")
}

// 未超限时 PayloadTrimStats 必须全零 —— 保证日志不产生噪声。
func TestPayloadTrimStatsSilentWhenUnderLimit(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5-20250929","max_tokens":16,
		"messages":[{"role":"user","content":"hi"}]}`)
	res, err := BuildKiroPayloadWithContext(body, "claude-sonnet-4.5", "arn:test", "AI_EDITOR", nil)
	require.NoError(t, err)

	s := res.Context.PayloadTrimStats()
	require.False(t, s.Triggered(), "未超限时不得产生任何日志噪声")
	require.Zero(t, s.DroppedItems)
	require.Zero(t, s.CompressedItems)
	require.Empty(t, s.Stages)
}

// ---------------------------------------------------------------------------
// 加权口径（实测反解，见 payload_guard.go 顶部注释）
// ---------------------------------------------------------------------------

// 上游限的不是字节数：同样字节数的中文比 ASCII 更早触顶。
// 这条断言是整套阈值逻辑的地基，如果它退化成 len()，中文请求会被放行到 400。
func TestKiroPayloadWeightChargesNonASCIIMore(t *testing.T) {
	require.Equal(t, 5, kiroPayloadWeight([]byte("hello")), "ASCII 每字符计 1")

	cn := []byte("中文")
	require.Equal(t, 6, len(cn), "前置条件：UTF-8 下每个汉字 3 字节")
	require.Equal(t, 2*kiroNonASCIIWeight, kiroPayloadWeight(cn), "非 ASCII 每字符计 8，与字节数无关")

	// 关键性质：字节数相同、加权值不同。
	ascii := []byte(strings.Repeat("a", 300))
	chinese := []byte(strings.Repeat("填", 100)) // 同样 300 字节
	require.Equal(t, len(ascii), len(chinese))
	require.Greater(t, kiroPayloadWeight(chinese), kiroPayloadWeight(ascii),
		"同字节数下中文必须权重更高，否则中文请求会被放到上游才 400")
}

// 默认阈值必须落在实测确定的可行区间 (1,320,000, 1,360,000] 之下。
func TestKiroDefaultWeightLimitWithinMeasuredInterval(t *testing.T) {
	require.LessOrEqual(t, kiroDefaultMaxPayloadWeight, 1_320_000,
		"默认阈值必须 <= 实测最后一个通过点，否则会放行注定 400 的请求")
	require.Greater(t, kiroDefaultMaxPayloadWeight, 1_000_000,
		"过低会无谓地裁剪本可正常发送的请求")
}

// ---------------------------------------------------------------------------
// 压缩优先：这是用户的硬性要求 ——「除非压缩之后都无法减少了才进行裁剪」
// ---------------------------------------------------------------------------

// bigToolResultMsg 构造一条携带超大工具输出的 User 消息（合法配对）。
func bigToolResultMsg(toolUseID string, chars int) KiroHistoryMessage {
	return KiroHistoryMessage{UserInputMessage: &KiroUserInputMessage{
		Content: "tool done",
		UserInputMessageContext: &KiroUserInputMessageContext{
			ToolResults: []KiroToolResult{{
				ToolUseID: toolUseID,
				Status:    "success",
				Content:   []KiroTextContent{{Text: strings.Repeat("L", chars)}},
			}},
		},
	}}
}

// 最重要的一条：能靠压缩解决时，**一条历史都不许丢**。
func TestCompressionRunsBeforeAnyHistoryIsDropped(t *testing.T) {
	t.Setenv("KIRO_MAX_PAYLOAD_WEIGHT", "30000")

	history := []KiroHistoryMessage{
		userMsg("第一轮的暗号是 ALPHA-7"), // 放在最早一轮：被裁剪就会消失
		assistantMsgWithToolUse("我查一下", "tu_1"),
		bigToolResultMsg("tu_1", 60000), // 单条就足以超限，但可压缩
		assistantMsg("查完了"),
	}
	payload := newPayload(history)
	got, result, err := enforceKiroPayloadSize(payload, marshalPayload(t, payload))
	require.NoError(t, err)

	require.True(t, result.Compressed, "必须先尝试压缩")
	require.False(t, result.Trimmed, "压缩已能解决时，不得裁剪任何历史")
	require.Zero(t, result.DroppedItems)
	require.False(t, result.StillOversized)
	require.Equal(t, []string{"history_tool_results"}, result.Stages)
	require.LessOrEqual(t, result.FinalWeight, result.LimitWeight)

	// 最早一轮的暗号必须还在 —— 上下文没有静默丢失。
	var parsed KiroPayload
	require.NoError(t, json.Unmarshal(got, &parsed))
	require.Len(t, parsed.ConversationState.History, 4, "历史条目数不变")
	require.Contains(t, parsed.ConversationState.History[0].UserInputMessage.Content, "ALPHA-7")
}

// 压缩后仍然超限时，才允许退化为裁剪，且要记录两个阶段都跑过。
func TestTrimOnlyAsLastResortAfterCompression(t *testing.T) {
	t.Setenv("KIRO_MAX_PAYLOAD_WEIGHT", "20000")

	var history []KiroHistoryMessage
	for i := 0; i < 12; i++ {
		id := "tu_" + strconv.Itoa(i)
		history = append(history,
			userMsg(strings.Repeat("u", 3000)), // 纯文本，压缩阶段动不了
			assistantMsgWithToolUse("call", id),
			bigToolResultMsg(id, 20000), // 可压缩
		)
	}
	payload := newPayload(history)
	_, result, err := enforceKiroPayloadSize(payload, marshalPayload(t, payload))
	require.NoError(t, err)

	require.True(t, result.Compressed, "必须先压缩")
	require.True(t, result.Trimmed, "压缩不够时才裁剪")
	require.Greater(t, result.DroppedItems, 0)
	require.Contains(t, result.Stages, "history_tool_results")
	require.Equal(t, "history_trim", result.Stages[len(result.Stages)-1],
		"裁剪必须是最后一个阶段")
}

// ---------------------------------------------------------------------------
// 各压缩阶段的单元断言
// ---------------------------------------------------------------------------

// 工具输出截断必须保留头尾 —— 关键信息在开头(状态)和结尾(结论/错误)。
func TestCompressKiroHistoryToolResultsKeepsHeadAndTail(t *testing.T) {
	text := strings.Repeat("H", 100) + strings.Repeat("m", 50000) + strings.Repeat("T", 100)
	payload := newPayload([]KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{
			UserInputMessageContext: &KiroUserInputMessageContext{
				ToolResults: []KiroToolResult{{
					ToolUseID: "tu_1", Status: "success",
					Content: []KiroTextContent{{Text: text}},
				}},
			},
		}},
	})

	n := compressKiroHistoryToolResults(payload)
	require.Equal(t, 1, n)

	got := payload.ConversationState.History[0].UserInputMessage.
		UserInputMessageContext.ToolResults[0].Content[0].Text
	require.Less(t, len(got), len(text), "必须真的变小")
	require.True(t, strings.HasPrefix(got, strings.Repeat("H", 100)), "头部必须保留")
	require.True(t, strings.HasSuffix(got, strings.Repeat("T", 100)), "尾部必须保留")
	require.Contains(t, got, "代理已截断", "必须标注被截断，避免模型把残缺输出当完整的")

	// 幂等：已压缩过的不再重复计数。
	require.Zero(t, compressKiroHistoryToolResults(payload))
}

// 短工具输出不得被动 —— 避免对正常请求造成无谓损伤。
func TestCompressKiroHistoryToolResultsLeavesShortOutputUntouched(t *testing.T) {
	payload := newPayload([]KiroHistoryMessage{bigToolResultMsg("tu_1", 100)})
	require.Zero(t, compressKiroHistoryToolResults(payload))
	require.Equal(t, strings.Repeat("L", 100),
		payload.ConversationState.History[0].UserInputMessage.
			UserInputMessageContext.ToolResults[0].Content[0].Text)
}

// 历史思维链剥离：只动 history，且保留非 thinking 正文。
func TestStripKiroHistoryThinking(t *testing.T) {
	payload := newPayload([]KiroHistoryMessage{
		userMsg("q"),
		assistantMsg("<thinking>" + strings.Repeat("t", 5000) + "</thinking>可见结论"),
		assistantMsg("没有思维链"),
	})

	require.Equal(t, 1, stripKiroHistoryThinking(payload))
	require.Equal(t, "可见结论", payload.ConversationState.History[1].AssistantResponseMessage.Content)
	require.Equal(t, "没有思维链", payload.ConversationState.History[2].AssistantResponseMessage.Content)
	require.Zero(t, stripKiroHistoryThinking(payload), "幂等")
}

// 工具定义压缩：当前轮和历史轮都要覆盖（当前轮才是真正每次重发的大头）。
func TestCompressKiroToolDefinitions(t *testing.T) {
	longDesc := strings.Repeat("d", 5000)
	tools := []KiroToolWrapper{
		{ToolSpecification: KiroToolSpecification{Name: "big", Description: longDesc}},
		{ToolSpecification: KiroToolSpecification{Name: "small", Description: "短描述"}},
	}
	payload := newPayload([]KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{
			UserInputMessageContext: &KiroUserInputMessageContext{Tools: tools},
		}},
	})
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext =
		&KiroUserInputMessageContext{Tools: []KiroToolWrapper{
			{ToolSpecification: KiroToolSpecification{Name: "cur", Description: longDesc}},
		}}

	require.Equal(t, 2, compressKiroToolDefinitions(payload), "当前轮 1 个 + 历史 1 个")

	cur := payload.ConversationState.CurrentMessage.UserInputMessage.
		UserInputMessageContext.Tools[0].ToolSpecification
	require.Less(t, len(cur.Description), len(longDesc))
	require.Equal(t, "cur", cur.Name, "工具名绝不能被改动，否则调用直接失配")

	hist := payload.ConversationState.History[0].UserInputMessage.UserInputMessageContext.Tools
	require.Less(t, len(hist[0].ToolSpecification.Description), len(longDesc))
	require.Equal(t, "短描述", hist[1].ToolSpecification.Description, "短描述不得被动")

	// 幂等：on_upstream_400 重试路径会再跑一遍压缩，不得二次截断。
	before := cur.Description
	require.Zero(t, compressKiroToolDefinitions(payload))
	require.Equal(t, before, payload.ConversationState.CurrentMessage.UserInputMessage.
		UserInputMessageContext.Tools[0].ToolSpecification.Description)
}

// 丢弃历史图片：必须留下占位符，否则模型会以为用户从没发过图。
func TestDropKiroHistoryImagesLeavesPlaceholder(t *testing.T) {
	payload := newPayload([]KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{
			Content: "看这张图",
			Images: []KiroImage{
				{Format: "png", Source: KiroImageSource{Bytes: strings.Repeat("A", 100000)}},
				{Format: "webp", Source: KiroImageSource{Bytes: strings.Repeat("B", 100000)}},
			},
		}},
	})

	require.Equal(t, 2, dropKiroHistoryImages(payload), "按图片张数计数")
	msg := payload.ConversationState.History[0].UserInputMessage
	require.Empty(t, msg.Images)
	require.Contains(t, msg.Content, "看这张图", "原文必须保留")
	require.Contains(t, msg.Content, kiroDroppedImagePlaceholder)
	require.Zero(t, dropKiroHistoryImages(payload), "幂等")
}

// 当前轮的图片是模型正在推理的依据，绝不能丢。
func TestDropKiroHistoryImagesKeepsCurrentTurnImages(t *testing.T) {
	payload := newPayload(nil)
	payload.ConversationState.CurrentMessage.UserInputMessage.Images =
		[]KiroImage{{Format: "png", Source: KiroImageSource{Bytes: "AAAA"}}}

	require.Zero(t, dropKiroHistoryImages(payload))
	require.Len(t, payload.ConversationState.CurrentMessage.UserInputMessage.Images, 1,
		"当前轮图片必须原样保留")
}

// ---------------------------------------------------------------------------
// 三种超限行为
// ---------------------------------------------------------------------------

func oversizedPayload() (*KiroPayload, []byte) {
	var history []KiroHistoryMessage
	for i := 0; i < 30; i++ {
		history = append(history, userMsg(strings.Repeat("x", 2000)), assistantMsg("ok"))
	}
	p := newPayload(history)
	b, _ := json.Marshal(p)
	return p, b
}

// reject：不发上游，返回可识别的错误类型供上层映射成 4xx。
func TestOversizeBehaviorReject(t *testing.T) {
	payload, raw := oversizedPayload()

	_, result, err := enforceKiroPayloadSizeWithConfig(payload, raw,
		KiroPayloadGuardConfig{MaxWeight: 5000, Behavior: KiroOversizeReject})

	require.Error(t, err)
	var tooLarge *ErrKiroPayloadTooLarge
	require.ErrorAs(t, err, &tooLarge, "必须是可判别的类型，上层才能回 413 而不是 500")
	require.Equal(t, 5000, tooLarge.Limit)
	require.Greater(t, tooLarge.Weight, tooLarge.Limit)

	require.True(t, result.Rejected)
	require.False(t, result.Trimmed, "拒绝模式下不得改动负载")
	require.Len(t, payload.ConversationState.History, 60, "历史必须原封不动")
}

// on_upstream_400：预检阶段什么都不做，原样发上游。
func TestOversizeBehaviorOnUpstream400SkipsPreflight(t *testing.T) {
	payload, raw := oversizedPayload()

	got, result, err := enforceKiroPayloadSizeWithConfig(payload, raw,
		KiroPayloadGuardConfig{MaxWeight: 5000, Behavior: KiroOversizeOnUpstream400})

	require.NoError(t, err)
	require.Equal(t, raw, got, "预检不得改动一个字节")
	require.False(t, result.Trimmed)
	require.False(t, result.Compressed)
	require.False(t, result.Rejected)
	require.True(t, result.DeferredToUpstream, "必须标记出来，供调用方决定重试")
	require.False(t, result.StillOversized,
		"StillOversized 的语义是「压缩+裁剪跑到底仍塞不下」的软失败；"+
			"这里守卫一个字节都没动，混用会让每个大请求都误报成压缩失效")
	require.Len(t, payload.ConversationState.History, 60)
}

// 守卫没动过负载时不得回裁剪响应头 —— 否则 original==final 的一组头
// 会和"真的裁掉了历史"混淆，调用方无法区分。
func TestDeferredToUpstreamIsNotReportedAsTriggered(t *testing.T) {
	payload, raw := oversizedPayload()

	_, result, err := enforceKiroPayloadSizeWithConfig(payload, raw,
		KiroPayloadGuardConfig{MaxWeight: 5000, Behavior: KiroOversizeOnUpstream400})
	require.NoError(t, err)

	stats := KiroRequestContext{PayloadTrim: result}.PayloadTrimStats()
	require.False(t, stats.Triggered(), "未改动负载不算触发")
	require.True(t, stats.DeferredToUpstream, "但跨包视图必须能看到这个状态")
	require.Equal(t, stats.OriginalWeight, stats.FinalWeight, "一个字节都没动")
}

// on_upstream_400 收到上游 400 后的重试路径：DisableCompression=false 且
// 强制走缩减分支，等价于 compress_then_trim。
func TestOversizeBehaviorOnUpstream400RetryShrinks(t *testing.T) {
	payload, raw := oversizedPayload()

	got, result, err := enforceKiroPayloadSizeWithConfig(payload, raw,
		KiroPayloadGuardConfig{MaxWeight: 5000, Behavior: KiroOversizeCompressThenTrim})

	require.NoError(t, err)
	require.True(t, result.Trimmed)
	require.LessOrEqual(t, result.FinalWeight, result.LimitWeight)
	require.Less(t, len(got), len(raw))
}

// 未超限时，三种行为的表现必须完全一致：什么都不做。
func TestAllBehaviorsNoopWhenUnderLimit(t *testing.T) {
	for _, b := range []KiroOversizeBehavior{
		KiroOversizeCompressThenTrim, KiroOversizeReject, KiroOversizeOnUpstream400,
	} {
		t.Run(string(b), func(t *testing.T) {
			payload := newPayload([]KiroHistoryMessage{userMsg("hi"), assistantMsg("yo")})
			raw := marshalPayload(t, payload)

			got, result, err := enforceKiroPayloadSizeWithConfig(payload, raw,
				KiroPayloadGuardConfig{MaxWeight: 1_000_000, Behavior: b})

			require.NoError(t, err)
			require.Equal(t, raw, got)
			require.False(t, result.Trimmed || result.Compressed || result.Rejected || result.StillOversized)
		})
	}
}

// 行为的环境变量解析：非法值必须回落到默认（compress_then_trim），
// 绝不能因为配置写错就静默变成 reject 而拒服务。
func TestKiroOversizeBehaviorFromEnv(t *testing.T) {
	require.Equal(t, KiroOversizeCompressThenTrim, kiroOversizeBehaviorFromEnv())

	t.Setenv("KIRO_OVERSIZE_BEHAVIOR", "reject")
	require.Equal(t, KiroOversizeReject, kiroOversizeBehaviorFromEnv())

	t.Setenv("KIRO_OVERSIZE_BEHAVIOR", "ON_UPSTREAM_400")
	require.Equal(t, KiroOversizeOnUpstream400, kiroOversizeBehaviorFromEnv(), "大小写不敏感")

	t.Setenv("KIRO_OVERSIZE_BEHAVIOR", "garbage")
	require.Equal(t, KiroOversizeCompressThenTrim, kiroOversizeBehaviorFromEnv(),
		"非法值必须回落到默认，不能拒服务")
}
