package kiro

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

// KiroOversizeBehavior 决定负载超过阈值时怎么办。
type KiroOversizeBehavior string

const (
	// KiroOversizeCompressThenTrim 预检超限 -> 先压缩, 压不下去再裁剪, 然后发送。默认。
	KiroOversizeCompressThenTrim KiroOversizeBehavior = "compress_then_trim"
	// KiroOversizeReject 预检超限 -> 直接拒绝, 不发上游。
	KiroOversizeReject KiroOversizeBehavior = "reject"
	// KiroOversizeOnUpstream400 不预检, 原样发; 上游回体积超限 400 后再压缩+裁剪重试。
	KiroOversizeOnUpstream400 KiroOversizeBehavior = "on_upstream_400"
)

// kiroDefaultMaxPayloadWeight 是实测得到的上游阈值(加权口径), 见下方 kiroPayloadWeight。
//
// 2026-09-14 真实上游实测(KIRO FREE, claude-sonnet-4-5):
//
//	内容            加权值       结果
//	中文 165,000 字  1,320,000   200
//	中文 170,000 字  1,360,000   400  <- 边界在这两点之间
//	ASCII 1,280,000  1,280,000   200
//	ASCII 1,361,920  1,361,920   400
//	混合 8*80k+600k  1,240,000   200  (独立验证, 预测 200 命中)
//	混合 8*100k+600k 1,400,000   400  (独立验证, 预测 400 命中)
//
// 阈值可行区间 (1,320,000, 1,360,000]。取 1,300,000 留约 2% 余量。
//
// ⚠️ 注意: 上游限的**不是字节数**。同样是我们真正发出去的负载,
// 中文 681,990 字节就 400, 而 ASCII 1,408,286 字节仍然 200。
// 所以绝不能用 len(payloadBytes) 当判据 —— 那会对中文太松、对英文太紧。
const kiroDefaultMaxPayloadWeight = 1_300_000

// DefaultMaxPayloadWeight 导出默认阈值, 供设置层做兜底与页面默认值展示。
// 必须与 kiroDefaultMaxPayloadWeight 保持同源, 避免两处各写一个数字而漂移。
const DefaultMaxPayloadWeight = kiroDefaultMaxPayloadWeight

// kiroNonASCIIWeight 是非 ASCII 字符的计价倍数。
//
// 由实测反解得到: 设权重 w, 需同时满足
//
//	w*165000 <= 阈值 < w*170000  且  1,280,000 <= 阈值 < 1,361,920
//
// 枚举 w=2..14, 只有 w=8 有解(w=7 的区间为空)。随后用「混合内容」独立验证,
// 两个预测点全部命中 —— 不是对边界点的过拟合。
const kiroNonASCIIWeight = 8

// kiroPayloadWeight 按上游的真实计价口径估算负载"大小"。
//
// ASCII 字符计 1, 非 ASCII 计 kiroNonASCIIWeight。这是黑盒反解的经验模型,
// 不是官方口径 —— 但它同时解释了纯中文/纯英文/混合三类实测数据。
func kiroPayloadWeight(payloadBytes []byte) int {
	weight := 0
	for i := 0; i < len(payloadBytes); {
		c := payloadBytes[i]
		if c < utf8.RuneSelf {
			weight++
			i++
			continue
		}
		_, size := utf8.DecodeRune(payloadBytes[i:])
		if size <= 0 {
			size = 1
		}
		weight += kiroNonASCIIWeight
		i += size
	}
	return weight
}

