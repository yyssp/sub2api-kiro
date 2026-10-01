package kiro

import (
	"encoding/json"
)

// PayloadDiagnostics 是发往上游的 payload 的结构计数，用于事后定位上游 400。
//
// 上游 400 只回一句"格式不对"，响应体里没有位置信息；只记响应摘要无法还原
// 是哪种请求结构触发的。这里只统计形状，不含任何正文、工具输入或凭证。
type PayloadDiagnostics struct {
	PayloadBytes          int  `json:"payload_bytes"`
	PayloadWeight         int  `json:"payload_weight"`
	HistoryMessages       int  `json:"history_messages"`
	HistoryUserMessages   int  `json:"history_user_messages"`
	HistoryAssistantMsgs  int  `json:"history_assistant_messages"`
	HistoryToolUses       int  `json:"history_tool_uses"`
	HistoryToolResults    int  `json:"history_tool_results"`
	CurrentToolResults    int  `json:"current_tool_results"`
	CurrentImages         int  `json:"current_images"`
	HistoryImages         int  `json:"history_images"`
	Tools                 int  `json:"tools"`
	MaxToolSchemaDepth    int  `json:"max_tool_schema_depth"`
	EmptyToolDescriptions int  `json:"empty_tool_descriptions"`
	ConsecutiveSameRole   int  `json:"consecutive_same_role"`
	FirstHistoryIsUser    bool `json:"first_history_is_user"`
	// UnpairedToolResults 统计"toolResult 数超过上一轮 toolUse 数"的位置个数，
	// 正是 Kiro 最常见的那条 400。
	UnpairedToolResults int  `json:"unpaired_tool_results"`
	HasProfileArn       bool `json:"has_profile_arn"`
	HasAdditionalFields bool `json:"has_additional_fields"`
}

// DiagnosePayload 解析出站 payload 并统计结构；解析失败时只填体积字段。
func DiagnosePayload(payloadBytes []byte) PayloadDiagnostics {
	d := PayloadDiagnostics{PayloadBytes: len(payloadBytes), PayloadWeight: kiroPayloadWeight(payloadBytes)}
	var payload KiroPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return d
	}
	d.HasProfileArn = payload.ProfileArn != ""
	d.HasAdditionalFields = len(payload.AdditionalModelRequestFields) > 0

	state := payload.ConversationState
	d.HistoryMessages = len(state.History)
	prevRole := ""
	prevToolUses := 0
	for i, h := range state.History {
		role := ""
		switch {
		case h.UserInputMessage != nil:
			role = "user"
			d.HistoryUserMessages++
			d.HistoryImages += len(h.UserInputMessage.Images)
			if ctx := h.UserInputMessage.UserInputMessageContext; ctx != nil {
				d.HistoryToolResults += len(ctx.ToolResults)
				if len(ctx.ToolResults) > prevToolUses {
					d.UnpairedToolResults++
				}
			}
			prevToolUses = 0
		case h.AssistantResponseMessage != nil:
			role = "assistant"
			d.HistoryAssistantMsgs++
			d.HistoryToolUses += len(h.AssistantResponseMessage.ToolUses)
			prevToolUses = len(h.AssistantResponseMessage.ToolUses)
		}
		if i == 0 {
			d.FirstHistoryIsUser = role == "user"
		}
		if role != "" && role == prevRole {
			d.ConsecutiveSameRole++
		}
		prevRole = role
	}

	current := state.CurrentMessage.UserInputMessage
	d.CurrentImages = len(current.Images)
	if prevRole == "user" {
		d.ConsecutiveSameRole++
	}
	if ctx := current.UserInputMessageContext; ctx != nil {
		d.CurrentToolResults = len(ctx.ToolResults)
		if len(ctx.ToolResults) > prevToolUses {
			d.UnpairedToolResults++
		}
		d.Tools = len(ctx.Tools)
		for _, tool := range ctx.Tools {
			if tool.ToolSpecification.Description == "" {
				d.EmptyToolDescriptions++
			}
			if depth := jsonDepth(tool.ToolSpecification.InputSchema.JSON, 0); depth > d.MaxToolSchemaDepth {
				d.MaxToolSchemaDepth = depth
			}
		}
	}
	return d
}

func jsonDepth(value any, depth int) int {
	maxDepth := depth
	switch v := value.(type) {
	case map[string]any:
		for _, child := range v {
			if d := jsonDepth(child, depth+1); d > maxDepth {
				maxDepth = d
			}
		}
	case []any:
		for _, child := range v {
			if d := jsonDepth(child, depth+1); d > maxDepth {
				maxDepth = d
			}
		}
	}
	return maxDepth
}
