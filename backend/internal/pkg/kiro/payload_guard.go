package kiro

import (
	"encoding/json"
	"os"
	"strconv"
)

// kiroDefaultMaxPayloadBytes 是请求体的保守上限。
//
// 三个独立实现给出的阈值分歧明显：
//   - 2ue_kiro.rs   450KiB 上限 + 32KiB 余量（实际目标约 418KiB）
//   - AbdoKnbGit/tau 600KB 硬上限 / 220KB 软目标
//   - 社区观测      ~615KB
//
// 取最保守的 450KiB：裁多了只是浪费上下文，裁少了会整个请求 400。安全侧优先。
const kiroDefaultMaxPayloadBytes = 450 * 1024

// kiroPayloadSizeLimit 允许用环境变量覆盖，便于实测后调整而不必改代码。
// 阈值本身是社区经验值而非官方文档值，必须可调。
func kiroPayloadSizeLimit() int {
	if raw := os.Getenv("KIRO_MAX_PAYLOAD_BYTES"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return kiroDefaultMaxPayloadBytes
}

// kiroPayloadTrimResult 记录裁剪过程，供调用方写诊断日志。
type kiroPayloadTrimResult struct {
	Trimmed        bool
	StillOversized bool
	OriginalBytes  int
	FinalBytes     int
	LimitBytes     int
	DroppedItems   int
}

// enforceKiroPayloadSize 在序列化后按「轮次粒度」裁剪历史，直到请求体落回阈值内。
//
// 三条不可妥协的规则：
//  1. 切点必须是**不带 toolResults 的 User 消息** —— 从 tool_use/toolResult 对中间
//     切开会制造孤儿，上游同样 400，等于用一种 400 换另一种 400。
//  2. 找不到干净切点时**宁可不裁** —— 见 (1)。
//  3. 裁完仍超限只记录 StillOversized 放行（软失败），由上游裁决 ——
//     我们的阈值是社区经验值，不该比上游更严格地拒绝用户请求。
func enforceKiroPayloadSize(payload *KiroPayload, payloadBytes []byte) ([]byte, kiroPayloadTrimResult, error) {
	limit := kiroPayloadSizeLimit()
	result := kiroPayloadTrimResult{
		OriginalBytes: len(payloadBytes),
		FinalBytes:    len(payloadBytes),
		LimitBytes:    limit,
	}
	if len(payloadBytes) <= limit {
		return payloadBytes, result, nil
	}

	history := payload.ConversationState.History
	for len(payloadBytes) > limit {
		cut := nextKiroHistoryCutPoint(history)
		if cut <= 0 {
			// 没有干净切点：宁可不裁。
			break
		}

		history = history[cut:]
		// 裁剪打断了 tool 配对，必须复用既有修复逻辑重新收敛，
		// 否则留下的孤儿 toolUse 会直接触发 400。
		_, orphaned := validateToolPairing(history, nil)
		removeOrphanedToolUses(history, orphaned)
		history = alignKiroHistoryToUser(history)

		result.DroppedItems += cut
		payload.ConversationState.History = history

		next, err := json.Marshal(payload)
		if err != nil {
			return nil, result, err
		}
		payloadBytes = next
		result.Trimmed = true
		result.FinalBytes = len(payloadBytes)
	}

	payload.ConversationState.History = history
	result.FinalBytes = len(payloadBytes)
	result.StillOversized = len(payloadBytes) > limit
	return payloadBytes, result, nil
}

// nextKiroHistoryCutPoint 返回可以安全丢弃的前缀长度。
// 干净切点 = 一条「没有 toolResults」的 User 消息，从它开始保留。
// 返回 0 表示没有可用切点（调用方应放弃裁剪）。
func nextKiroHistoryCutPoint(history []KiroHistoryMessage) int {
	// 从 1 开始：切点为 0 等于什么都没裁，会让调用方死循环。
	for i := 1; i < len(history); i++ {
		msg := history[i].UserInputMessage
		if msg == nil {
			continue
		}
		if msg.UserInputMessageContext != nil && len(msg.UserInputMessageContext.ToolResults) > 0 {
			// 这条 User 消息在回应上一条 Assistant 的 toolUse，
			// 从这里切会把配对拆散。
			continue
		}
		return i
	}
	return 0
}

// alignKiroHistoryToUser 丢弃开头的 Assistant 消息，保证 history 以 User 开头。
// 上游要求 role 严格交替且以 user 起始。
func alignKiroHistoryToUser(history []KiroHistoryMessage) []KiroHistoryMessage {
	for len(history) > 0 && history[0].UserInputMessage == nil {
		history = history[1:]
	}
	return history
}