// kiroPayloadSizeLimit 返回加权阈值, 允许环境变量覆盖。
// 阈值来自实测而非官方文档, 必须可调。
func kiroPayloadSizeLimit() int {
	if raw := os.Getenv("KIRO_MAX_PAYLOAD_WEIGHT"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return kiroDefaultMaxPayloadWeight
}

// kiroOversizeBehaviorFromEnv 读取默认行为, 供未显式传配置的调用方使用。
func kiroOversizeBehaviorFromEnv() KiroOversizeBehavior {
	switch strings.TrimSpace(strings.ToLower(os.Getenv("KIRO_OVERSIZE_BEHAVIOR"))) {
	case string(KiroOversizeReject):
		return KiroOversizeReject
	case string(KiroOversizeOnUpstream400):
		return KiroOversizeOnUpstream400
	default:
		return KiroOversizeCompressThenTrim
	}
}

// KiroPayloadGuardConfig 是体积守卫的可配置项。
type KiroPayloadGuardConfig struct {
	// MaxWeight 为 0 时回落到 kiroPayloadSizeLimit()。
	MaxWeight int
	// Behavior 为空时回落到环境变量 / 默认值。
	Behavior KiroOversizeBehavior
	// DisableCompression 仅用于测试与排查: 跳过压缩阶段直接裁剪。
	DisableCompression bool
}

func (c KiroPayloadGuardConfig) resolved() (int, KiroOversizeBehavior) {
	limit := c.MaxWeight
	if limit <= 0 {
		limit = kiroPayloadSizeLimit()
	}
	behavior := c.Behavior
	if behavior == "" {
		behavior = kiroOversizeBehaviorFromEnv()
	}
	return limit, behavior
}

// kiroPayloadTrimResult 记录守卫做过的事，供调用方写诊断日志 / 回响应头。
type kiroPayloadTrimResult struct {
	Trimmed        bool
	StillOversized bool
	// Compressed 表示压缩阶段真的削减了体积(哪怕最终还需要裁剪)。
	Compressed bool
	// Rejected 表示按 reject 行为拒绝了请求。
	Rejected bool
	// DeferredToUpstream 表示 on_upstream_400 行为下**故意**不做预检缩减, 原样发出。
	//
	// 必须与 StillOversized 分开: 后者的语义是"压缩+裁剪都跑到底了仍塞不下"(软失败,
	// 会打 kiro.payload_still_oversized 警告)。而本字段代表守卫一个字节都没动 ——
	// 两者混用会让每个超阈值请求都误报成"裁剪到底仍超限", 并让本该静默的响应
	// 平白带上一组数值全等的裁剪头。
	DeferredToUpstream bool
	// OriginalBytes/FinalBytes 保留字节口径, 仅用于日志可读性。
	OriginalBytes int
	FinalBytes    int
	// OriginalWeight/FinalWeight 才是与 LimitWeight 同口径的判据。
	OriginalWeight int
	FinalWeight    int
	LimitWeight    int
	DroppedItems   int
	// CompressedItems 统计被压缩(截断/剥离)的条目数。
	CompressedItems int
	// Stages 按顺序记录生效过的压缩阶段名, 便于排查"到底省在哪"。
	Stages []string
}

// ErrKiroPayloadTooLarge 在 behavior=reject 且预检超限时返回。
type ErrKiroPayloadTooLarge struct {
	Weight int
	Limit  int
}

func (e *ErrKiroPayloadTooLarge) Error() string {
	return fmt.Sprintf("kiro payload too large: weight=%d limit=%d", e.Weight, e.Limit)
}

// enforceKiroPayloadSize 把超限负载压回阈值内。
//
// 阶段顺序(参考 2ue_kiro.rs 的 payload_guard, 但按本仓库数据结构重写):
//
//  1. 压缩历史 toolResults      —— 工具输出通常是最大且最冗余的部分
//  2. 剥离历史 assistant 思维链  —— <thinking> 块对后续轮次无信息价值
//  3. 压缩工具定义描述          —— 大量 MCP 工具时 description 可达数十 KB
//  4. 丢弃历史图片              —— 单张 base64 图片可达数百 KB
//  5. 裁剪整轮历史              —— 最后手段, 会真正丢上下文
//
// 前四步是"压缩"(保留语义骨架, 标注已截断), 只有第 5 步才丢整轮对话。
// 这正是用户要求的: 除非压缩都压不下去了, 才进行裁剪。
//
// 三条不可妥协的规则(沿用原实现):
//  1. 切点必须是**不带 toolResults 的 User 消息** —— 从 tool_use/toolResult 对中间
//     切开会制造孤儿, 上游同样 400, 等于用一种 400 换另一种 400。
//  2. 找不到干净切点时**宁可不裁** —— 见 (1)。
//  3. 裁完仍超限只记录 StillOversized 放行(软失败), 由上游裁决。
func enforceKiroPayloadSize(payload *KiroPayload, payloadBytes []byte) ([]byte, kiroPayloadTrimResult, error) {
	return enforceKiroPayloadSizeWithConfig(payload, payloadBytes, KiroPayloadGuardConfig{})
}

func enforceKiroPayloadSizeWithConfig(payload *KiroPayload, payloadBytes []byte, cfg KiroPayloadGuardConfig) ([]byte, kiroPayloadTrimResult, error) {
	limit, behavior := cfg.resolved()
	weight := kiroPayloadWeight(payloadBytes)
	result := kiroPayloadTrimResult{
		OriginalBytes:  len(payloadBytes),
		FinalBytes:     len(payloadBytes),
		OriginalWeight: weight,
		FinalWeight:    weight,
		LimitWeight:    limit,
	}
	if weight <= limit {
		return payloadBytes, result, nil
	}

	// on_upstream_400: 预检不做任何事, 原样发。上游真的回体积 400 时,
	// 调用方会带 forceShrink 再调一次本函数。
	if behavior == KiroOversizeOnUpstream400 && !cfg.DisableCompression {
		result.DeferredToUpstream = true
		return payloadBytes, result, nil
	}
	if behavior == KiroOversizeReject {
		result.Rejected = true
		result.StillOversized = true
		return payloadBytes, result, &ErrKiroPayloadTooLarge{Weight: weight, Limit: limit}
	}

	reserialize := func() error {
		next, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		payloadBytes = next
		result.FinalBytes = len(next)
		result.FinalWeight = kiroPayloadWeight(next)
		return nil
	}

	// ---- 压缩阶段: 保留语义骨架, 不丢整轮对话 ----
	if !cfg.DisableCompression {
		stages := []struct {
			name string
			run  func(*KiroPayload) int
		}{
			{"history_tool_results", compressKiroHistoryToolResults},
			{"history_thinking", stripKiroHistoryThinking},
			{"tool_definitions", compressKiroToolDefinitions},
			{"history_images", dropKiroHistoryImages},
		}
		for _, stage := range stages {
			if result.FinalWeight <= limit {
				break
			}
			n := stage.run(payload)
			if n == 0 {
				continue
			}
			if err := reserialize(); err != nil {
				return nil, result, err
			}
			result.Compressed = true
			result.CompressedItems += n
			result.Stages = append(result.Stages, stage.name)
		}
	}

	if result.FinalWeight <= limit {
		return payloadBytes, result, nil
	}

	// ---- 最后手段: 裁剪整轮历史 ----
	history := payload.ConversationState.History
	// 必须在任何裁剪之前采集: 被裁掉的 toolUse 事后无从查名。
	toolUseNames := collectToolUseNames(history)
	for result.FinalWeight > limit {
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
		// align 可能丢掉开头带 toolUse 的 Assistant，使紧随其后的
		// user(toolResult) 失去配对，因此必须在 align **之后**再清一次。
		removeOrphanedToolResults(history)

		// 关键: 当前轮的 toolResults 是在 processMessages 阶段就依据
		// **未裁剪**的 history 校验过的(translator.go 的 validateToolPairing),
		// 裁剪把配对的 toolUse 切走后没有任何环节会重新校验它们。
		// 结果是上游稳定回:
		//   The number of toolResult blocks at messages.N.content
		//   exceeds the number of toolUse blocks of previous turn.
		// 2026-09-14 真实上游实测(scenario C, 7 轮 + MCP): 每次
		// kiro.payload_trimmed 后一秒内必然收到该 400, 且位置恒为
		// messages.4.content —— 正是当前轮的位置。
		payload.ConversationState.History = history
		removeOrphanedCurrentToolResults(&payload.ConversationState, history, toolUseNames)
		// 补回 toolUse 时会往 History 末尾追加消息，必须同步回本地变量，
		// 否则下一轮循环会用旧切片覆盖掉刚补的那条。
		history = payload.ConversationState.History

		result.DroppedItems += cut

		if err := reserialize(); err != nil {
			return nil, result, err
		}
		result.Trimmed = true
	}

	payload.ConversationState.History = history
	result.StillOversized = result.FinalWeight > limit
	if result.Trimmed {
		result.Stages = append(result.Stages, "history_trim")
	}
	return payloadBytes, result, nil
}

// kiroCompressedToolResultChars 是历史工具输出保留的字符预算。
// 头尾各留一半: 工具输出的关键信息通常在开头(状态/摘要)和结尾(结论/错误)。
//
// ⚠️ 与上游的 compactKiroToolResultText 是两道独立的闸门, 别弄混:
// 翻译阶段已经把**单条** >12000 字符的工具输出压到约 6000 字符(头 4000 + 尾 2000),
// 所以本阶段对单条超大输出通常是空转。本阶段真正解决的是**累积**问题 ——
// 几十轮工具调用每条都刚好卡在 6000 字符以下, 合计仍可达数百 KB。
// 预算必须显著低于 6000, 否则这一阶段在实际负载上一条都压不动。
const kiroCompressedToolResultChars = 2000

// compressKiroHistoryToolResults 截断历史里过长的工具输出。
// 只动 history, 不动当前轮 —— 当前轮的工具结果是模型正在推理的依据。
func compressKiroHistoryToolResults(payload *KiroPayload) int {
	changed := 0
	history := payload.ConversationState.History
	for i := range history {
		msg := history[i].UserInputMessage
		if msg == nil || msg.UserInputMessageContext == nil {
			continue
		}
		for j := range msg.UserInputMessageContext.ToolResults {
			tr := &msg.UserInputMessageContext.ToolResults[j]
			for k := range tr.Content {
				text := tr.Content[k].Text
				if utf8.RuneCountInString(text) <= kiroCompressedToolResultChars {
					continue
				}
				compressed := truncateHeadTail(text, kiroCompressedToolResultChars)
				if compressed == text {
					// 已经压过了(见 truncateHeadTail 的幂等判别), 不重复计数。
					continue
				}
				tr.Content[k].Text = compressed
				changed++
			}
		}
	}
	return changed
}

// stripKiroHistoryThinking 剥离历史 assistant 消息里的 <thinking> 块。
// 思维链只对产生它的那一轮有意义, 对后续轮次是纯粹的体积负担。
func stripKiroHistoryThinking(payload *KiroPayload) int {
	changed := 0
	history := payload.ConversationState.History
	for i := range history {
		msg := history[i].AssistantResponseMessage
		if msg == nil || msg.Content == "" {
			continue
		}
		stripped := removeTaggedBlocks(msg.Content, "thinking")
		if stripped != msg.Content {
			msg.Content = stripped
			changed++
		}
	}
	return changed
}

// kiroCompressedToolDescChars 是单个工具 description 的字符预算。
const kiroCompressedToolDescChars = 512

// kiroToolDescEllipsis 标记 description 已被截断, 同时用于幂等判别。
const kiroToolDescEllipsis = " […]"

// compressKiroToolDefinitions 截断过长的工具描述。
// 挂了几十个 MCP 工具时, description 合计可达数十 KB, 且每轮都要重发。
func compressKiroToolDefinitions(payload *KiroPayload) int {
	changed := 0
	compress := func(ctx *KiroUserInputMessageContext) {
		if ctx == nil {
			return
		}
		for i := range ctx.Tools {
			spec := &ctx.Tools[i].ToolSpecification
			if utf8.RuneCountInString(spec.Description) <= kiroCompressedToolDescChars {
				continue
			}
			// 同 truncateHeadTail: 截断结果带后缀, 仍 > budget, 必须判别避免重复截断。
			if strings.HasSuffix(spec.Description, kiroToolDescEllipsis) {
				continue
			}
			spec.Description = truncateUTF8(spec.Description, kiroCompressedToolDescChars) + kiroToolDescEllipsis
			changed++
		}
	}
	compress(payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext)
	for i := range payload.ConversationState.History {
		if msg := payload.ConversationState.History[i].UserInputMessage; msg != nil {
			compress(msg.UserInputMessageContext)
		}
	}
	return changed
}

// kiroDroppedImagePlaceholder 替换被丢弃的历史图片, 让模型知道这里原本有图。
const kiroDroppedImagePlaceholder = "\n[历史图片已因请求体积超限被省略]"

// dropKiroHistoryImages 丢弃历史消息里的图片(保留当前轮的图)。
// 单张 base64 图片动辄数百 KB, 是压缩收益最高的一步, 但也最有损 —— 故排在最后。
func dropKiroHistoryImages(payload *KiroPayload) int {
	changed := 0
	history := payload.ConversationState.History
	for i := range history {
		msg := history[i].UserInputMessage
		if msg == nil || len(msg.Images) == 0 {
			continue
		}
		changed += len(msg.Images)
		msg.Images = nil
		if !strings.Contains(msg.Content, kiroDroppedImagePlaceholder) {
			msg.Content += kiroDroppedImagePlaceholder
		}
	}
	return changed
}

// kiroTruncationMarker 是截断标记的识别前缀。
//
// 必须能被识别: 截断结果本身长度是 budget + 标记长度, 仍然 > budget。
// 若不加判别, 第二次压缩(on_upstream_400 重试路径会再跑一遍)会把已截断的
// 内容再截一次 —— 既重复计数, 又会把上一次的标记切碎成乱码。
const kiroTruncationMarker = "[…代理已截断"

// truncateHeadTail 保留首尾各一半预算, 中间用标记替代。
// 对已含截断标记的文本是幂等的。
func truncateHeadTail(text string, budget int) string {
	total := utf8.RuneCountInString(text)
	if total <= budget || budget <= 0 {
		return text
	}
	if strings.Contains(text, kiroTruncationMarker) {
		return text
	}
	half := budget / 2
	runes := []rune(text)
	head := string(runes[:half])
	tail := string(runes[total-half:])
	return fmt.Sprintf("%s\n%s %d 字符…]\n%s", head, kiroTruncationMarker, total-budget, tail)
}

// removeTaggedBlocks 移除 <tag>…</tag> 包裹的内容(含标签本身)。
func removeTaggedBlocks(text, tag string) string {
	open, close := "<"+tag+">", "</"+tag+">"
	for {
		start := strings.Index(text, open)
		if start < 0 {
			return text
		}
		end := strings.Index(text[start:], close)
		if end < 0 {
			return text
		}
		text = text[:start] + text[start+end+len(close):]
	}
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
