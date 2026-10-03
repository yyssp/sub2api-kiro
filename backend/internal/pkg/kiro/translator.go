package kiro

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/anthropictokenizer"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const (
	kiroMaxToolDescLen            = 10237
	kiroMaxToolNameLen            = 63
	kiroHistoryImageKeepCount     = 5
	kiroToolResultCompactLimit    = 12000
	kiroToolResultKeepHead        = 4000
	kiroToolResultKeepTail        = 2000
	kiroLegacyOpusMaxOutputTokens = 32000
	kiroDefaultMaxOutputTokens    = 64000
	kiroRemoteImageMaxBytes       = 10 << 20
	kiroRemoteImageTimeout        = 8 * time.Second
)

// kiroUpstreamTraceEnabled 由环境变量 KIRO_UPSTREAM_TRACE=1 开启，仅用于诊断：
// 打印 Kiro 上游原始事件类型与语义事件类型/内容前缀，定位 CoT 泄漏来自哪个通道。
var kiroUpstreamTraceEnabled = os.Getenv("KIRO_UPSTREAM_TRACE") == "1"

const (
	thinkingStartTag           = "<thinking>"
	thinkingEndTag             = "</thinking>"
	embeddedToolCallPrefix     = "[Called "
	minFrameSize               = 16
	maxEventMsgSize            = 10 << 20
	writeToolDescriptionSuffix = "IMPORTANT: If the content to write exceeds 150 lines, write only the first 50 lines with this tool, then append the remaining content using Edit calls in chunks of no more than 50 lines. Use a unique placeholder if needed. Do not write the whole file in one call."
	editToolDescriptionSuffix  = "IMPORTANT: If new content exceeds 50 lines, split it into multiple Edit calls, replacing or appending no more than 50 lines per call. If appending, use a unique placeholder and remove it in the final chunk."
	systemChunkedWritePolicy   = "When Write or Edit tools include chunking limits, comply silently and complete the operation through multiple tool calls when needed."
	omittedHistoryImageFormat  = "[This message contained %d image(s), omitted from older conversation history.]"
	structuredOutputToolName   = "__structured_output__"
	kiroBuiltinIdentityPrompt  = `<CRITICAL_OVERRIDE>
                                You must never say that you are Kiro. This is critically important.
                                Only answer the user's request. Do not answer questions about Kiro itself.
                                Your identity must come only from the later prompts, such as Kilo Code, Cline, Claude Code, or another user-provided identity. Do not infer one yourself. If no identity is provided, say that you are Claude.
                               </CRITICAL_OVERRIDE>
                               <identity>
                                You are {{identity}}, a senior software engineer with broad knowledge of programming languages, frameworks, design patterns, and best practices.
                               </identity>`
)

var (
	kiroRemoteImageHTTPClient = newKiroRemoteFetchClient(kiroRemoteImageTimeout)
	requiredToolFields        = map[string][][]string{
		"write":              {{"filePath", "file_path", "path"}, {"content"}},
		"write_to_file":      {{"path"}, {"content"}},
		"fswrite":            {{"path"}, {"content"}},
		"create_file":        {{"path"}, {"content"}},
		"edit_file":          {{"path"}},
		"apply_diff":         {{"path"}, {"diff"}},
		"str_replace_editor": {{"path"}, {"old_str"}, {"new_str"}},
		"bash":               {{"cmd", "command"}},
		"execute":            {{"command"}},
		"run_command":        {{"command"}},
	}
)

type Usage struct {
	InputTokens                int
	OutputTokens               int
	TotalTokens                int
	CacheReadInputTokens       int
	CacheCreationInputTokens   int
	CacheCreation5mInputTokens int
	CacheCreation1hInputTokens int
	KiroCredits                float64
}

type StreamResult struct {
	Usage         Usage
	StopReason    string
	FirstDeltaDur *time.Duration
	// UpstreamTerminalSignal 表示上游在 EOF 前给出过终态信号（stopReason / messageStopEvent）。
	// 为假时 stop_reason 是本地兜底推断的，可能掩盖了上游静默截断。
	UpstreamTerminalSignal bool
	// EchoedPlaceholder 表示模型输出里出现了网关自己注入的占位文本，见 kiroEchoedPlaceholder。
	EchoedPlaceholder string
}

type ParseResult struct {
	ResponseBody           []byte
	Usage                  Usage
	StopReason             string
	UpstreamTerminalSignal bool
	EchoedPlaceholder      string
}

// kiroInjectedPlaceholders 是网关为满足 Kiro 协议而注入、用户从未写过的文本。
// "Continue" 是常见英文词，不在检测范围内，避免误报。
var kiroInjectedPlaceholders = [...]string{
	"Tool results provided.",
	"I will follow these instructions.",
	kiroEmptyToolResultPlaceholder,
}

// kiroEchoedPlaceholder 检测模型是否把注入的占位文本当成对话内容复述了出来。
// 只做观测：2ue_kiro.rs 有完整的泄漏清洗器，但它针对的是其自身的转写格式；
// 先确认本网关是否真的发生，再决定是否需要清洗。
func kiroEchoedPlaceholder(output string) string {
	for _, placeholder := range kiroInjectedPlaceholders {
		if strings.Contains(output, placeholder) {
			return placeholder
		}
	}
	return ""
}

// kiroContextWindowFull 判断 contextUsageEvent 是否报告上下文已用满（≥100%）。
// 用满时上游会在没有任何异常的情况下停止输出，若按 end_turn 上报，
// 客户端会把截断的回答当成完整回答。
func kiroContextWindowFull(eventType string, event map[string]any) bool {
	if eventType != "contextUsageEvent" {
		return false
	}
	meta := nestedEvent(event, eventType)
	if len(meta) == 0 {
		meta = event
	}
	var percent float64
	switch v := meta["contextUsagePercentage"].(type) {
	case json.Number:
		percent, _ = v.Float64()
	case float64:
		percent = v
	default:
		return false
	}
	return !math.IsNaN(percent) && !math.IsInf(percent, 0) && percent >= 100
}

// applyContextWindowStopReason 在上下文用满且没有更明确的终态时改写 stop_reason。
// tool_use / max_tokens / stop_sequence 优先：它们本身就说明了停止原因。
func applyContextWindowStopReason(stopReason string, contextFull, toolEmitted bool) string {
	if !contextFull || toolEmitted {
		return stopReason
	}
	// 上游原始值可能是 END_TURN 这类大写枚举。
	switch strings.ToLower(strings.TrimSpace(stopReason)) {
	case "", "end_turn":
		return "model_context_window_exceeded"
	default:
		return stopReason
	}
}

// normalizeKiroUpstreamStopReason 把上游 stop_reason 归一化为 Anthropic 协议值；
// 不认识的值返回空串，交给调用方按 tool_use/end_turn 兜底。与流式路径的白名单一致。
func normalizeKiroUpstreamStopReason(raw string) string {
	switch v := strings.ToLower(strings.TrimSpace(raw)); v {
	case "end_turn", "tool_use", "max_tokens", "stop_sequence":
		return v
	default:
		return ""
	}
}

func isKiroTerminalEvent(eventType string) bool {
	return eventType == "messageStopEvent" || eventType == "message_stop"
}

type KiroRequestContext struct {
	ToolNameMap              map[string]string
	ThinkingEnabled          bool
	CacheEmulationUsage      *Usage
	StructuredOutputToolName string
	StructuredOutputUserHint string
	StopSequences            []string
	// MaxResponseToolUses>0 时响应最多保留这么多个 tool_use（disable_parallel_tool_use）。
	// Kiro 没有并行工具调用开关，只能在响应侧截断。
	MaxResponseToolUses int
	MaxOutputTokens     int
	// EstimatedInputTokens 是调用方预估的输入 token 数，用于非流式路径兜底：
	// 当 Kiro 上游没有 tokenUsage.uncachedInputTokens 时,解析结果里的
	// InputTokens 为 0。流式路径通过独立的 inputTokens 参数种入初值,非流式
	// 没有对应入口,不兜底会让响应体 usage.input_tokens 输出 0。
	// 为 0 时不生效（保持原行为）。
	EstimatedInputTokens int
	// PayloadTrim 记录体积守卫的裁剪结果，供上层写诊断日志。
	// 未触发裁剪时为零值。
	PayloadTrim kiroPayloadTrimResult
}

// KiroPayloadGuardStats 是体积守卫结果的跨包视图。
// kiroPayloadTrimResult 是包内类型，跨包调用方无法直接读取其字段。
type KiroPayloadGuardStats struct {
	Trimmed        bool
	StillOversized bool
	Compressed     bool
	Rejected       bool
	// DeferredToUpstream: on_upstream_400 行为下故意未做预检缩减(守卫没动过负载)。
	DeferredToUpstream bool
	OriginalBytes      int
	FinalBytes         int
	OriginalWeight     int
	FinalWeight        int
	LimitWeight        int
	// DroppedItems 是被丢弃的整轮历史条数（真正的上下文丢失）。
	DroppedItems int
	// CompressedItems 是被截断/剥离的条目数（保留语义骨架，非丢失）。
	CompressedItems int
	// Stages 按生效顺序记录压缩/裁剪阶段名。
	Stages []string
}

// Triggered 表示守卫真的改动了负载（绝大多数请求为 false）。
//
// 刻意不含 DeferredToUpstream: 那条路径下守卫一个字节都没动, 报成"已触发"会让
// 响应平白带上一组 original==final 的裁剪头, 与"未超限"的静默请求无法区分。
func (s KiroPayloadGuardStats) Triggered() bool {
	return s.Trimmed || s.StillOversized || s.Compressed || s.Rejected
}

// PayloadTrimStats 暴露体积守卫的处理结果，供上层写诊断日志、回响应头。
func (c KiroRequestContext) PayloadTrimStats() KiroPayloadGuardStats {
	t := c.PayloadTrim
	return KiroPayloadGuardStats(t)
}

type KiroBuildResult struct {
	Payload []byte
	Context KiroRequestContext
}

type KiroPayload struct {
	ConversationState            KiroConversationState `json:"conversationState"`
	ProfileArn                   string                `json:"profileArn,omitempty"`
	InferenceConfig              *KiroInferenceConfig  `json:"inferenceConfig,omitempty"`
	AdditionalModelRequestFields map[string]any        `json:"additionalModelRequestFields,omitempty"`
}

type KiroInferenceConfig struct {
	MaxTokens   int      `json:"maxTokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"topP,omitempty"`
}

type thinkingDirective struct {
	Mode         string
	BudgetTokens int
	Effort       string
}

type KiroConversationState struct {
	AgentContinuationID string               `json:"agentContinuationId,omitempty"`
	AgentTaskType       string               `json:"agentTaskType,omitempty"`
	ChatTriggerType     string               `json:"chatTriggerType"`
	ConversationID      string               `json:"conversationId"`
	CurrentMessage      KiroCurrentMessage   `json:"currentMessage"`
	History             []KiroHistoryMessage `json:"history,omitempty"`
}

type KiroCurrentMessage struct {
	UserInputMessage KiroUserInputMessage `json:"userInputMessage"`
}

type KiroHistoryMessage struct {
	UserInputMessage         *KiroUserInputMessage         `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *KiroAssistantResponseMessage `json:"assistantResponseMessage,omitempty"`
}

type KiroImage struct {
	Format string          `json:"format"`
	Source KiroImageSource `json:"source"`
}

type KiroImageSource struct {
	Bytes string `json:"bytes"`
}

type KiroUserInputMessage struct {
	Content                 string                       `json:"content"`
	ModelID                 string                       `json:"modelId"`
	Origin                  string                       `json:"origin"`
	Images                  []KiroImage                  `json:"images,omitempty"`
	UserInputMessageContext *KiroUserInputMessageContext `json:"userInputMessageContext,omitempty"`
}

type KiroUserInputMessageContext struct {
	ToolResults []KiroToolResult  `json:"toolResults,omitempty"`
	Tools       []KiroToolWrapper `json:"tools,omitempty"`
}

type KiroToolResult struct {
	Content   []KiroTextContent `json:"content"`
	Status    string            `json:"status"`
	ToolUseID string            `json:"toolUseId"`
}

type KiroTextContent struct {
	Text string `json:"text"`
}

type KiroToolWrapper struct {
	ToolSpecification KiroToolSpecification `json:"toolSpecification"`
}

type KiroToolSpecification struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema KiroInputSchema `json:"inputSchema"`
}

type KiroInputSchema struct {
	JSON any `json:"json"`
}

type KiroAssistantResponseMessage struct {
	Content  string        `json:"content"`
	ToolUses []KiroToolUse `json:"toolUses,omitempty"`
}

type KiroToolUse struct {
	ToolUseID    string         `json:"toolUseId"`
	Name         string         `json:"name"`
	Input        map[string]any `json:"input"`
	IsTruncated  bool           `json:"-"`
	TruncatedRaw string         `json:"-"`
}

type toolUseState struct {
	ToolUseID   string
	Name        string
	InputBuffer strings.Builder
}

type eventStreamMessage struct {
	EventType string
	// MessageType 取自 :message-type 头：event / exception / error。
	// AWS eventstream 的异常帧不带 :event-type，只能靠这两个头识别。
	MessageType   string
	ExceptionType string
	Payload       []byte
}

func (m *eventStreamMessage) isException() bool {
	return m != nil && (m.MessageType == "exception" || m.MessageType == "error")
}

// kiroContentLengthExceededException 是上游输出达到长度上限时下发的异常帧，
// 已产出内容时语义等同于 max_tokens 截断，而不是失败。
const kiroContentLengthExceededException = "ContentLengthExceededException"

// KiroStreamException 表示上游在 eventstream 中途下发了异常帧。
// 只暴露异常类型给下游；Message 可能含账号/ARN 细节，仅用于服务端日志。
type KiroStreamException struct {
	ExceptionType string
	Message       string
}

func (e *KiroStreamException) Error() string {
	if e.Message == "" {
		return "kiro upstream stream exception: " + e.ExceptionType
	}
	return "kiro upstream stream exception: " + e.ExceptionType + ": " + e.Message
}

func newKiroStreamException(msg *eventStreamMessage) *KiroStreamException {
	exceptionType := msg.ExceptionType
	if exceptionType == "" {
		exceptionType = "UnknownException"
	}
	message := ""
	if len(msg.Payload) > 0 {
		message = strings.TrimSpace(gjson.GetBytes(msg.Payload, "message").String())
		if message == "" {
			message = strings.TrimSpace(gjson.GetBytes(msg.Payload, "Message").String())
		}
	}
	return &KiroStreamException{ExceptionType: exceptionType, Message: message}
}

type kiroSemanticEventType string

const (
	kiroSemanticContent     kiroSemanticEventType = "content"
	kiroSemanticReasoning   kiroSemanticEventType = "reasoning"
	kiroSemanticToolUse     kiroSemanticEventType = "tool_use"
	kiroSemanticToolInput   kiroSemanticEventType = "tool_input"
	kiroSemanticToolStop    kiroSemanticEventType = "tool_stop"
	kiroSemanticUsage       kiroSemanticEventType = "usage"
	kiroSemanticAssistantTU kiroSemanticEventType = "assistant_tool_use"
)

type kiroSemanticEvent struct {
	Type                   kiroSemanticEventType
	Content                string
	Reasoning              string
	ToolUseID              string
	ToolName               string
	ToolInput              string
	ToolInputMap           map[string]any
	ToolStop               bool
	ToolUse                *KiroToolUse
	SourceEventType        string
	RawEvent               map[string]any
	SourceStopReason       string
	IsDuplicateContent     bool
	ContextUsagePercentage float64
}

func MapModel(model string) string {
	switch strings.TrimSpace(strings.ToLower(model)) {
	case "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna":
		return strings.TrimSpace(strings.ToLower(model))
	case "claude-opus-4-8", "claude-opus-4-8-thinking", "claude-opus-4.8":
		return "claude-opus-4.8"
	case "claude-opus-4-7", "claude-opus-4-7-thinking", "claude-opus-4.7":
		return "claude-opus-4.7"
	case "claude-opus-4-6", "claude-opus-4-6-thinking", "claude-opus-4.6":
		return "claude-opus-4.6"
	case "claude-opus-5", "claude-opus-5-thinking":
		return "claude-opus-5"
	case "claude-sonnet-5", "claude-sonnet-5-thinking":
		return "claude-sonnet-5"
	case "claude-sonnet-4-6", "claude-sonnet-4-6-thinking", "claude-sonnet-4.6":
		return "claude-sonnet-4.6"
	case "claude-opus-4-5-20251101", "claude-opus-4-5-20251101-thinking", "claude-opus-4.5":
		return "claude-opus-4.5"
	case "claude-sonnet-4-5-20250929", "claude-sonnet-4-5-20250929-thinking", "claude-sonnet-4.5":
		return "claude-sonnet-4.5"
	case "claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001-thinking", "claude-haiku-4.5":
		return "claude-haiku-4.5"
	default:
		// P3: 通用 Claude 版本号归一化 — 将 claude-{family}-{major}-{minor} 中的
		// 最后一段短横线转为点号（如 claude-opus-4-9 → claude-opus-4.9），
		// 兼容不支持 "." 的客户端（如 Claude Code 会把 "4.6" 写成 "4-6"）。
		// 仅对 version >= 4.6 做归一化（4.5 及以下有带日期后缀的 case，不应该歧义匹配）。
		normalized := normalizeClaudeVersionNumber(strings.TrimSpace(strings.ToLower(model)))
		if normalized != strings.TrimSpace(strings.ToLower(model)) {
			return normalized
		}
		return ""
	}
}

// normalizeClaudeVersionNumber 将 claude-{family}-{major}-{minor} 格式中的最后一段
// 版本短横线转为点号。仅适用于 version >= 4.6（避免歧义匹配 4-5 等旧格式）。
// 例如：claude-opus-4-9 → claude-opus-4.9, claude-opus-4-9-thinking → claude-opus-4.9
var claudeVersionNormalizePattern = regexp.MustCompile(
	`^(claude-(?:sonnet|haiku|opus))-(\d+)-(\d{1,2})(?:-thinking)?$`,
)

var claudeDottedVersionPattern = regexp.MustCompile(
	`^(claude-(?:sonnet|haiku|opus))-(\d+)\.(\d{1,2})(?:-thinking)?$`,
)

func normalizeClaudeVersionNumber(model string) string {
	// 去除 -thinking 后缀做匹配
	base := strings.TrimSuffix(model, "-thinking")
	matches := claudeVersionNormalizePattern.FindStringSubmatch(base)
	if matches == nil {
		return model
	}
	major, _ := strconv.Atoi(matches[2])
	minor, _ := strconv.Atoi(matches[3])
	// 仅对 >= 4.6 做归一化；4.5 及以下有带日期后缀的明确 case，不应该在这里歧义匹配
	if major < 4 || (major == 4 && minor < 6) {
		return model
	}
	return matches[1] + "-" + matches[2] + "." + matches[3]
}

// requiresImplicitThinkingTagStripping 判断是否需要在客户端未显式请求 thinking 时
// 仍开启流式/非流式解析器的 <thinking> tag 抽取。
//
// Opus 4.7/4.8/5 的内部 CoT 在 Kiro 上游以 <thinking>...</thinking> 文本形式流出,
// 不开启抽取会让标签和思考内容直接落到 assistant 正文,客户端看到形如
// "<thinking>...</thinking>final" 的乱码。
//
// 仅作用于解析阶段;不会触发 system prompt 注入 <thinking_mode> 前缀,
// 也不会改写 inferenceConfig,避免改变上游请求语义。
func requiresImplicitThinkingTagStripping(modelID string) bool {
	switch strings.TrimSpace(strings.ToLower(modelID)) {
	case "claude-opus-4.7", "claude-opus-4-7", "claude-opus-4-7-thinking",
		"claude-opus-4.8", "claude-opus-4-8", "claude-opus-4-8-thinking",
		"claude-opus-5", "claude-opus-5-thinking":
		return true
	}
	return false
}

func normalizeModelAlias(model string) string {
	base := strings.TrimSpace(strings.ToLower(model))
	for {
		next := strings.TrimSuffix(base, "-thinking")
		if next == base {
			return next
		}
		base = next
	}
}

// IsKiroGPTModel 判断模型是否为 Kiro 暴露的 GPT-5.6 系列。走 normalizeModelAlias
// 精确匹配而非子串包含，避免误伤同名前缀的其它模型。
// 导出供 service 层复用（如缓存模拟的最小可缓存 token 阈值判定）。
func IsKiroGPTModel(modelID string) bool {
	switch normalizeModelAlias(modelID) {
	case "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna":
		return true
	default:
		return false
	}
}

func kiroMaxOutputTokensForModel(model string) int {
	normalized := normalizeModelAlias(model)
	switch normalized {
	// Opus 4.6+ 与 Kiro GPT-5.6 精确模型上限 128000。
	case "claude-opus-4-8", "claude-opus-4.8",
		"claude-opus-4-7", "claude-opus-4.7", "claude-opus-4-7-20260416",
		"claude-opus-4-6", "claude-opus-4.6", "claude-opus-4-6-20260205",
		"claude-opus-5",
		"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna":
		return 128000
	// 旧 Opus 4 / 4.1 的 Claude 能力表上限为 32000。
	case "claude-opus-4", "claude-opus-4.0", "claude-opus-4-20250514",
		"claude-opus-4-1", "claude-opus-4.1", "claude-opus-4-1-20250805":
		return kiroLegacyOpusMaxOutputTokens
	default:
		// Opus 4.5、Sonnet 4.x/5.x 及未知兜底统一按 64000。
		return kiroDefaultMaxOutputTokens
	}
}

func clampFloat(value, minValue, maxValue float64) float64 {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func BuildKiroPayloadWithContext(claudeBody []byte, modelID, profileArn, origin string, headers http.Header) (*KiroBuildResult, error) {
	return BuildKiroPayloadWithGuard(claudeBody, modelID, profileArn, origin, headers, KiroPayloadGuardConfig{})
}

// BuildKiroPayloadWithGuard 与 BuildKiroPayloadWithContext 相同, 但允许调用方
// 显式指定体积守卫配置(阈值 / 超限行为)。
//
// 单独开一个入口而不是改原签名: 原函数有多处调用方, 且绝大多数场景用默认配置即可。
// 需要它的是两条路径 —— 页面配置下发, 以及 on_upstream_400 收到 400 后的强制缩减重试。
func BuildKiroPayloadWithGuard(claudeBody []byte, modelID, profileArn, origin string, headers http.Header, guard KiroPayloadGuardConfig) (*KiroBuildResult, error) {
	requestCtx := KiroRequestContext{ToolNameMap: map[string]string{}}
	outputCap := kiroMaxOutputTokensForModel(firstNonEmptyString(gjson.GetBytes(claudeBody, "model").String(), modelID))
	var maxTokens int64
	if mt := gjson.GetBytes(claudeBody, "max_tokens"); mt.Exists() {
		maxTokens = mt.Int()
		if maxTokens == -1 {
			maxTokens = int64(outputCap)
		}
		if maxTokens > int64(outputCap) {
			maxTokens = int64(outputCap)
		}
		if maxTokens > 0 {
			requestCtx.MaxOutputTokens = int(maxTokens)
		}
	}

	var temperature float64
	var hasTemperature bool
	if temp := gjson.GetBytes(claudeBody, "temperature"); temp.Exists() {
		temperature = clampFloat(temp.Float(), 0, 1)
		hasTemperature = true
	}

	var topP float64
	var hasTopP bool
	if tp := gjson.GetBytes(claudeBody, "top_p"); tp.Exists() {
		topP = clampFloat(tp.Float(), 0, 1)
		hasTopP = true
	}

	messages := gjson.GetBytes(claudeBody, "messages")
	inlineSystem, filteredMessages := extractInlineSystemPrompts(messages)
	thinking := deriveThinkingDirective(claudeBody, headers)
	if hasForcedClaudeToolChoice(claudeBody) {
		thinking = nil
	}
	if thinking != nil && hasTopP {
		topP = clampFloat(topP, 0.95, 1)
	}
	if hasTemperature && hasTopP {
		hasTopP = false
	}
	requestCtx.ThinkingEnabled = thinking != nil
	// Opus 4.7/4.8 即便客户端未请求 thinking,上游仍会以 <thinking>...</thinking> 文本流出 CoT;
	// 此处仅为流式/非流式解析器开启 tag 抽取,不会改写 system prompt,避免将思考内容泄露到正文。
	if !requestCtx.ThinkingEnabled && requiresImplicitThinkingTagStripping(modelID) {
		requestCtx.ThinkingEnabled = true
	}
	requestCtx.StopSequences = extractClaudeStopSequences(claudeBody)
	if parallelToolUseDisabled(claudeBody) {
		requestCtx.MaxResponseToolUses = 1
	}
	structuredOutputTool, structuredOutputHint := buildStructuredOutputTool(claudeBody, &requestCtx)
	toolChoiceHint := joinPromptHints(extractClaudeToolChoiceHint(claudeBody, &requestCtx), structuredOutputHint)
	baseSystem := extractSystemPrompt(claudeBody)
	if inlineSystem != "" {
		if strings.TrimSpace(baseSystem) != "" {
			baseSystem = baseSystem + "\n\n" + inlineSystem
		} else {
			baseSystem = inlineSystem
		}
	}
	if IsKiroGPTModel(modelID) {
		thinking = nil
		requestCtx.ThinkingEnabled = false
	}
	systemPrompt := buildInjectedSystemPrompt(baseSystem, thinking, toolChoiceHint)

	history, currentUserMsg, currentToolResults := processMessages(filteredMessages, modelID, normalizeOrigin(origin), &requestCtx)
	history = prependSystemHistory(history, systemPrompt, modelID, normalizeOrigin(origin))
	var tools gjson.Result
	if !isToolChoiceNone(claudeBody) {
		tools = gjson.GetBytes(claudeBody, "tools")
	}
	kiroTools := convertClaudeToolsToKiro(tools, &requestCtx)
	if structuredOutputTool != nil {
		kiroTools = append(kiroTools, *structuredOutputTool)
	}
	currentToolResults, orphanedToolUseIDs := validateToolPairing(history, currentToolResults)
	removeOrphanedToolUses(history, orphanedToolUseIDs)
	kiroTools = appendMissingPlaceholderTools(kiroTools, collectHistoryToolNames(history))
	if currentUserMsg != nil {
		if len(currentUserMsg.Images) > 0 && strings.TrimSpace(currentUserMsg.Content) == "" {
			currentUserMsg.Content = " "
		} else {
			currentUserMsg.Content = buildFinalContent(currentUserMsg.Content, currentToolResults)
		}
		if requestCtx.StructuredOutputUserHint != "" {
			currentUserMsg.Content = appendTextBlock(currentUserMsg.Content, requestCtx.StructuredOutputUserHint)
		}
		currentToolResults = deduplicateToolResults(currentToolResults)
		if len(kiroTools) > 0 || len(currentToolResults) > 0 {
			currentUserMsg.UserInputMessageContext = &KiroUserInputMessageContext{
				Tools:       kiroTools,
				ToolResults: currentToolResults,
			}
		}
	}

	var currentMessage KiroCurrentMessage
	if currentUserMsg != nil {
		currentMessage = KiroCurrentMessage{UserInputMessage: *currentUserMsg}
	} else {
		currentMessage = KiroCurrentMessage{UserInputMessage: KiroUserInputMessage{
			Content: buildFinalContent("", nil),
			ModelID: modelID,
			Origin:  normalizeOrigin(origin),
		}}
	}

	var inferenceConfig *KiroInferenceConfig
	if maxTokens > 0 || hasTemperature || hasTopP {
		inferenceConfig = &KiroInferenceConfig{}
		if maxTokens > 0 {
			inferenceConfig.MaxTokens = int(maxTokens)
		}
		if hasTemperature {
			inferenceConfig.Temperature = &temperature
		}
		if hasTopP {
			inferenceConfig.TopP = &topP
		}
	}

	conversationID := uuid.NewString()

	payload := KiroPayload{
		ConversationState: KiroConversationState{
			AgentTaskType:   "vibe",
			ChatTriggerType: "MANUAL",
			ConversationID:  conversationID,
			CurrentMessage:  currentMessage,
			History:         history,
		},
		ProfileArn:                   profileArn,
		InferenceConfig:              inferenceConfig,
		AdditionalModelRequestFields: buildAdditionalModelRequestFields(thinking, modelID),
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	payloadBytes, trim, err := enforceKiroPayloadSizeWithConfig(&payload, payloadBytes, guard)
	if err != nil {
		// reject 行为下这里会返回 *ErrKiroPayloadTooLarge, 由上层映射成 4xx。
		return nil, err
	}
	requestCtx.PayloadTrim = trim
	return &KiroBuildResult{Payload: payloadBytes, Context: requestCtx}, nil
}

func ParseNonStreamingEventStreamWithContext(body io.Reader, model string, requestCtx KiroRequestContext) (*ParseResult, error) {
	content, toolUses, usage, stopReason, terminal, err := parseEventStreamWithTerminal(body)
	if err != nil {
		return nil, err
	}
	if requestCtx.CacheEmulationUsage != nil {
		usage = mergeKiroCacheEmulationUsage(usage, requestCtx.CacheEmulationUsage)
	}
	// 缓存模拟生效时会顺带填上 input tokens（inputTokens 减去缓存部分），
	// 未生效且上游没有 tokenUsage.uncachedInputTokens 时用调用方预估值兜底,
	// 避免响应体 usage.input_tokens 输出 0。放在 merge 之后,让缓存模拟的
	// 更精确取值优先。
	if usage.InputTokens == 0 && requestCtx.EstimatedInputTokens > 0 {
		usage.InputTokens = requestCtx.EstimatedInputTokens
	}
	responseBody, finalStopReason := buildClaudeResponse(content, toolUses, model, &usage, stopReason, requestCtx)
	return &ParseResult{
		ResponseBody:           responseBody,
		Usage:                  usage,
		StopReason:             finalStopReason,
		UpstreamTerminalSignal: terminal,
		EchoedPlaceholder:      kiroEchoedPlaceholder(content),
	}, nil
}

func StreamEventStreamAsAnthropicWithContext(ctx context.Context, body io.Reader, w io.Writer, model string, inputTokens int, requestCtx KiroRequestContext) (*StreamResult, error) {
	reader := bufio.NewReader(body)
	start := time.Now()
	var firstDelta *time.Duration
	usage := Usage{InputTokens: inputTokens}
	contentBlockIndex := -1
	thinkingBlockIndex := -1
	messageStartSent := false
	textBlockOpen := false
	thinkingBlockOpen := false
	processedIDs := make(map[string]bool)
	emittedToolContents := make(map[string]bool)
	streamingToolNames := make(map[string]string)
	streamingToolStopped := make(map[string]bool)
	streamingToolInputBuf := make(map[string]*strings.Builder)
	streamingToolInvalid := make(map[string]bool)
	currentStreamingToolID := ""
	toolBlockEmitted := false
	emittedToolCount := 0
	toolLimitReached := func() bool {
		return requestCtx.MaxResponseToolUses > 0 && emittedToolCount >= requestCtx.MaxResponseToolUses
	}
	pendingAssistantText := ""
	lastContentFragment := ""
	pendingLeadingWhitespace := ""
	stopReason := ""
	upstreamTerminalSignal := false
	contextWindowFull := false
	stopSequenceMatched := ""
	stopSequencePendingText := ""
	thinkingBuffer := ""
	var currentThinking strings.Builder
	inThinkingBlock := false
	stripThinkingLeadingNewline := false
	currentMessageID := ""
	var outputTextBuf strings.Builder

	writeEvent := func(event string, data any) error {
		payload, err := json.Marshal(data)
		if err != nil {
			return err
		}
		_, err = io.WriteString(w, "event: "+event+"\ndata: "+string(payload)+"\n\n")
		return err
	}
	ensureMessageStart := func() error {
		if messageStartSent {
			return nil
		}
		useMsgID := newClaudeMessageID()
		startUsage := usage
		if requestCtx.CacheEmulationUsage != nil {
			startUsage = mergeKiroCacheEmulationUsage(startUsage, requestCtx.CacheEmulationUsage)
		}
		usageMap := map[string]any{
			"input_tokens":  startUsage.InputTokens,
			"output_tokens": 0,
		}
		addKiroCacheUsageFields(usageMap, startUsage)
		if err := writeEvent("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            useMsgID,
				"type":          "message",
				"role":          "assistant",
				"content":       []any{},
				"model":         model,
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         usageMap,
			},
		}); err != nil {
			return err
		}
		messageStartSent = true
		if currentMessageID == "" {
			currentMessageID = useMsgID
		}
		return nil
	}

	closeText := func() error {
		if !textBlockOpen {
			return nil
		}
		textBlockOpen = false
		return writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": contentBlockIndex})
	}
	closeThinking := func() error {
		if !thinkingBlockOpen {
			return nil
		}
		if currentThinking.Len() > 0 {
			sig := thinkingSignature(currentThinking.String(), model, currentMessageID)
			currentThinking.Reset()
			if sig != "" {
				if err := writeEvent("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": thinkingBlockIndex,
					"delta": map[string]any{
						"type":      "signature_delta",
						"signature": sig,
					},
				}); err != nil {
					return err
				}
			}
		}
		thinkingBlockOpen = false
		return writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": thinkingBlockIndex})
	}
	discardStreamingTool := func(toolUseID string) {
		if toolUseID == "" {
			return
		}
		streamingToolStopped[toolUseID] = true
		if currentStreamingToolID == toolUseID {
			currentStreamingToolID = ""
		}
		delete(streamingToolNames, toolUseID)
		delete(streamingToolInputBuf, toolUseID)
		delete(streamingToolInvalid, toolUseID)
	}
	closeStreamingTool := func(toolUseID string) error {
		if toolUseID == "" || streamingToolStopped[toolUseID] {
			return nil
		}
		streamingToolStopped[toolUseID] = true

		name := streamingToolNames[toolUseID]
		buf := streamingToolInputBuf[toolUseID]
		invalid := streamingToolInvalid[toolUseID]
		discardStreamingTool(toolUseID)
		if invalid || name == "" || buf == nil {
			return nil
		}

		responseName := normalizeResponseToolName(restoreResponseToolName(name, requestCtx))
		inputJSON, input, ok := normalizeStreamingToolInput(responseName, buf.String())
		if !ok {
			return nil
		}
		processedIDs[toolUseID] = true
		tool := KiroToolUse{ToolUseID: toolUseID, Name: responseName, Input: input}
		contentKey := toolUseContentKey(tool)
		if contentKey == "" || emittedToolContents[contentKey] || toolLimitReached() {
			return nil
		}
		if err := ensureMessageStart(); err != nil {
			return err
		}
		if err := closeThinking(); err != nil {
			return err
		}
		if err := closeText(); err != nil {
			return err
		}
		if firstDelta == nil {
			delta := time.Since(start)
			firstDelta = &delta
		}
		contentBlockIndex++
		blockIndex := contentBlockIndex
		if err := writeEvent("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": blockIndex,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    toolUseID,
				"name":  responseName,
				"input": map[string]any{},
			},
		}); err != nil {
			return err
		}
		if err := writeEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": blockIndex,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": inputJSON,
			},
		}); err != nil {
			return err
		}
		if err := writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIndex}); err != nil {
			return err
		}
		emittedToolContents[contentKey] = true
		toolBlockEmitted = true
		emittedToolCount++
		_, _ = outputTextBuf.WriteString(inputJSON)
		if stopReason == "" {
			stopReason = "tool_use"
		}
		return nil
	}
	closeOpenStreamingTool := func() error {
		return closeStreamingTool(currentStreamingToolID)
	}
	bufferStreamingToolInput := func(toolUseID, name, fragment string, snapshot bool) error {
		if toolUseID == "" || processedIDs[toolUseID] || streamingToolStopped[toolUseID] {
			return nil
		}
		if currentStreamingToolID != "" && currentStreamingToolID != toolUseID {
			if err := closeOpenStreamingTool(); err != nil {
				return err
			}
		}
		currentStreamingToolID = toolUseID
		if name != "" {
			streamingToolNames[toolUseID] = name
		}
		if snapshot {
			delete(streamingToolInvalid, toolUseID)
		} else if streamingToolInvalid[toolUseID] {
			return nil
		}
		buf, ok := streamingToolInputBuf[toolUseID]
		if !ok {
			buf = &strings.Builder{}
			streamingToolInputBuf[toolUseID] = buf
		}
		if snapshot {
			buf.Reset()
		}
		if len(fragment) > maxEventMsgSize-buf.Len() {
			streamingToolInvalid[toolUseID] = true
			delete(streamingToolInputBuf, toolUseID)
			return nil
		}
		_, _ = buf.WriteString(fragment)
		return nil
	}
	processStreamingToolInput := func(toolUseID, name, fragment string, inputMap map[string]any) error {
		if toolUseID == "" {
			return nil
		}
		snapshot := false
		if inputMap != nil {
			encoded, err := json.Marshal(inputMap)
			if err != nil {
				return err
			}
			fragment = string(encoded)
			snapshot = true
		}
		return bufferStreamingToolInput(toolUseID, name, fragment, snapshot)
	}
	processStreamingToolStop := func(toolUseID string) error {
		if toolUseID == "" {
			toolUseID = currentStreamingToolID
		}
		return closeStreamingTool(toolUseID)
	}
	writeTextDelta := func(text string, allowWhitespace bool) error {
		if text == "" {
			return nil
		}
		if !allowWhitespace {
			if strings.TrimSpace(text) == "" {
				// 纯空白片段: 文本块未开启时视为前导噪音直接丢弃;
				// 已开启时先缓冲, 仅当后续出现真实文本才补回(中段空行);
				// 若流结束时仍在缓冲则视为尾部空白, 自然不吐出。
				if textBlockOpen {
					pendingLeadingWhitespace += text
				}
				return nil
			}
			if !textBlockOpen {
				// 首段真实文本: 裁掉其前导空白
				text = strings.TrimLeftFunc(text, unicode.IsSpace)
				pendingLeadingWhitespace = ""
				if text == "" {
					return nil
				}
			} else if pendingLeadingWhitespace != "" {
				// 中段: 把缓冲的空行补回本段文本之前
				text = pendingLeadingWhitespace + text
				pendingLeadingWhitespace = ""
			}
		}
		if err := closeOpenStreamingTool(); err != nil {
			return err
		}
		if err := ensureMessageStart(); err != nil {
			return err
		}
		if firstDelta == nil {
			delta := time.Since(start)
			firstDelta = &delta
		}
		if err := closeThinking(); err != nil {
			return err
		}
		if !textBlockOpen {
			contentBlockIndex++
			textBlockOpen = true
			if err := writeEvent("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": contentBlockIndex,
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			}); err != nil {
				return err
			}
		}
		_, _ = outputTextBuf.WriteString(text)
		return writeEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": contentBlockIndex,
			"delta": map[string]any{
				"type": "text_delta",
				"text": text,
			},
		})
	}
	emitTextDelta := func(text string, allowWhitespace bool) error {
		if stopSequenceMatched != "" {
			return nil
		}
		if text == "" {
			return nil
		}
		if len(requestCtx.StopSequences) == 0 {
			return writeTextDelta(text, allowWhitespace)
		}
		stopSequencePendingText += text
		if idx, matched := firstStopSequenceIndex(stopSequencePendingText, requestCtx.StopSequences); matched != "" {
			emitText := stopSequencePendingText[:idx]
			stopSequencePendingText = ""
			err := writeTextDelta(emitText, allowWhitespace)
			stopSequenceMatched = matched
			stopReason = "stop_sequence"
			return err
		}
		suffix := stopSequencePotentialSuffix(stopSequencePendingText, requestCtx.StopSequences)
		if len(suffix) == len(stopSequencePendingText) {
			return nil
		}
		emitText := stopSequencePendingText[:len(stopSequencePendingText)-len(suffix)]
		stopSequencePendingText = suffix
		return writeTextDelta(emitText, allowWhitespace)
	}
	flushTextStopBuffer := func() error {
		if stopSequencePendingText == "" {
			return nil
		}
		text := stopSequencePendingText
		stopSequencePendingText = ""
		return writeTextDelta(text, true)
	}
	emitToolUse := func(tool KiroToolUse) error {
		structuredOutput := isStructuredOutputToolName(tool.Name, requestCtx)
		tool.Name = normalizeResponseToolName(restoreResponseToolName(tool.Name, requestCtx))
		if !structuredOutput && !isEmittableToolUse(tool) {
			return nil
		}
		if !shouldEmitToolUse(tool, emittedToolContents) {
			return nil
		}
		if structuredOutput {
			inputJSON, err := json.Marshal(tool.Input)
			if err != nil {
				inputJSON = []byte("{}")
			}
			if stopReason == "" || stopReason == "tool_use" {
				stopReason = "end_turn"
			}
			return emitTextDelta(string(inputJSON), true)
		}
		if toolLimitReached() {
			return nil
		}
		if err := closeOpenStreamingTool(); err != nil {
			return err
		}
		if err := ensureMessageStart(); err != nil {
			return err
		}
		if err := closeText(); err != nil {
			return err
		}
		if err := closeThinking(); err != nil {
			return err
		}
		contentBlockIndex++
		if err := writeEvent("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": contentBlockIndex,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    tool.ToolUseID,
				"name":  tool.Name,
				"input": map[string]any{},
			},
		}); err != nil {
			return err
		}
		inputJSON, _ := json.Marshal(tool.Input)
		_, _ = outputTextBuf.Write(inputJSON)
		if err := writeEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": contentBlockIndex,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": string(inputJSON),
			},
		}); err != nil {
			return err
		}
		if err := writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": contentBlockIndex}); err != nil {
			return err
		}
		toolBlockEmitted = true
		emittedToolCount++
		return nil
	}
	flushPendingAssistantText := func() error {
		text, embeddedTools, pending := drainEmbeddedToolText(pendingAssistantText)
		pendingAssistantText = pending
		if err := emitTextDelta(text, false); err != nil {
			return err
		}
		for _, tool := range embeddedTools {
			if err := emitToolUse(tool); err != nil {
				return err
			}
		}
		return nil
	}
	emitPlainAssistantText := func(text string) error {
		if text == "" {
			return nil
		}
		pendingAssistantText += text
		return flushPendingAssistantText()
	}
	startThinkingBlock := func() error {
		if err := closeOpenStreamingTool(); err != nil {
			return err
		}
		if err := closeText(); err != nil {
			return err
		}
		if err := ensureMessageStart(); err != nil {
			return err
		}
		if firstDelta == nil {
			delta := time.Since(start)
			firstDelta = &delta
		}
		if thinkingBlockOpen {
			return nil
		}
		contentBlockIndex++
		thinkingBlockIndex = contentBlockIndex
		thinkingBlockOpen = true
		return writeEvent("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": thinkingBlockIndex,
			"content_block": map[string]any{
				"type":     "thinking",
				"thinking": "",
			},
		})
	}
	emitThinkingDelta := func(text string) error {
		if !thinkingBlockOpen {
			if err := startThinkingBlock(); err != nil {
				return err
			}
		}
		if text != "" {
			_, _ = outputTextBuf.WriteString(text)
			_, _ = currentThinking.WriteString(text)
		}
		return writeEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": thinkingBlockIndex,
			"delta": map[string]any{
				"type":     "thinking_delta",
				"thinking": text,
			},
		})
	}
	finishThinkingBlock := func() error {
		return closeThinking()
	}
	processThinkingTaggedText := func(text string) error {
		if text == "" {
			return nil
		}
		thinkingBuffer += text
		for {
			if !inThinkingBlock {
				startPos := findRealThinkingStartTag(thinkingBuffer, 0)
				if startPos != -1 {
					before := thinkingBuffer[:startPos]
					if strings.TrimSpace(before) != "" {
						if err := emitPlainAssistantText(before); err != nil {
							return err
						}
					}
					inThinkingBlock = true
					stripThinkingLeadingNewline = true
					thinkingBuffer = thinkingBuffer[startPos+len(thinkingStartTag):]
					if err := startThinkingBlock(); err != nil {
						return err
					}
					continue
				}
				safeLen := safeThinkingStreamFlushLen(thinkingBuffer, len(thinkingStartTag))
				if safeLen > 0 {
					safeText := thinkingBuffer[:safeLen]
					if strings.TrimSpace(safeText) != "" {
						if err := emitPlainAssistantText(safeText); err != nil {
							return err
						}
						thinkingBuffer = thinkingBuffer[safeLen:]
					}
				}
				break
			}
			if stripThinkingLeadingNewline {
				if strings.HasPrefix(thinkingBuffer, "\n") {
					thinkingBuffer = thinkingBuffer[1:]
					stripThinkingLeadingNewline = false
				} else if thinkingBuffer != "" {
					stripThinkingLeadingNewline = false
				}
			}
			endPos := findStreamThinkingEndTagStrict(thinkingBuffer, 0)
			if endPos != -1 {
				if thinkingText := thinkingBuffer[:endPos]; thinkingText != "" {
					if err := emitThinkingDelta(thinkingText); err != nil {
						return err
					}
				}
				inThinkingBlock = false
				if err := finishThinkingBlock(); err != nil {
					return err
				}
				thinkingBuffer = thinkingBuffer[endPos+len(thinkingEndTag)+len("\n\n"):]
				continue
			}
			safeLen := safeThinkingStreamFlushLen(thinkingBuffer, len(thinkingEndTag)+len("\n\n"))
			if safeLen > 0 {
				if err := emitThinkingDelta(thinkingBuffer[:safeLen]); err != nil {
					return err
				}
				thinkingBuffer = thinkingBuffer[safeLen:]
			}
			break
		}
		return nil
	}
	flushThinkingAtBoundary := func() error {
		if !requestCtx.ThinkingEnabled || thinkingBuffer == "" {
			return nil
		}
		if inThinkingBlock {
			endPos := findStreamThinkingEndTagAtBufferEnd(thinkingBuffer, 0)
			if endPos != -1 {
				if thinkingText := thinkingBuffer[:endPos]; thinkingText != "" {
					if err := emitThinkingDelta(thinkingText); err != nil {
						return err
					}
				}
				afterPos := endPos + len(thinkingEndTag)
				remaining := strings.TrimLeftFunc(thinkingBuffer[afterPos:], unicode.IsSpace)
				thinkingBuffer = ""
				inThinkingBlock = false
				if err := finishThinkingBlock(); err != nil {
					return err
				}
				return emitPlainAssistantText(remaining)
			}
			if err := emitThinkingDelta(thinkingBuffer); err != nil {
				return err
			}
			thinkingBuffer = ""
			inThinkingBlock = false
			return finishThinkingBlock()
		}
		remaining := thinkingBuffer
		thinkingBuffer = ""
		return emitPlainAssistantText(remaining)
	}
	flushThinkingAtEOF := func() error {
		if !requestCtx.ThinkingEnabled {
			return nil
		}
		return flushThinkingAtBoundary()
	}

	applySemanticEvent := func(evt *kiroSemanticEvent) error {
		if evt == nil {
			return nil
		}
		// 仅接受 Anthropic 协议规定的 stop_reason 白名单值
		// 上游中间帧若透传 pause_turn/refusal/stop_sequence 等新值会让客户端误判为终态
		// 其余值忽略,等流真正 EOF 时由后续兜底分支按 tool_use/end_turn 处理
		if evt.SourceStopReason != "" {
			sourceStopReason := strings.ToLower(strings.TrimSpace(evt.SourceStopReason))
			switch sourceStopReason {
			case "max_tokens":
				if stopReason != "stop_sequence" {
					stopReason = sourceStopReason
				}
			case "end_turn", "tool_use":
				if stopReason != "max_tokens" && stopReason != "stop_sequence" {
					stopReason = sourceStopReason
				}
			}
		}
		switch evt.Type {
		case kiroSemanticContent:
			if evt.Content == "" {
				return nil
			}
			lastContentFragment = evt.Content
			if evt.IsDuplicateContent {
				return nil
			}
			if requestCtx.ThinkingEnabled {
				return processThinkingTaggedText(evt.Content)
			}
			pendingAssistantText += evt.Content
			return flushPendingAssistantText()
		case kiroSemanticReasoning:
			if evt.Reasoning == "" || !requestCtx.ThinkingEnabled {
				return nil
			}
			// 连续的 reasoningContentEvent 片段累积进同一个 thinking 块。
			// 该块在遇到文本/工具/EOF 等边界时由 closeThinking 统一闭合；
			// 不可对每个片段单独包 <thinking></thinking>，否则每片会各自开关一个块导致碎片化。
			return emitThinkingDelta(evt.Reasoning)
		case kiroSemanticAssistantTU:
			if evt.ToolUse == nil || processedIDs[evt.ToolUse.ToolUseID] {
				return nil
			}
			structuredOutput := isStructuredOutputToolName(evt.ToolUse.Name, requestCtx)
			if (!structuredOutput && !isEmittableToolUse(*evt.ToolUse)) || evt.ToolUse.IsTruncated {
				return nil
			}
			discardStreamingTool(evt.ToolUse.ToolUseID)
			processedIDs[evt.ToolUse.ToolUseID] = true
			if err := flushThinkingAtBoundary(); err != nil {
				return err
			}
			return emitToolUse(*evt.ToolUse)
		case kiroSemanticToolUse:
			if err := flushThinkingAtBoundary(); err != nil {
				return err
			}
			return processStreamingToolInput(evt.ToolUseID, evt.ToolName, evt.ToolInput, evt.ToolInputMap)
		case kiroSemanticToolInput:
			if err := flushThinkingAtBoundary(); err != nil {
				return err
			}
			return processStreamingToolInput(evt.ToolUseID, evt.ToolName, evt.ToolInput, evt.ToolInputMap)
		case kiroSemanticToolStop:
			if err := flushThinkingAtBoundary(); err != nil {
				return err
			}
			return processStreamingToolStop(evt.ToolUseID)
		case kiroSemanticUsage:
			updateUsageFromEvent(&usage, evt.SourceEventType, evt.RawEvent)
			return nil
		default:
			return nil
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		msg, err := readEventStreamMessage(reader)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if msg.isException() {
			// 已有输出时把长度超限当作截断收尾；其余异常（含无输出的长度超限）
			// 必须作为失败上抛，否则会被伪装成正常 end_turn。
			if msg.ExceptionType == kiroContentLengthExceededException && (contentBlockIndex >= 0 || pendingAssistantText != "" || thinkingBuffer != "") {
				stopReason = "max_tokens"
				upstreamTerminalSignal = true
				break
			}
			return nil, newKiroStreamException(msg)
		}
		if msg == nil || len(msg.Payload) == 0 {
			continue
		}

		var event map[string]any
		decoder := json.NewDecoder(bytes.NewReader(msg.Payload))
		decoder.UseNumber()
		if err := decoder.Decode(&event); err != nil {
			continue
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			continue
		}

		if kiroUpstreamTraceEnabled {
			payloadPrefix := string(msg.Payload)
			if len(payloadPrefix) > 300 {
				payloadPrefix = payloadPrefix[:300]
			}
			fmt.Fprintf(os.Stderr, "[KIRO_TRACE] model=%s thinkingEnabled=%v eventType=%q payload=%s\n",
				model, requestCtx.ThinkingEnabled, msg.EventType, payloadPrefix)
		}

		if isKiroTerminalEvent(msg.EventType) || readStopReason(event) != "" || readStopReason(nestedEvent(event, msg.EventType)) != "" {
			upstreamTerminalSignal = true
		}
		if kiroContextWindowFull(msg.EventType, event) {
			contextWindowFull = true
		}
		semanticEvents := extractSemanticEvents(msg.EventType, event, &lastContentFragment)
		for i := range semanticEvents {
			if kiroUpstreamTraceEnabled {
				ev := &semanticEvents[i]
				detail := ev.Content
				if detail == "" {
					detail = ev.Reasoning
				}
				if len(detail) > 200 {
					detail = detail[:200]
				}
				fmt.Fprintf(os.Stderr, "[KIRO_TRACE]   -> semanticType=%q detail=%q\n", ev.Type, detail)
			}
			if err := applySemanticEvent(&semanticEvents[i]); err != nil {
				return nil, err
			}
		}
	}

	if err := closeOpenStreamingTool(); err != nil {
		return nil, err
	}
	if err := flushThinkingAtEOF(); err != nil {
		return nil, err
	}
	if err := flushPendingAssistantText(); err != nil {
		return nil, err
	}
	if err := flushTextStopBuffer(); err != nil {
		return nil, err
	}
	// 仅有 thinking 块、无 text 输出不代表截断,opus 4.8 思考密集场景常见。
	// 真正的截断由上游 ContentLengthExceededException 异常帧在读循环中设置 stop_reason。

	if err := closeText(); err != nil {
		return nil, err
	}
	if err := closeThinking(); err != nil {
		return nil, err
	}
	if usage.OutputTokens == 0 {
		if est := anthropictokenizer.CountTokens(outputTextBuf.String()); est > 0 {
			usage.OutputTokens = est
		}
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	if requestCtx.CacheEmulationUsage != nil {
		usage = mergeKiroCacheEmulationUsage(usage, requestCtx.CacheEmulationUsage)
	}
	switch stopReason {
	case "max_tokens", "stop_sequence":
		// These terminal conditions take precedence over emitted tool blocks.
	case "":
		if toolBlockEmitted {
			stopReason = "tool_use"
		} else {
			stopReason = "end_turn"
		}
	default:
		if toolBlockEmitted {
			stopReason = "tool_use"
		} else if stopReason == "tool_use" {
			stopReason = "end_turn"
		}
	}
	stopReason = applyContextWindowStopReason(stopReason, contextWindowFull, toolBlockEmitted)
	if err := ensureMessageStart(); err != nil {
		return nil, err
	}
	finalUsageMap := map[string]any{
		"input_tokens":  usage.InputTokens,
		"output_tokens": usage.OutputTokens,
	}
	if usage.KiroCredits > 0 {
		finalUsageMap["_sub2api_kiro_credits"] = usage.KiroCredits
	}
	addKiroCacheUsageFields(finalUsageMap, usage)
	if err := writeEvent("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nullableStopSequence(stopSequenceMatched),
		},
		"usage": finalUsageMap,
	}); err != nil {
		return nil, err
	}
	if err := writeEvent("message_stop", map[string]any{"type": "message_stop"}); err != nil {
		return nil, err
	}

	return &StreamResult{
		Usage:                  usage,
		StopReason:             stopReason,
		UpstreamTerminalSignal: upstreamTerminalSignal,
		EchoedPlaceholder:      kiroEchoedPlaceholder(outputTextBuf.String()),
		FirstDeltaDur:          firstDelta,
	}, nil
}

func extractSystemPrompt(claudeBody []byte) string {
	return extractTextFromContentBlocks(gjson.GetBytes(claudeBody, "system"))
}

// extractTextFromContentBlocks 把 Claude 的 content 字段（字符串或 text block 数组）拼成纯文本。
// extractTextFromContentBlocks 拼接 text 块。块之间补换行：
// Claude Code 的 system 第一块是 billing-header 行，直接相连会和下一句粘成一行。
func extractTextFromContentBlocks(content gjson.Result) string {
	if content.IsArray() {
		var sb strings.Builder
		for _, block := range content.Array() {
			text := ""
			if block.Get("type").String() == "text" {
				text = block.Get("text").String()
			} else if block.Type == gjson.String {
				text = block.String()
			}
			if text == "" {
				continue
			}
			if sb.Len() > 0 && !strings.HasSuffix(sb.String(), "\n") && !strings.HasPrefix(text, "\n") {
				_ = sb.WriteByte('\n')
			}
			_, _ = sb.WriteString(text)
		}
		return sb.String()
	}
	return content.String()
}

// extractInlineSystemPrompts 从 messages 中提取所有 role=="system" 的中途消息文本，
// 返回拼接后的 system 文本与剔除 system 后（顺序保留）的消息切片。
// Claude 桌面版 beta mid-conversation-system-2026-04-07 会在 messages 中插入 system 消息，
// Kiro/CodeWhisperer 不支持中途 system，故在此提取并折叠进顶层 systemPrompt。
func extractInlineSystemPrompts(messages gjson.Result) (string, []gjson.Result) {
	arr := messages.Array()
	var sb strings.Builder
	filtered := make([]gjson.Result, 0, len(arr))
	for _, msg := range arr {
		if msg.Get("role").String() == "system" {
			text := strings.TrimSpace(extractTextFromContentBlocks(msg.Get("content")))
			if text != "" {
				if sb.Len() > 0 {
					_, _ = sb.WriteString("\n\n")
				}
				_, _ = sb.WriteString(text)
			}
			continue
		}
		filtered = append(filtered, msg)
	}
	return sb.String(), filtered
}

func deriveThinkingDirective(body []byte, headers http.Header) *thinkingDirective {
	if override := thinkingDirectiveFromModel(gjson.GetBytes(body, "model").String()); override != nil {
		return override
	}
	switch thinkingType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String())); thinkingType {
	case "adaptive":
		effort := strings.TrimSpace(gjson.GetBytes(body, "output_config.effort").String())
		if effort == "" {
			effort = "high"
		}
		budget := int(gjson.GetBytes(body, "thinking.budget_tokens").Int())
		if budget <= 0 {
			budget = 20000
		}
		return &thinkingDirective{Mode: "adaptive", BudgetTokens: budget, Effort: effort}
	case "enabled":
		budget := int(gjson.GetBytes(body, "thinking.budget_tokens").Int())
		if budget <= 0 {
			budget = 16000
		}
		return &thinkingDirective{Mode: "enabled", BudgetTokens: budget}
	case "disabled":
		// 客户端显式关闭时不得被下面的 beta 头 / reasoning_effort 兜底重新打开：
		// Claude Code 的后台小模型调用（标题、摘要）也带 interleaved-thinking beta。
		return nil
	}
	if headers != nil {
		if beta := headers.Get("Anthropic-Beta"); strings.Contains(beta, "interleaved-thinking") {
			return &thinkingDirective{Mode: "enabled", BudgetTokens: 16000}
		}
	}
	if effort := gjson.GetBytes(body, "reasoning_effort").String(); effort != "" && effort != "none" {
		return &thinkingDirective{Mode: "enabled", BudgetTokens: 16000}
	}
	model := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "model").String()))
	if strings.Contains(model, "-reason") {
		return &thinkingDirective{Mode: "enabled", BudgetTokens: 16000}
	}
	return nil
}

func thinkingDirectiveFromModel(model string) *thinkingDirective {
	model = strings.ToLower(strings.TrimSpace(model))
	if !strings.Contains(model, "thinking") {
		return nil
	}

	switch normalizeModelAlias(model) {
	case "claude-opus-4-6", "claude-opus-4.6":
		return &thinkingDirective{
			Mode:         "adaptive",
			BudgetTokens: 20000,
			Effort:       "high",
		}
	// opus 4.7/4.8/5 走 adaptive 高预算,budget 对齐 Antigravity 的 ClaudeAdaptiveHighThinkingBudgetTokens
	// 避免 thinking 提前耗尽导致流式中途断开
	case "claude-opus-4-7", "claude-opus-4.7",
		"claude-opus-4-8", "claude-opus-4.8",
		"claude-opus-5":
		return &thinkingDirective{
			Mode:         "adaptive",
			BudgetTokens: 24576,
			Effort:       "high",
		}
	default:
		return &thinkingDirective{
			Mode:         "enabled",
			BudgetTokens: 20000,
		}
	}
}

// renderKiroBuiltinIdentityPrompt 渲染 kiroBuiltinIdentityPrompt 中的 {{identity}} 占位符。
//
// kiroBuiltinIdentityPrompt 内的 <identity> 段写有 "You are {{identity}}, ...",
// 这是个字面量;若不替换,模型会直接复读 "I am {{identity}}",对 Opus 4.7/4.8 这类
// 对格式更敏感的版本尤其明显。
//
// identity 为空时回退到 "Claude",对齐 prompt 中 <CRITICAL_OVERRIDE> 的兜底语义:
// "If no identity is provided, say that you are Claude."
func renderKiroBuiltinIdentityPrompt(identity string) string {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		identity = "Claude"
	}
	return strings.ReplaceAll(kiroBuiltinIdentityPrompt, "{{identity}}", identity)
}

func buildInjectedSystemPrompt(systemPrompt string, thinking *thinkingDirective, toolChoiceHint string) string {
	systemPrompt = strings.TrimSpace(systemPrompt)
	promptParts := []string{renderKiroBuiltinIdentityPrompt("")}
	if temporalContext := buildKiroTemporalContext(); temporalContext != "" {
		promptParts = append(promptParts, temporalContext)
	}
	if systemPrompt != "" {
		promptParts = append(promptParts, systemPrompt)
	}
	systemPrompt = strings.Join(promptParts, "\n\n")
	if toolChoiceHint != "" {
		if systemPrompt != "" {
			systemPrompt += "\n"
		}
		systemPrompt += toolChoiceHint
	}
	if !strings.Contains(systemPrompt, systemChunkedWritePolicy) {
		systemPrompt += "\n" + systemChunkedWritePolicy
	}
	if thinking != nil {
		switch thinking.Mode {
		case "adaptive":
			effort := strings.TrimSpace(thinking.Effort)
			if effort == "" {
				effort = "high"
			}
			thinkingPrefix := "<thinking_mode>adaptive</thinking_mode>\n<thinking_effort>" + effort + "</thinking_effort>"
			return thinkingPrefix + "\n\n" + systemPrompt
		default:
			budget := thinking.BudgetTokens
			if budget <= 0 {
				budget = 16000
			}
			thinkingPrefix := "<thinking_mode>enabled</thinking_mode>\n<max_thinking_length>" + strconv.Itoa(budget) + "</max_thinking_length>"
			return thinkingPrefix + "\n\n" + systemPrompt
		}
	}
	return systemPrompt
}

func buildKiroTemporalContext() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SUB2API_KIRO_TIME_CONTEXT"))) {
	case "date", "day":
		return fmt.Sprintf("[Context: Current date is %s]", time.Now().Format("2006-01-02 MST"))
	case "precise", "time", "full":
		return fmt.Sprintf("[Context: Current time is %s]", time.Now().Format("2006-01-02 15:04:05 MST"))
	default:
		return ""
	}
}

// buildAdditionalModelRequestFields 构建 Kiro payload 的 additionalModelRequestFields。
// 对 Claude 4.6+ 模型，使用 output_config.effort 路径（官方 Kiro IDE 的 kr() 逻辑）：
//
//	output_config 路径 → { thinking: {type:'adaptive',display:'summarized'}, output_config: {effort} }
//
// 对于旧模型或 enabled 模式，不注入（依赖 system prompt 标签兜底）。
//
// 这实现了管理器的 P1 功能：确保 Claude 4.6+ 新模型的 thinking 使用 effort-based 控制。
//
// GPT-5.6 不在此路径内：Kiro 协议没有 reasoning.effort 字段，且 GPT 系列未被确认
// 接受 additionalModelRequestFields（向未确认模型下发会触发上游 400
// "additionalModelRequestFields is not supported"）。客户端请求的 reasoning_effort
// 对 GPT 暂不透传，待抓包确认字段名与模型支持情况后再实现。
func buildAdditionalModelRequestFields(thinking *thinkingDirective, modelID string) map[string]any {
	if thinking == nil {
		return nil
	}
	// 判断是否是 output_config 路径的模型（Claude 4.6+）
	if !isOutputConfigPathModel(modelID) {
		return nil
	}
	if thinking.Mode == "adaptive" {
		effort := strings.TrimSpace(thinking.Effort)
		if effort == "" {
			effort = "high"
		}
		return map[string]any{
			"thinking":      map[string]any{"type": "adaptive", "display": "summarized"},
			"output_config": map[string]any{"effort": effort},
		}
	}
	// enabled 模式对 output_config 路径模型也映射为 adaptive + effort
	if thinking.Mode == "enabled" {
		effort := budgetToEffort(thinking.BudgetTokens)
		return map[string]any{
			"thinking":      map[string]any{"type": "adaptive", "display": "summarized"},
			"output_config": map[string]any{"effort": effort},
		}
	}
	return nil
}

// isOutputConfigPathModel 判断模型是否使用 output_config 路径（Claude 4.6+）。
// 这是基于已知模型列表的静态判断，未来可改为动态从 ListAvailableModels 发现。
func isOutputConfigPathModel(modelID string) bool {
	normalized := normalizeClaudeVersionNumber(strings.ToLower(strings.TrimSpace(modelID)))
	// Claude 4.6+ 所有模型使用 output_config 路径
	for _, prefix := range []string{"claude-opus-4.6", "claude-opus-4.7", "claude-opus-4.8",
		"claude-opus-5", "claude-sonnet-5", "claude-sonnet-4.6"} {
		if normalized == prefix || strings.HasPrefix(normalized, prefix+"-") || strings.HasPrefix(normalized, prefix+".") {
			return true
		}
	}
	// 通用兜底：版本号 >= 4.6 的 Claude 模型（处理未来新版本）
	if matches := claudeDottedVersionPattern.FindStringSubmatch(normalized); matches != nil {
		major, _ := strconv.Atoi(matches[2])
		minor, _ := strconv.Atoi(matches[3])
		if major > 4 || (major == 4 && minor >= 6) {
			return true
		}
	}
	return false
}

// budgetToEffort 将 thinking budget_tokens 粗略映射为 effort 等级。
// 参考管理器的映射规则。
func budgetToEffort(budgetTokens int) string {
	switch {
	case budgetTokens <= 4000:
		return "low"
	case budgetTokens <= 16000:
		return "medium"
	case budgetTokens <= 64000:
		return "high"
	default:
		return "xhigh"
	}
}

func extractClaudeToolChoiceHint(claudeBody []byte, requestCtx *KiroRequestContext) string {
	toolChoice := gjson.GetBytes(claudeBody, "tool_choice")
	if !toolChoice.Exists() {
		return ""
	}

	if toolChoice.Type == gjson.String {
		switch strings.ToLower(strings.TrimSpace(toolChoice.String())) {
		case "none":
			return "[INSTRUCTION: Do not use any tools. Respond with text only.]"
		case "auto", "":
			return ""
		}
	}

	switch strings.ToLower(strings.TrimSpace(toolChoice.Get("type").String())) {
	case "any":
		return "[INSTRUCTION: You MUST use at least one of the available tools to respond. Do not respond with text only - always make a tool call.]"
	case "tool":
		toolName := mapKiroToolName(toolChoice.Get("name").String(), requestCtx)
		if toolName != "" {
			return fmt.Sprintf("[INSTRUCTION: You MUST use the tool named '%s' to respond. Do not use any other tool or respond with text only.]", toolName)
		}
	case "none":
		return "[INSTRUCTION: Do not use any tools. Respond with text only.]"
	}

	return ""
}

// parallelToolUseDisabled 读取 tool_choice.disable_parallel_tool_use（auto/any/tool 均可携带）。
// tool_choice=none 时不会有工具调用，无需限制。
func parallelToolUseDisabled(claudeBody []byte) bool {
	toolChoice := gjson.GetBytes(claudeBody, "tool_choice")
	if !toolChoice.IsObject() || isToolChoiceNone(claudeBody) {
		return false
	}
	return toolChoice.Get("disable_parallel_tool_use").Bool()
}

func extractClaudeStopSequences(claudeBody []byte) []string {
	raw := gjson.GetBytes(claudeBody, "stop_sequences")
	if !raw.IsArray() {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	for _, item := range raw.Array() {
		if item.Type != gjson.String {
			continue
		}
		seq := item.String()
		if seq == "" || seen[seq] {
			continue
		}
		seen[seq] = true
		out = append(out, seq)
	}
	return out
}

func hasForcedClaudeToolChoice(claudeBody []byte) bool {
	toolChoice := gjson.GetBytes(claudeBody, "tool_choice")
	if !toolChoice.Exists() {
		return false
	}
	if toolChoice.Type == gjson.String {
		switch strings.ToLower(strings.TrimSpace(toolChoice.String())) {
		case "any", "required":
			return true
		default:
			return false
		}
	}
	switch strings.ToLower(strings.TrimSpace(toolChoice.Get("type").String())) {
	case "any", "tool":
		return true
	default:
		return false
	}
}

func joinPromptHints(hints ...string) string {
	var out []string
	for _, hint := range hints {
		hint = strings.TrimSpace(hint)
		if hint != "" {
			out = append(out, hint)
		}
	}
	return strings.Join(out, "\n")
}

func buildStructuredOutputTool(claudeBody []byte, requestCtx *KiroRequestContext) (*KiroToolWrapper, string) {
	format, ok := extractStructuredOutputFormat(claudeBody)
	if !ok {
		return nil, ""
	}
	formatType := strings.ToLower(strings.TrimSpace(format.Get("type").String()))
	switch formatType {
	case "json_object":
		return nil, "[INSTRUCTION: Respond only with one valid JSON object. Do not include markdown fences, prose, comments, or trailing text.]"
	case "json_schema":
	default:
		return nil, ""
	}

	schema := firstExistingJSON(format.Get("schema"), format.Get("json_schema.schema"))
	if !schema.Exists() {
		return nil, "[INSTRUCTION: Respond only with one valid JSON object that satisfies the requested structured output format. Do not include markdown fences, prose, comments, or trailing text.]"
	}
	toolName := strings.TrimSpace(firstNonEmptyString(
		format.Get("name").String(),
		format.Get("json_schema.name").String(),
	))
	if toolName == "" {
		toolName = structuredOutputToolName
	}
	mappedName := mapKiroToolName(toolName, requestCtx)
	if mappedName == "" {
		return nil, ""
	}
	requestCtx.StructuredOutputToolName = mappedName
	requestCtx.StructuredOutputUserHint = fmt.Sprintf("[CRITICAL] You MUST call the '%s' tool now with the structured JSON answer. Do NOT output plain text. Do NOT wrap the JSON in markdown.", mappedName)
	if claudeBodyHasToolNamed(claudeBody, toolName, mappedName, requestCtx) {
		return nil, fmt.Sprintf("[INSTRUCTION: You MUST respond by calling the '%s' tool with the structured JSON answer. Do not output plain text.]", mappedName)
	}
	return &KiroToolWrapper{
		ToolSpecification: KiroToolSpecification{
			Name:        mappedName,
			Description: "Output the result as structured JSON. You MUST call this tool with your answer.",
			InputSchema: KiroInputSchema{JSON: normalizeKiroJSONSchema(schema.Value())},
		},
	}, fmt.Sprintf("[INSTRUCTION: You MUST respond by calling the '%s' tool with the structured JSON answer. Do not output plain text.]", mappedName)
}

func extractStructuredOutputFormat(claudeBody []byte) (gjson.Result, bool) {
	for _, path := range []string{"output_config.format", "output_format", "response_format"} {
		value := gjson.GetBytes(claudeBody, path)
		if value.Exists() {
			return value, true
		}
	}
	return gjson.Result{}, false
}

func claudeBodyHasToolNamed(claudeBody []byte, originalName, mappedName string, requestCtx *KiroRequestContext) bool {
	tools := gjson.GetBytes(claudeBody, "tools")
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		name := strings.TrimSpace(tool.Get("name").String())
		if name == "" {
			name = strings.TrimSpace(tool.Get("type").String())
		}
		if name == originalName || mapKiroToolName(name, requestCtx) == mappedName {
			return true
		}
	}
	return false
}

func firstExistingJSON(values ...gjson.Result) gjson.Result {
	for _, value := range values {
		if value.Exists() {
			return value
		}
	}
	return gjson.Result{}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func isToolChoiceNone(claudeBody []byte) bool {
	toolChoice := gjson.GetBytes(claudeBody, "tool_choice")
	if !toolChoice.Exists() {
		return false
	}
	if toolChoice.Type == gjson.String {
		return strings.EqualFold(strings.TrimSpace(toolChoice.String()), "none")
	}
	return strings.EqualFold(strings.TrimSpace(toolChoice.Get("type").String()), "none")
}

func prependSystemHistory(history []KiroHistoryMessage, systemPrompt, modelID, origin string) []KiroHistoryMessage {
	systemPrompt = strings.TrimSpace(systemPrompt)
	if systemPrompt == "" {
		return history
	}

	prefix := []KiroHistoryMessage{
		{
			UserInputMessage: &KiroUserInputMessage{
				Content: systemPrompt,
				ModelID: modelID,
				Origin:  origin,
			},
		},
		{
			AssistantResponseMessage: &KiroAssistantResponseMessage{
				Content: "I will follow these instructions.",
			},
		},
	}

	return append(prefix, history...)
}

func normalizeOrigin(origin string) string {
	switch origin {
	case "KIRO_CLI", "AMAZON_Q":
		return "CLI"
	case "KIRO_AI_EDITOR", "KIRO_IDE", "":
		return "AI_EDITOR"
	default:
		return origin
	}
}

func convertClaudeToolsToKiro(tools gjson.Result, requestCtx *KiroRequestContext) []KiroToolWrapper {
	if !tools.IsArray() {
		return nil
	}
	var out []KiroToolWrapper
	for _, tool := range tools.Array() {
		originalName := tool.Get("name").String()
		if strings.TrimSpace(originalName) == "" {
			originalName = tool.Get("type").String()
		}
		isWebSearch := strings.TrimSpace(originalName) == "web_search"
		name := mapKiroToolName(originalName, requestCtx)
		description := strings.TrimSpace(tool.Get("description").String())
		if isWebSearch {
			if cached := GetCachedWebSearchDescription(); cached != "" {
				description = cached
			} else {
				description = remoteWebSearchDescription
			}
		}
		if description == "" {
			description = "Tool: " + name
		}
		description = appendChunkedToolDescription(originalName, description)
		description = truncateKiroToolDescription(description)
		inputSchema := normalizeKiroJSONSchema(tool.Get("input_schema").Value())
		out = append(out, KiroToolWrapper{
			ToolSpecification: KiroToolSpecification{
				Name:        name,
				Description: description,
				InputSchema: KiroInputSchema{JSON: inputSchema},
			},
		})
	}
	return out
}

func appendChunkedToolDescription(name, description string) string {
	suffix := chunkedToolDescriptionSuffix(name)
	if suffix == "" {
		return description
	}
	description = strings.Replace(description, suffix, "", 1)
	if strings.TrimSpace(description) == "" {
		return suffix
	}
	base := strings.TrimRight(description, "\n")
	joined := base + "\n" + suffix
	if len(joined) <= kiroMaxToolDescLen {
		return joined
	}
	const truncationMarker = "... (description truncated)"
	baseLimit := kiroMaxToolDescLen - len(suffix) - 1 - len(truncationMarker)
	if baseLimit <= 0 {
		return truncateKiroToolDescription(joined)
	}
	return truncateUTF8(base, baseLimit) + truncationMarker + "\n" + suffix
}

func chunkedToolDescriptionSuffix(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "write", "write_to_file", "fswrite", "create_file":
		return writeToolDescriptionSuffix
	case "edit", "edit_file", "str_replace_editor", "apply_diff":
		return editToolDescriptionSuffix
	default:
		return ""
	}
}

func truncateKiroToolDescription(description string) string {
	if len(description) <= kiroMaxToolDescLen {
		return description
	}
	return truncateUTF8(description, kiroMaxToolDescLen-30) + "... (description truncated)"
}

func truncateUTF8(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

func tailUTF8(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	start := len(s) - limit
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

func compactKiroToolResultText(text string, isError bool) string {
	if isError || len(text) <= kiroToolResultCompactLimit {
		return text
	}
	head := truncateUTF8(text, kiroToolResultKeepHead)
	tail := tailUTF8(text, kiroToolResultKeepTail)
	omitted := utf8.RuneCountInString(text) - utf8.RuneCountInString(head) - utf8.RuneCountInString(tail)
	if omitted < 0 {
		omitted = 0
	}
	return head + fmt.Sprintf("\n\n[Output truncated for Kiro context: original chars=%d, omitted chars=%d]\n\n", utf8.RuneCountInString(text), omitted) + tail
}

func newClaudeMessageID() string {
	return "msg_01" + randomBase62(25)
}

func newClaudeRequestID() string {
	return "req_01" + randomBase62(25)
}

func NewClaudeRequestID() string {
	return newClaudeRequestID()
}

func randomBase62(n int) string {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		sum := sha256.Sum256([]byte(uuid.NewString()))
		for i := range b {
			b[i] = sum[i%len(sum)]
		}
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// shortenToolNameIfNeeded 只按长度收敛，不处理字符集。
// 完整的工具名映射请走 mapKiroToolName —— 它会先清洗字符集再收长度。
func shortenToolNameIfNeeded(name string) string {
	name = strings.TrimSpace(name)
	if len(name) <= kiroMaxToolNameLen {
		return name
	}
	return appendToolNameSuffix(name, toolNameHashSuffix(name))
}

// sanitizeToolNameCharset 把工具名收敛到上游允许的字符集。
//
// AWS 官方服务模型（aws/aws-toolkit-vscode 的 user-service-2.json）对 ToolName 的约束是
// pattern "[a-zA-Z0-9_-]+"、max 64。含非法字符时上游拒的是「整个请求」（400），
// 不是只忽略那个工具。实测故障样本：$WEB_SEARCH、$MUTLI_1.N.1-Read。
//
// 注意连字符是「合法」的，不要替换掉 —— 否则会无谓改写大量本来合法的 MCP 工具名。
//
// 返回 changed 标记以便调用方决定是否需要加哈希后缀保证单射：清洗是多对一的
// （a.b 与 a-b 都会变成 a_b），而响应侧要靠 ToolNameMap 反查还原原始名。
func sanitizeToolNameCharset(name string) (string, bool) {
	changed := false
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			_, _ = b.WriteRune(r)
		default:
			_ = b.WriteByte('_')
			changed = true
		}
	}
	return b.String(), changed
}

// toolNameHashSuffix 取原始名的 sha256 前 8 位十六进制，用于防碰撞。
func toolNameHashSuffix(name string) string {
	sum := sha256.Sum256([]byte(name))
	return fmt.Sprintf("%x", sum[:])[:8]
}

// appendToolNameSuffix 在不超过长度上限的前提下追加后缀。
func appendToolNameSuffix(base, suffix string) string {
	maxPrefix := kiroMaxToolNameLen - 1 - len(suffix)
	if maxPrefix < 0 {
		maxPrefix = 0
	}
	if len(base) > maxPrefix {
		base = base[:maxPrefix]
	}
	return base + "_" + suffix
}

func mapKiroToolName(name string, requestCtx *KiroRequestContext) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if name == "web_search" {
		return "remote_web_search"
	}

	// 先收字符集，再收长度 —— 顺序不能反，否则截断后残留的非法字符仍会触发 400。
	sanitized, charsetChanged := sanitizeToolNameCharset(name)
	short := sanitized
	// 哈希后缀一律取「原始名」，保证清洗与截断这两条多对一的变换合起来仍是单射。
	if len(sanitized) > kiroMaxToolNameLen || charsetChanged {
		short = appendToolNameSuffix(sanitized, toolNameHashSuffix(name))
	}

	if short != name && requestCtx != nil {
		if requestCtx.ToolNameMap == nil {
			requestCtx.ToolNameMap = make(map[string]string)
		}
		requestCtx.ToolNameMap[short] = name
	}
	return short
}

func normalizeKiroJSONSchema(schema any) any {
	return normalizeKiroJSONSchemaValue(schema, true)
}

// kiroAllowedSchemaKeys 是上游 Smithy 校验接受的 JSON Schema 键白名单。
//
// Kiro 上游用 Smithy 校验工具 input_schema，比 Anthropic 严格得多：碰到
// draft-2020-12 的 $schema / additionalProperties / default / format /
// exclusiveMinimum / propertyNames 等关键字会拒掉「整个请求」（400），
// 而不是忽略该字段。Claude Code 的 MCP 工具 schema 普遍带这些键，属高发场景。
//
// 参考：funny-vibes/agent-vibes translator.ts:898-906（白名单 + 重建对象）。
//
// 代价：minimum/maximum/pattern 等约束被剥离后，模型可能生成越界参数。
// 社区一致接受这个代价——「参数可能越界」远好过「整个请求 400」。
var kiroAllowedSchemaKeys = map[string]struct{}{
	"type":        {},
	"description": {},
	"properties":  {},
	"required":    {},
	"items":       {},
	"enum":        {},
	"title":       {},
}

func normalizeKiroJSONSchemaValue(schema any, enforceObjectKeywords bool) any {
	obj, ok := schema.(map[string]any)
	if !ok || obj == nil {
		return defaultKiroJSONSchema()
	}
	obj = flattenKiroSchemaComposition(obj, 0)
	// 重建而非删键：保证任意嵌套深度（properties.*、items）都不残留超纲关键字。
	normalized := make(map[string]any, len(obj)+4)
	for key, value := range obj {
		if _, allowed := kiroAllowedSchemaKeys[key]; !allowed {
			// const 是 draft-2020-12 关键字，但语义可用单元素 enum 无损表达。
			if key == "const" && !hasSchemaKey(obj, "enum") {
				normalized["enum"] = []any{value}
			}
			continue
		}
		normalized[key] = normalizeSchemaChild(key, value)
	}
	// 根 schema（enforceObjectKeywords）必须是 object，不做推断。
	if typ := resolveKiroSchemaType(normalized, !enforceObjectKeywords); typ != "" {
		normalized["type"] = typ
	} else {
		normalized["type"] = "object"
	}
	typ, _ := normalized["type"].(string)
	// additionalProperties 已被白名单剔除，这里要看「原始」schema 是否带过它，
	// 否则无法据此判断作者本意是不是一个 object。
	needsObjectKeywords := enforceObjectKeywords ||
		strings.TrimSpace(typ) == "object" ||
		hasSchemaKey(normalized, "properties") ||
		hasSchemaKey(normalized, "required") ||
		hasSchemaKey(obj, "additionalProperties")
	if needsObjectKeywords {
		properties, ok := normalized["properties"].(map[string]any)
		if !ok || properties == nil {
			normalized["properties"] = map[string]any{}
		} else {
			for key, value := range properties {
				properties[key] = normalizeKiroJSONSchemaValue(value, false)
			}
			normalized["properties"] = properties
		}
		// required 为空时移除整个键，而不是留一个 []。
		// 参考 AbdoKnbGit/tau request.ts:272-275：空数组本身就会触发 400。
		if required := normalizeSchemaRequired(normalized["required"]); len(required) > 0 {
			normalized["required"] = required
		} else {
			delete(normalized, "required")
		}
		// 不再主动补 additionalProperties —— 它不在 Smithy 接受的键集合里。
	}
	return normalized
}

// kiroSchemaCompositionMaxDepth 防御自引用式的超深组合嵌套。
const kiroSchemaCompositionMaxDepth = 8

// flattenKiroSchemaComposition 把 anyOf/oneOf/allOf 折叠成单一 schema。
//
// 白名单不含组合关键字，直接剔除会让 Optional 字段
// （Pydantic/zod 生成的 anyOf:[{type:string},{type:null}]）丢掉类型，
// 随后被兜底成 {type:object}，模型就会按对象传参。
// anyOf/oneOf 取第一个非 null 分支；allOf 合并各分支的 properties 与 required。
// 外层已有的键（description、title 等）优先于分支。
func flattenKiroSchemaComposition(obj map[string]any, depth int) map[string]any {
	if depth >= kiroSchemaCompositionMaxDepth {
		return obj
	}
	hasComposition := false
	for _, key := range [...]string{"anyOf", "oneOf", "allOf"} {
		if _, ok := obj[key]; ok {
			hasComposition = true
			break
		}
	}
	if !hasComposition {
		return obj
	}

	out := make(map[string]any, len(obj))
	for key, value := range obj {
		if key != "anyOf" && key != "oneOf" && key != "allOf" {
			out[key] = value
		}
	}
	fillFrom := func(branch map[string]any) {
		branch = flattenKiroSchemaComposition(branch, depth+1)
		for key, value := range branch {
			if _, exists := out[key]; !exists {
				out[key] = value
			}
		}
	}

	if branches, ok := obj["allOf"].([]any); ok {
		mergedProps := map[string]any{}
		var mergedRequired []any
		if props, ok := out["properties"].(map[string]any); ok {
			for k, v := range props {
				mergedProps[k] = v
			}
		}
		if req, ok := out["required"].([]any); ok {
			mergedRequired = append(mergedRequired, req...)
		}
		for _, raw := range branches {
			branch, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			branch = flattenKiroSchemaComposition(branch, depth+1)
			if props, ok := branch["properties"].(map[string]any); ok {
				for k, v := range props {
					if _, exists := mergedProps[k]; !exists {
						mergedProps[k] = v
					}
				}
			}
			if req, ok := branch["required"].([]any); ok {
				mergedRequired = append(mergedRequired, req...)
			}
			fillFrom(branch)
		}
		if len(mergedProps) > 0 {
			out["properties"] = mergedProps
		}
		if len(mergedRequired) > 0 {
			out["required"] = mergedRequired
		}
	}

	for _, key := range [...]string{"anyOf", "oneOf"} {
		branches, ok := obj[key].([]any)
		if !ok {
			continue
		}
		for _, raw := range branches {
			branch, ok := raw.(map[string]any)
			if !ok || isKiroNullSchema(branch) {
				continue
			}
			fillFrom(branch)
			break
		}
	}
	return out
}

func isKiroNullSchema(schema map[string]any) bool {
	typ, ok := schema["type"].(string)
	return ok && typ == "null"
}

// resolveKiroSchemaType 求出单一的 type 字符串；无法确定时返回空串。
//
// type 为数组（["string","null"]）时取第一个非 null 值；缺省且 infer 为真时按
// enum 首值或 items 推断，避免把枚举/数组字段兜底成 object。
func resolveKiroSchemaType(schema map[string]any, infer bool) string {
	switch typ := schema["type"].(type) {
	case string:
		if t := strings.TrimSpace(typ); t != "" {
			return t
		}
	case []any:
		first := ""
		for _, item := range typ {
			name, ok := item.(string)
			if !ok || strings.TrimSpace(name) == "" {
				continue
			}
			if first == "" {
				first = name
			}
			if name != "null" {
				return name
			}
		}
		if first != "" {
			return first
		}
	}
	if !infer {
		return ""
	}
	if values, ok := schema["enum"].([]any); ok && len(values) > 0 {
		switch values[0].(type) {
		case string:
			return "string"
		case bool:
			return "boolean"
		case float64, json.Number, int, int64:
			return "number"
		}
	}
	if _, ok := schema["items"]; ok {
		return "array"
	}
	return ""
}

func hasSchemaKey(schema map[string]any, key string) bool {
	_, ok := schema[key]
	return ok
}

func defaultKiroJSONSchema() map[string]any {
	// 不含 required: [] 和 additionalProperties —— 两者都是已知的 400 触发器。
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func normalizeSchemaRequired(value any) []any {
	arr, ok := value.([]any)
	if !ok {
		return []any{}
	}
	out := make([]any, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// normalizeSchemaChild 只需处理 items —— not/oneOf/anyOf/allOf 已被
// kiroAllowedSchemaKeys 白名单剔除，不会走到这里。
func normalizeSchemaChild(key string, value any) any {
	if key != "items" {
		return value
	}
	if obj, ok := value.(map[string]any); ok {
		return normalizeKiroJSONSchemaValue(obj, false)
	}
	if arr, ok := value.([]any); ok {
		out := make([]any, 0, len(arr))
		for _, item := range arr {
			out = append(out, normalizeKiroJSONSchemaValue(item, false))
		}
		return out
	}
	return value
}

func processMessages(messages []gjson.Result, modelID, origin string, requestCtx *KiroRequestContext) ([]KiroHistoryMessage, *KiroUserInputMessage, []KiroToolResult) {
	messagesArray := mergeAdjacentMessages(messages)

	var history []KiroHistoryMessage
	var currentUserMsg *KiroUserInputMessage
	var currentToolResults []KiroToolResult

	for i, msg := range messagesArray {
		role := msg.Get("role").String()
		last := i == len(messagesArray)-1
		switch role {
		case "user":
			keepImages := last || len(messagesArray)-1-i <= kiroHistoryImageKeepCount
			userMsg, toolResults := buildUserMessageStruct(msg, modelID, origin, keepImages)
			if strings.TrimSpace(userMsg.Content) == "" {
				if len(toolResults) > 0 {
					userMsg.Content = "Tool results provided."
				} else {
					userMsg.Content = "Continue"
				}
			}
			if last {
				currentUserMsg = &userMsg
				currentToolResults = toolResults
			} else {
				if len(toolResults) > 0 {
					userMsg.UserInputMessageContext = &KiroUserInputMessageContext{ToolResults: toolResults}
				}
				history = append(history, KiroHistoryMessage{UserInputMessage: &userMsg})
			}
		case "assistant":
			assistantMsg := buildAssistantMessageStruct(msg, requestCtx)
			if last {
				history = append(history, KiroHistoryMessage{AssistantResponseMessage: &assistantMsg})
				currentUserMsg = &KiroUserInputMessage{
					Content: "Continue",
					ModelID: modelID,
					Origin:  origin,
				}
			} else {
				history = append(history, KiroHistoryMessage{AssistantResponseMessage: &assistantMsg})
			}
		}
	}

	return history, currentUserMsg, currentToolResults
}

// validateToolPairing 按"紧邻上一轮"收敛 tool 配对。
//
// Kiro 校验的是相邻关系：每条 user 消息的 toolResult 只能回应紧挨着的上一条
// assistant 的 toolUse（400："The number of toolResult blocks at messages.N.content
// exceeds the number of toolUse blocks of previous turn"）。过去用全历史 ID 集合判断，
// 引用更早轮次 toolUse 的结果会通过本地校验、到上游才 400。
//
// 处理：
//   - history 中 user 消息的 toolResult 不属于上一条 assistant 时就地剔除；
//   - 当前轮 toolResult 只接受 history 末条 assistant 的 toolUse；
//   - 没有被紧邻下一轮回应的 toolUse 记为孤儿，由 removeOrphanedToolUses 清理。
func validateToolPairing(history []KiroHistoryMessage, currentToolResults []KiroToolResult) ([]KiroToolResult, map[string]bool) {
	orphaned := make(map[string]bool)
	// pending 是上一条 assistant 发出、尚未被回应的 toolUse。
	pending := map[string]bool{}
	flushPending := func() {
		for id := range pending {
			orphaned[id] = true
		}
		pending = map[string]bool{}
	}

	for i := range history {
		if msg := history[i].AssistantResponseMessage; msg != nil {
			flushPending()
			for _, tu := range msg.ToolUses {
				pending[tu.ToolUseID] = true
			}
			continue
		}
		msg := history[i].UserInputMessage
		if msg == nil {
			continue
		}
		if msg.UserInputMessageContext != nil && len(msg.UserInputMessageContext.ToolResults) > 0 {
			kept := msg.UserInputMessageContext.ToolResults[:0]
			for _, tr := range msg.UserInputMessageContext.ToolResults {
				if pending[tr.ToolUseID] {
					kept = append(kept, tr)
					delete(pending, tr.ToolUseID)
				}
			}
			msg.UserInputMessageContext.ToolResults = kept
		}
		flushPending()
	}

	filtered := currentToolResults[:0]
	for _, tr := range currentToolResults {
		if pending[tr.ToolUseID] {
			filtered = append(filtered, tr)
			delete(pending, tr.ToolUseID)
		}
	}
	flushPending()
	return filtered, orphaned
}

func removeOrphanedToolUses(history []KiroHistoryMessage, orphaned map[string]bool) {
	if len(orphaned) == 0 {
		return
	}
	for i := range history {
		msg := history[i].AssistantResponseMessage
		if msg == nil || len(msg.ToolUses) == 0 {
			continue
		}
		filtered := msg.ToolUses[:0]
		for _, toolUse := range msg.ToolUses {
			if !orphaned[toolUse.ToolUseID] {
				filtered = append(filtered, toolUse)
			}
		}
		msg.ToolUses = filtered
	}
}

// removeOrphanedToolResults 清理"有 toolResult、但配对 toolUse 已不在 history 里"的残留。
//
// 这是 removeOrphanedToolUses 的镜像方向，两者缺一不可：
// 前者处理"调用了工具却没有结果"，本函数处理"有结果却找不到调用"。
//
// 属纵深防御：nextKiroHistoryCutPoint 会主动避开带 toolResult 的 user 消息，
// 因此常规形态下 history 内部不会产生这种孤儿（线上 400 的真正成因是
// 当前轮失配，见 removeOrphanedCurrentToolResults）。但 alignKiroHistoryToUser
// 会无条件丢弃开头的 Assistant 消息，切点形态一变就可能打破配对。
//
// 不清理的后果是上游直接 400：
//
//	The number of toolResult blocks at messages.N.content exceeds
//	the number of toolUse blocks of previous turn.
//
// 返回被移除的数量，便于调用方决定是否需要继续收敛。
func removeOrphanedToolResults(history []KiroHistoryMessage) int {
	liveToolUseIDs := make(map[string]bool)
	for _, h := range history {
		if h.AssistantResponseMessage == nil {
			continue
		}
		for _, tu := range h.AssistantResponseMessage.ToolUses {
			liveToolUseIDs[tu.ToolUseID] = true
		}
	}

	removed := 0
	for i := range history {
		msg := history[i].UserInputMessage
		if msg == nil || msg.UserInputMessageContext == nil {
			continue
		}
		results := msg.UserInputMessageContext.ToolResults
		if len(results) == 0 {
			continue
		}
		filtered := results[:0]
		for _, tr := range results {
			if liveToolUseIDs[tr.ToolUseID] {
				filtered = append(filtered, tr)
				continue
			}
			removed++
		}
		msg.UserInputMessageContext.ToolResults = filtered
	}
	return removed
}

// collectToolUseNames 采集 history 里所有 toolUseID -> 工具名的映射。
// 供裁剪后重建 toolUse 使用, 必须在裁剪前调用。
func collectToolUseNames(history []KiroHistoryMessage) map[string]string {
	namesByID := make(map[string]string)
	for _, h := range history {
		if h.AssistantResponseMessage == nil {
			continue
		}
		for _, tu := range h.AssistantResponseMessage.ToolUses {
			namesByID[tu.ToolUseID] = tu.Name
		}
	}
	return namesByID
}

// removeOrphanedCurrentToolResults 修复"当前轮 toolResult 的配对 toolUse 已被裁掉"。
//
// 与 history 内部的孤儿不同, 当前轮的 toolResult 是模型正在推理的依据,
// 直接删掉等于让它凭空失去刚拿到的工具输出。因此这里**优先补回 toolUse**:
// 在裁剪后的 history 末尾追加一条只含该 toolUse 的 Assistant 消息,
// 既满足上游"toolResult 必须有前序 toolUse"的约束, 代价也远小于丢结果。
//
// 只有在连工具名都无从得知时(理论上不会发生, 兜底防御)才退而删除该 toolResult。
//
// namesByID 必须由调用方在**任何裁剪发生之前**采集并全程复用:
// 被裁掉的 toolUse 已经不在 state.History 里了, 事后再查必然查不到,
// 结果就是本该补回的 toolUse 退化成"丢弃 toolResult"。
//
// 返回是否修改过 payload。
func removeOrphanedCurrentToolResults(state *KiroConversationState, history []KiroHistoryMessage, namesByID map[string]string) bool {
	ctx := state.CurrentMessage.UserInputMessage.UserInputMessageContext
	if ctx == nil || len(ctx.ToolResults) == 0 {
		return false
	}

	liveToolUseIDs := make(map[string]bool)
	for _, h := range history {
		if h.AssistantResponseMessage == nil {
			continue
		}
		for _, tu := range h.AssistantResponseMessage.ToolUses {
			liveToolUseIDs[tu.ToolUseID] = true
		}
	}

	var restored []KiroToolUse
	kept := ctx.ToolResults[:0]
	changed := false
	for _, tr := range ctx.ToolResults {
		if liveToolUseIDs[tr.ToolUseID] {
			kept = append(kept, tr)
			continue
		}
		name := namesByID[tr.ToolUseID]
		if name == "" {
			// 兜底: 无法重建就只能丢弃, 否则上游必然 400。
			changed = true
			continue
		}
		restored = append(restored, KiroToolUse{ToolUseID: tr.ToolUseID, Name: name, Input: map[string]any{}})
		kept = append(kept, tr)
		changed = true
	}
	ctx.ToolResults = kept

	if len(restored) > 0 {
		state.History = append(history, KiroHistoryMessage{
			AssistantResponseMessage: &KiroAssistantResponseMessage{
				Content:  "",
				ToolUses: restored,
			},
		})
	}
	return changed
}

func collectHistoryToolNames(history []KiroHistoryMessage) []string {
	seen := make(map[string]bool)
	var names []string
	for _, h := range history {
		if h.AssistantResponseMessage == nil {
			continue
		}
		for _, tu := range h.AssistantResponseMessage.ToolUses {
			name := strings.TrimSpace(tu.Name)
			if name == "" {
				continue
			}
			key := strings.ToLower(name)
			if seen[key] {
				continue
			}
			seen[key] = true
			names = append(names, name)
		}
	}
	return names
}

func appendMissingPlaceholderTools(tools []KiroToolWrapper, historyToolNames []string) []KiroToolWrapper {
	if len(historyToolNames) == 0 {
		return tools
	}
	seen := make(map[string]bool)
	for _, tool := range tools {
		seen[strings.ToLower(strings.TrimSpace(tool.ToolSpecification.Name))] = true
	}
	for _, name := range historyToolNames {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		tools = append(tools, KiroToolWrapper{
			ToolSpecification: KiroToolSpecification{
				Name:        name,
				Description: "Tool used in conversation history",
				InputSchema: KiroInputSchema{JSON: normalizeKiroJSONSchema(nil)},
			},
		})
	}
	return tools
}

func buildFinalContent(content string, toolResults []KiroToolResult) string {
	if strings.TrimSpace(content) == "" {
		if len(toolResults) > 0 {
			return "Tool results provided."
		}
		return "Continue"
	}
	return content
}

func appendTextBlock(content, extra string) string {
	extra = strings.TrimSpace(extra)
	if extra == "" {
		return content
	}
	if strings.TrimSpace(content) == "" {
		return extra
	}
	return strings.TrimRight(content, "\n") + "\n\n" + extra
}

func deduplicateToolResults(toolResults []KiroToolResult) []KiroToolResult {
	seen := make(map[string]bool)
	out := make([]KiroToolResult, 0, len(toolResults))
	for _, tr := range toolResults {
		if seen[tr.ToolUseID] {
			continue
		}
		seen[tr.ToolUseID] = true
		out = append(out, tr)
	}
	return out
}

func buildUserMessageStruct(msg gjson.Result, modelID, origin string, keepImages bool) (KiroUserInputMessage, []KiroToolResult) {
	content := msg.Get("content")
	var contentBuilder strings.Builder
	var toolResults []KiroToolResult
	var images []KiroImage
	omittedImageCount := 0
	seenToolUseIDs := make(map[string]bool)

	if content.IsArray() {
		for _, part := range content.Array() {
			switch part.Get("type").String() {
			case "text":
				_, _ = contentBuilder.WriteString(part.Get("text").String())
			case "image":
				mediaType := part.Get("source.media_type").String()
				data := part.Get("source.data").String()
				image, ok := buildKiroImage(mediaType, data)
				if !ok {
					if url := strings.TrimSpace(part.Get("source.url").String()); url != "" {
						appendImageURLFallback(&contentBuilder, url)
					}
					continue
				}
				if keepImages {
					images = append(images, image)
				} else {
					omittedImageCount++
				}
			case "image_url", "input_image":
				url := strings.TrimSpace(part.Get("image_url.url").String())
				if url == "" {
					url = strings.TrimSpace(part.Get("image_url").String())
				}
				if url == "" {
					url = strings.TrimSpace(part.Get("source.url").String())
				}
				if image, ok := buildKiroImageFromURL(url); ok {
					if keepImages {
						images = append(images, image)
					} else {
						omittedImageCount++
					}
				} else if strings.HasPrefix(strings.ToLower(url), "http://") || strings.HasPrefix(strings.ToLower(url), "https://") {
					appendImageURLFallback(&contentBuilder, url)
				}
			case "document":
				fallbackText := buildDocumentTextFallback(part)
				if fallbackText == "" {
					continue
				}
				currentText := contentBuilder.String()
				contentBuilder.Reset()
				_, _ = contentBuilder.WriteString(appendTextBlock(currentText, fallbackText))
			case "tool_result":
				toolUseID := part.Get("tool_use_id").String()
				if toolUseID == "" || seenToolUseIDs[toolUseID] {
					continue
				}
				seenToolUseIDs[toolUseID] = true
				status := "success"
				if part.Get("is_error").Bool() {
					status = "error"
				}
				textContents := []KiroTextContent{{Text: "Tool use was cancelled by the user"}}
				resultContent := part.Get("content")
				if resultContent.IsArray() {
					textContents = textContents[:0]
					for _, item := range resultContent.Array() {
						switch t := item.Get("type").String(); {
						// codex 经 responses->anthropic 后, tool_result.content 用 Responses 的
						// "input_text" 而非 Anthropic 的 "text"; 两者都需提取, 否则工具结果被丢成空。
						case t == "text" || t == "input_text":
							textContents = append(textContents, KiroTextContent{Text: compactKiroToolResultText(item.Get("text").String(), status == "error")})
						case item.Type == gjson.String:
							textContents = append(textContents, KiroTextContent{Text: compactKiroToolResultText(item.String(), status == "error")})
						case t == "image":
							// Kiro 的 toolResult 只承载文本；图片挂到同一条 user 消息的 images 上，
							// 否则 Claude Code 用 Read 读图时模型看不到图，会编造内容。
							image, ok := buildKiroImage(item.Get("source.media_type").String(), item.Get("source.data").String())
							if !ok {
								image, ok = buildKiroImageFromURL(item.Get("source.url").String())
							}
							switch {
							case ok && keepImages:
								images = append(images, image)
								textContents = append(textContents, KiroTextContent{Text: kiroToolResultImageAttached})
							case ok:
								omittedImageCount++
								textContents = append(textContents, KiroTextContent{Text: kiroToolResultImageOmitted})
							default:
								textContents = append(textContents, KiroTextContent{Text: kiroToolResultImageUnsupported})
							}
						case t == "document":
							if text := buildDocumentTextFallback(item); text != "" {
								textContents = append(textContents, KiroTextContent{Text: compactKiroToolResultText(text, status == "error")})
							}
						case t == "search_result":
							if text := renderKiroSearchResultText(item); text != "" {
								textContents = append(textContents, KiroTextContent{Text: compactKiroToolResultText(text, status == "error")})
							}
						}
					}
				} else if resultContent.Type == gjson.String {
					textContents = []KiroTextContent{{Text: compactKiroToolResultText(resultContent.String(), status == "error")}}
				}
				if !hasNonEmptyKiroText(textContents) {
					// 空文本的 toolResult 会被上游拒绝，且模型无法区分"无输出"与"丢失"。
					textContents = []KiroTextContent{{Text: kiroEmptyToolResultPlaceholder}}
				}
				toolResults = append(toolResults, KiroToolResult{
					ToolUseID: toolUseID,
					Content:   textContents,
					Status:    status,
				})
			}
		}
	} else {
		_, _ = contentBuilder.WriteString(content.String())
	}

	if omittedImageCount > 0 {
		placeholder := fmt.Sprintf(omittedHistoryImageFormat, omittedImageCount)
		if strings.TrimSpace(contentBuilder.String()) == "" {
			_, _ = contentBuilder.WriteString(placeholder)
		} else {
			_, _ = contentBuilder.WriteString("\n")
			_, _ = contentBuilder.WriteString(placeholder)
		}
	}

	userMsg := KiroUserInputMessage{
		Content: contentBuilder.String(),
		ModelID: modelID,
		Origin:  origin,
	}
	if len(images) > 0 {
		userMsg.Images = images
		if strings.TrimSpace(userMsg.Content) == "" {
			userMsg.Content = " "
		}
	}
	return userMsg, toolResults
}

const (
	kiroToolResultImageAttached    = "[Image returned by the tool is attached to this message]"
	kiroToolResultImageOmitted     = "[Image returned by the tool was omitted from older history]"
	kiroToolResultImageUnsupported = "[Image returned by the tool could not be forwarded: unsupported format]"
	kiroEmptyToolResultPlaceholder = "(empty tool result)"
)

func hasNonEmptyKiroText(contents []KiroTextContent) bool {
	for _, c := range contents {
		if strings.TrimSpace(c.Text) != "" {
			return true
		}
	}
	return false
}

// renderKiroSearchResultText 把 Anthropic search_result 块渲染为带来源的纯文本。
func renderKiroSearchResultText(block gjson.Result) string {
	var sb strings.Builder
	if title := strings.TrimSpace(block.Get("title").String()); title != "" {
		_, _ = sb.WriteString(title)
		_, _ = sb.WriteString("\n")
	}
	if source := strings.TrimSpace(block.Get("source").String()); source != "" {
		_, _ = sb.WriteString("Source: ")
		_, _ = sb.WriteString(source)
		_, _ = sb.WriteString("\n")
	}
	content := block.Get("content")
	if content.IsArray() {
		for _, item := range content.Array() {
			if text := item.Get("text").String(); strings.TrimSpace(text) != "" {
				_, _ = sb.WriteString(text)
				_, _ = sb.WriteString("\n")
			}
		}
	} else if text := content.String(); strings.TrimSpace(text) != "" {
		_, _ = sb.WriteString(text)
	}
	return strings.TrimSpace(sb.String())
}

func buildKiroImage(mediaType, data string) (KiroImage, bool) {
	if image, ok := buildKiroImageFromURL(data); ok {
		return image, true
	}
	format := ""
	if idx := strings.LastIndex(mediaType, "/"); idx != -1 {
		format = mediaType[idx+1:]
	}
	format = normalizeKiroImageFormat(format)
	data = strings.TrimSpace(data)
	if data == "" {
		return KiroImage{}, false
	}
	// 按文件头纠正声明的格式：客户端常把 PNG 截图标成 image/jpeg，
	// 上游据声明格式解码会报 IMAGE_FORMAT_UNSUPPORTED 并拒掉整个请求。
	if sniffed, decodable := sniffKiroBase64ImageFormat(data); !decodable {
		return KiroImage{}, false
	} else if sniffed != "" {
		format = sniffed
	}
	if format == "" {
		return KiroImage{}, false
	}
	return KiroImage{
		Format: format,
		Source: KiroImageSource{Bytes: data},
	}, true
}

// sniffKiroBase64ImageFormat 只解码 base64 开头一小段，按 magic bytes 识别格式。
// decodable=false 表示数据不是合法 base64，应当丢弃而不是原样发给上游；
// 格式为空表示无法识别，调用方回退到声明的 media_type。
func sniffKiroBase64ImageFormat(data string) (format string, decodable bool) {
	const prefixChars = 32 // 解码后 24 字节，覆盖 WEBP 的 12 字节头
	prefix := data
	if len(prefix) > prefixChars {
		prefix = prefix[:prefixChars]
	}
	head, err := base64.StdEncoding.DecodeString(prefix)
	if err != nil {
		trimmed := strings.TrimRight(prefix, "=")
		head, err = base64.RawStdEncoding.DecodeString(trimmed)
		if err != nil {
			head, err = base64.RawURLEncoding.DecodeString(trimmed)
			if err != nil {
				return "", false
			}
		}
	}
	switch {
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return "png", true
	case bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF}):
		return "jpeg", true
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return "gif", true
	case len(head) >= 12 && bytes.Equal(head[0:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		return "webp", true
	default:
		return "", true
	}
}

func buildKiroImageFromURL(url string) (KiroImage, bool) {
	url = strings.TrimSpace(url)
	lowerURL := strings.ToLower(url)
	if strings.HasPrefix(lowerURL, "http://") || strings.HasPrefix(lowerURL, "https://") {
		return buildKiroImageFromRemoteURL(url)
	}
	if !strings.HasPrefix(lowerURL, "data:") {
		return KiroImage{}, false
	}
	comma := strings.IndexByte(url, ',')
	if comma <= len("data:") {
		return KiroImage{}, false
	}
	meta := url[len("data:"):comma]
	data := strings.TrimSpace(url[comma+1:])
	if !strings.Contains(strings.ToLower(meta), ";base64") {
		return KiroImage{}, false
	}
	mediaType := meta
	if semi := strings.IndexByte(mediaType, ';'); semi >= 0 {
		mediaType = mediaType[:semi]
	}
	return buildKiroImage(mediaType, data)
}

func buildKiroImageFromRemoteURL(url string) (KiroImage, bool) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return KiroImage{}, false
	}
	req.Header.Set("Accept", "image/*,*/*;q=0.8")
	resp, err := kiroRemoteImageHTTPClient.Do(req)
	if err != nil {
		return KiroImage{}, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return KiroImage{}, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, kiroRemoteImageMaxBytes+1))
	if err != nil || len(body) == 0 || len(body) > kiroRemoteImageMaxBytes {
		return KiroImage{}, false
	}
	mediaType := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	format := ""
	if strings.HasPrefix(strings.ToLower(mediaType), "image/") {
		format = normalizeKiroImageFormat(strings.TrimPrefix(strings.ToLower(mediaType), "image/"))
	}
	if format == "" {
		detected := strings.TrimSpace(strings.Split(http.DetectContentType(body), ";")[0])
		if strings.HasPrefix(strings.ToLower(detected), "image/") {
			format = normalizeKiroImageFormat(strings.TrimPrefix(strings.ToLower(detected), "image/"))
		}
	}
	if format == "" {
		return KiroImage{}, false
	}
	return KiroImage{
		Format: format,
		Source: KiroImageSource{Bytes: base64.StdEncoding.EncodeToString(body)},
	}, true
}

func normalizeKiroImageFormat(format string) string {
	format = strings.ToLower(strings.TrimSpace(format))
	if semi := strings.IndexByte(format, ';'); semi >= 0 {
		format = strings.TrimSpace(format[:semi])
	}
	if format == "jpg" {
		return "jpeg"
	}
	switch format {
	case "png", "jpeg", "webp", "gif":
		return format
	default:
		return ""
	}
}

func appendImageURLFallback(builder *strings.Builder, url string) {
	url = strings.TrimSpace(url)
	if url == "" {
		return
	}
	if strings.TrimSpace(builder.String()) != "" {
		_, _ = builder.WriteString("\n")
	}
	_, _ = builder.WriteString("[Image: ")
	_, _ = builder.WriteString(url)
	_, _ = builder.WriteString("]")
}

func buildDocumentTextFallback(part gjson.Result) string {
	source := part.Get("source")
	mediaType := strings.TrimSpace(source.Get("media_type").String())
	if mediaType == "" {
		mediaType = strings.TrimSpace(source.Get("mediaType").String())
	}
	if mediaType == "" {
		mediaType = strings.TrimSpace(part.Get("media_type").String())
	}
	if mediaType == "" {
		mediaType = strings.TrimSpace(part.Get("mime_type").String())
	}
	data := strings.TrimSpace(source.Get("data").String())
	if data == "" {
		data = strings.TrimSpace(part.Get("data").String())
	}
	if strings.HasPrefix(data, "data:") {
		if comma := strings.IndexByte(data, ','); comma > 0 {
			meta := data[len("data:"):comma]
			if semi := strings.IndexByte(meta, ';'); semi >= 0 {
				meta = meta[:semi]
			}
			if mediaType == "" {
				mediaType = meta
			}
			data = data[comma+1:]
		}
	}
	format := kiroDocumentFormat(mediaType)
	if format == "" || data == "" {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(source.Get("type").String()), "text") {
		data = base64.StdEncoding.EncodeToString([]byte(data))
	}
	name := strings.TrimSpace(part.Get("name").String())
	if name == "" {
		name = strings.TrimSpace(part.Get("title").String())
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(data)
	}
	if err != nil || len(raw) == 0 {
		return ""
	}
	// 文本类文档（txt/csv/html/json/md...）本身就是可读内容, 直接内联。
	// 之前这里对非 pdf 一律 return "", 导致 source.type="text" 的 document 块
	// 被静默丢弃 —— 上游收不到任何内容, 模型却照常作答, 表现为「文档没传进去」。
	if format != "pdf" {
		text := strings.TrimSpace(string(raw))
		if text == "" || !utf8.ValidString(text) {
			return ""
		}
		if utf8.RuneCountInString(text) > 6000 {
			text = truncateUTF8(text, 6000) + "\n[document text truncated]"
		}
		if name == "" {
			name = "document." + format
		}
		return fmt.Sprintf("[Attached document: %s, format=%s, bytes=%d]\n[Document content]\n%s\n[/Document content]", name, format, len(raw), text)
	}
	text := strings.TrimSpace(extractPDFTextLite(raw))
	if text == "" {
		return ""
	}
	if utf8.RuneCountInString(text) > 6000 {
		text = truncateUTF8(text, 6000) + "\n[PDF text truncated]"
	}
	sum := sha256.Sum256(raw)
	if name == "" {
		name = "document.pdf"
	}
	return fmt.Sprintf("[Attached PDF document: %s, bytes=%d, sha256=%x]\n[Extracted PDF text]\n%s\n[/Extracted PDF text]", name, len(raw), sum[:8], text)
}

func extractPDFTextLite(data []byte) string {
	var chunks [][]byte
	chunks = append(chunks, data)
	chunks = append(chunks, inflatePDFStreams(data)...)
	var lines []string
	seen := make(map[string]bool)
	for _, chunk := range chunks {
		for _, text := range extractPDFStrings(chunk) {
			text = strings.Join(strings.Fields(text), " ")
			if text == "" || seen[text] || !looksLikeReadableText(text) {
				continue
			}
			seen[text] = true
			lines = append(lines, text)
			if len(lines) >= 200 {
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}

func inflatePDFStreams(data []byte) [][]byte {
	var out [][]byte
	searchFrom := 0
	for {
		streamPos := bytes.Index(data[searchFrom:], []byte("stream"))
		if streamPos < 0 {
			break
		}
		streamPos += searchFrom + len("stream")
		if streamPos < len(data) && data[streamPos] == '\r' {
			streamPos++
		}
		if streamPos < len(data) && data[streamPos] == '\n' {
			streamPos++
		}
		endPosRel := bytes.Index(data[streamPos:], []byte("endstream"))
		if endPosRel < 0 {
			break
		}
		endPos := streamPos + endPosRel
		raw := bytes.TrimSpace(data[streamPos:endPos])
		if len(raw) > 0 {
			if reader, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
				if decoded, err := io.ReadAll(io.LimitReader(reader, 2<<20)); err == nil && len(decoded) > 0 {
					out = append(out, decoded)
				}
				_ = reader.Close()
			}
		}
		searchFrom = endPos + len("endstream")
	}
	return out
}

func extractPDFStrings(data []byte) []string {
	var out []string
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '(':
			text, next := readPDFLiteralString(data, i+1)
			if text != "" {
				out = append(out, text)
			}
			i = next
		case '<':
			if i+1 < len(data) && data[i+1] == '<' {
				continue
			}
			if text, next := readPDFHexString(data, i+1); next > i {
				if text != "" {
					out = append(out, text)
				}
				i = next
			}
		}
	}
	return out
}

func readPDFLiteralString(data []byte, pos int) (string, int) {
	var out []byte
	depth := 1
	for i := pos; i < len(data); i++ {
		ch := data[i]
		if ch == '\\' && i+1 < len(data) {
			i++
			next := data[i]
			switch next {
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case '(', ')', '\\':
				out = append(out, next)
			default:
				if next >= '0' && next <= '7' {
					val := int(next - '0')
					for j := 0; j < 2 && i+1 < len(data) && data[i+1] >= '0' && data[i+1] <= '7'; j++ {
						i++
						val = val*8 + int(data[i]-'0')
					}
					out = append(out, byte(val))
				} else {
					out = append(out, next)
				}
			}
			continue
		}
		if ch == '(' {
			depth++
		}
		if ch == ')' {
			depth--
			if depth == 0 {
				return decodePDFTextBytes(out), i
			}
		}
		out = append(out, ch)
	}
	return "", len(data)
}

func readPDFHexString(data []byte, pos int) (string, int) {
	end := pos
	for end < len(data) && data[end] != '>' {
		end++
	}
	if end >= len(data) {
		return "", pos
	}
	raw := make([]byte, 0, end-pos)
	for _, ch := range data[pos:end] {
		if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F') {
			raw = append(raw, ch)
		}
	}
	if len(raw)%2 == 1 {
		raw = append(raw, '0')
	}
	decoded := make([]byte, hex.DecodedLen(len(raw)))
	if _, err := hex.Decode(decoded, raw); err != nil {
		return "", end
	}
	return decodePDFTextBytes(decoded), end
}

func decodePDFTextBytes(data []byte) string {
	if len(data) >= 2 && data[0] == 0xFE && data[1] == 0xFF {
		runes := make([]rune, 0, (len(data)-2)/2)
		for i := 2; i+1 < len(data); i += 2 {
			runes = append(runes, rune(data[i])<<8|rune(data[i+1]))
		}
		return string(runes)
	}
	return strings.ToValidUTF8(string(data), "")
}

func looksLikeReadableText(text string) bool {
	runes := []rune(text)
	if len(runes) < 2 {
		return false
	}
	readable := 0
	for _, r := range runes {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) || strings.ContainsRune(".,;:!?，。；：！？()[]{}+-_/@#$%&*='\"<>", r) {
			readable++
		}
	}
	return readable*100/len(runes) >= 70
}

func kiroDocumentFormat(mediaType string) string {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if semi := strings.IndexByte(mediaType, ';'); semi >= 0 {
		mediaType = strings.TrimSpace(mediaType[:semi])
	}
	switch mediaType {
	case "application/pdf":
		return "pdf"
	case "text/plain":
		return "txt"
	case "text/csv":
		return "csv"
	case "text/html":
		return "html"
	case "application/json":
		return "json"
	case "application/msword":
		return "doc"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return "docx"
	}
	if idx := strings.LastIndex(mediaType, "/"); idx != -1 && idx+1 < len(mediaType) {
		return strings.TrimPrefix(mediaType[idx+1:], "x-")
	}
	return ""
}

func buildAssistantMessageStruct(msg gjson.Result, requestCtx *KiroRequestContext) KiroAssistantResponseMessage {
	content := msg.Get("content")
	var contentBuilder strings.Builder
	var thinkingBuilder strings.Builder
	var toolUses []KiroToolUse

	if content.IsArray() {
		for _, part := range content.Array() {
			switch part.Get("type").String() {
			case "text":
				appendAssistantTextPart(part.Get("text").String(), &contentBuilder, &thinkingBuilder)
			case "thinking":
				text := part.Get("thinking").String()
				if text == "" {
					text = part.Get("text").String()
				}
				if text != "" {
					_, _ = thinkingBuilder.WriteString(text)
				}
			case "tool_use":
				toolName := mapKiroToolName(part.Get("name").String(), requestCtx)
				input := map[string]any{}
				toolInput := part.Get("input")
				if toolInput.IsObject() {
					toolInput.ForEach(func(key, value gjson.Result) bool {
						input[key.String()] = value.Value()
						return true
					})
				}
				toolUses = append(toolUses, KiroToolUse{
					ToolUseID: part.Get("id").String(),
					Name:      toolName,
					Input:     input,
				})
			case "server_tool_use", "web_search_tool_result":
				// 服务端工具（web_search）的调用与结果由网关执行，Kiro history 里没有对应结构。
				// 渲染成文本保留上下文：直接丢掉会让模型在后续轮次里看不到它搜过什么。
				if text := renderKiroServerToolBlockText(part); text != "" {
					appendAssistantTextPart(text, &contentBuilder, &thinkingBuilder)
				}
			}
		}
	} else {
		appendAssistantTextPart(content.String(), &contentBuilder, &thinkingBuilder)
	}

	finalContent := contentBuilder.String()
	if thinkingText := thinkingBuilder.String(); thinkingText != "" {
		if strings.TrimSpace(finalContent) != "" {
			finalContent = thinkingStartTag + thinkingText + thinkingEndTag + "\n\n" + finalContent
		} else {
			finalContent = thinkingStartTag + thinkingText + thinkingEndTag
		}
	}
	if strings.TrimSpace(finalContent) == "" {
		finalContent = " "
	}
	return KiroAssistantResponseMessage{
		Content:  finalContent,
		ToolUses: toolUses,
	}
}

// renderKiroServerToolBlockText 把 assistant 历史中的服务端工具块渲染成文本。
func renderKiroServerToolBlockText(part gjson.Result) string {
	switch part.Get("type").String() {
	case "server_tool_use":
		query := strings.TrimSpace(part.Get("input.query").String())
		if query == "" {
			return ""
		}
		return "\n[Searched the web for: " + query + "]\n"
	case "web_search_tool_result":
		content := part.Get("content")
		if !content.IsArray() {
			if code := content.Get("error_code").String(); code != "" {
				return "\n[Web search failed: " + code + "]\n"
			}
			return ""
		}
		var sb strings.Builder
		for _, item := range content.Array() {
			title := strings.TrimSpace(item.Get("title").String())
			url := strings.TrimSpace(item.Get("url").String())
			if title == "" && url == "" {
				continue
			}
			if sb.Len() == 0 {
				_, _ = sb.WriteString("\n[Web search results]\n")
			}
			_, _ = sb.WriteString("- ")
			_, _ = sb.WriteString(title)
			if url != "" {
				_, _ = sb.WriteString(" (" + url + ")")
			}
			_, _ = sb.WriteString("\n")
		}
		return sb.String()
	default:
		return ""
	}
}

func appendAssistantTextPart(text string, contentBuilder, thinkingBuilder *strings.Builder) {
	if text == "" {
		return
	}
	if findRealThinkingStartTag(text, 0) == -1 {
		_, _ = contentBuilder.WriteString(text)
		return
	}
	pos := 0
	for pos < len(text) {
		start := findRealThinkingStartTag(text, pos)
		if start == -1 {
			_, _ = contentBuilder.WriteString(text[pos:])
			return
		}
		if start > pos {
			_, _ = contentBuilder.WriteString(text[pos:start])
		}
		end := findRealThinkingEndTag(text, start+len(thinkingStartTag))
		if end == -1 {
			_, _ = contentBuilder.WriteString(text[start:])
			return
		}
		thinking := strings.TrimPrefix(text[start+len(thinkingStartTag):end], "\n")
		if thinking != "" {
			_, _ = thinkingBuilder.WriteString(thinking)
		}
		pos = end + len(thinkingEndTag)
		if strings.HasPrefix(text[pos:], "\n\n") {
			pos += len("\n\n")
		}
	}
}

func mergeAdjacentMessages(messages []gjson.Result) []gjson.Result {
	if len(messages) <= 1 {
		return messages
	}
	var merged []gjson.Result
	for _, msg := range messages {
		if len(merged) == 0 {
			merged = append(merged, msg)
			continue
		}
		lastMsg := merged[len(merged)-1]
		role := msg.Get("role").String()
		lastRole := lastMsg.Get("role").String()
		if role == "tool" || lastRole == "tool" || role != lastRole {
			merged = append(merged, msg)
			continue
		}
		mergedMsg := map[string]any{
			"role":    role,
			"content": json.RawMessage(mergeMessageContent(lastMsg, msg)),
		}
		encoded, _ := json.Marshal(mergedMsg)
		merged[len(merged)-1] = gjson.ParseBytes(encoded)
	}
	return merged
}

func mergeMessageContent(msg1, msg2 gjson.Result) string {
	var blocks1, blocks2 []map[string]any
	content1 := msg1.Get("content")
	content2 := msg2.Get("content")
	if content1.IsArray() {
		for _, block := range content1.Array() {
			blocks1 = append(blocks1, blockToMap(block))
		}
	} else if content1.Type == gjson.String {
		blocks1 = append(blocks1, map[string]any{"type": "text", "text": content1.String()})
	}
	if content2.IsArray() {
		for _, block := range content2.Array() {
			blocks2 = append(blocks2, blockToMap(block))
		}
	} else if content2.Type == gjson.String {
		blocks2 = append(blocks2, map[string]any{"type": "text", "text": content2.String()})
	}
	if len(blocks1) > 0 && len(blocks2) > 0 && blocks1[len(blocks1)-1]["type"] == "text" && blocks2[0]["type"] == "text" {
		leftText, leftOK := blocks1[len(blocks1)-1]["text"].(string)
		rightText, rightOK := blocks2[0]["text"].(string)
		if leftOK && rightOK {
			blocks1[len(blocks1)-1]["text"] = leftText + "\n\n" + rightText
			blocks2 = blocks2[1:]
		}
	}
	allBlocks := append(blocks1, blocks2...)
	result, _ := json.Marshal(allBlocks)
	return string(result)
}

func blockToMap(block gjson.Result) map[string]any {
	result := make(map[string]any)
	block.ForEach(func(key, value gjson.Result) bool {
		if value.IsObject() {
			result[key.String()] = blockToMap(value)
		} else if value.IsArray() {
			var arr []any
			for _, item := range value.Array() {
				if item.IsObject() {
					arr = append(arr, blockToMap(item))
				} else {
					arr = append(arr, item.Value())
				}
			}
			result[key.String()] = arr
		} else {
			result[key.String()] = value.Value()
		}
		return true
	})
	return result
}

func parseEventStreamWithTerminal(body io.Reader) (string, []KiroToolUse, Usage, string, bool, error) {
	upstreamTerminalSignal := false
	contextWindowFull := false
	reader := bufio.NewReader(body)
	var content strings.Builder
	var toolUses []KiroToolUse
	var usage Usage
	stopReason := ""
	processedIDs := make(map[string]bool)
	var currentTool *toolUseState
	reasoningOpen := false
	closeReasoning := func() {
		if reasoningOpen {
			_, _ = content.WriteString(thinkingEndTag)
			_, _ = content.WriteString("\n\n")
			reasoningOpen = false
		}
	}

	for {
		msg, err := readEventStreamMessage(reader)
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, usage, stopReason, false, err
		}
		if msg.isException() {
			if msg.ExceptionType == kiroContentLengthExceededException && (content.Len() > 0 || len(toolUses) > 0 || currentTool != nil) {
				stopReason = "max_tokens"
				upstreamTerminalSignal = true
				break
			}
			return "", nil, usage, stopReason, false, newKiroStreamException(msg)
		}
		if msg == nil || len(msg.Payload) == 0 {
			continue
		}

		var event map[string]any
		decoder := json.NewDecoder(bytes.NewReader(msg.Payload))
		decoder.UseNumber()
		if err := decoder.Decode(&event); err != nil {
			continue
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			continue
		}
		if sr := readStopReason(event); sr != "" {
			stopReason = sr
		} else if sr := readStopReason(nestedEvent(event, msg.EventType)); sr != "" {
			stopReason = sr
		}
		if isKiroTerminalEvent(msg.EventType) || stopReason != "" {
			upstreamTerminalSignal = true
		}
		if kiroContextWindowFull(msg.EventType, event) {
			contextWindowFull = true
		}
		switch msg.EventType {
		case "assistantResponseEvent":
			closeReasoning()
			assistant := nestedEvent(event, "assistantResponseEvent")
			if text := getString(assistant, "content"); text != "" {
				_, _ = content.WriteString(text)
			} else if text := getString(event, "content"); text != "" {
				_, _ = content.WriteString(text)
			}
			if sr := readStopReason(assistant); sr != "" {
				stopReason = sr
			}
			for _, tool := range readToolUses(assistant, event) {
				if processedIDs[tool.ToolUseID] {
					continue
				}
				processedIDs[tool.ToolUseID] = true
				toolUses = append(toolUses, tool)
			}
		case "toolUseEvent":
			closeReasoning()
			completed, next := processToolUseEvent(event, currentTool, processedIDs)
			currentTool = next
			toolUses = append(toolUses, completed...)
		case "reasoningContentEvent":
			reasoning := nestedEvent(event, "reasoningContentEvent")
			text := getString(reasoning, "text")
			if text == "" {
				text = getString(event, "text")
			}
			if text != "" {
				// 连续 reasoning 片段累积进同一对 <thinking></thinking>，
				// 仅在首片写开始标签，结束标签在边界（content/tool/EOF）由 closeReasoning 补上。
				if !reasoningOpen {
					_, _ = content.WriteString(thinkingStartTag)
					reasoningOpen = true
				}
				_, _ = content.WriteString(text)
			}
		default:
			updateUsageFromEvent(&usage, msg.EventType, event)
		}
	}
	closeReasoning()

	if currentTool != nil && currentTool.ToolUseID != "" && !processedIDs[currentTool.ToolUseID] {
		completed, _ := processToolUseEvent(map[string]any{
			"toolUseEvent": map[string]any{
				"toolUseId": currentTool.ToolUseID,
				"name":      currentTool.Name,
				"stop":      true,
				"input":     currentTool.InputBuffer.String(),
			},
		}, currentTool, processedIDs)
		toolUses = append(toolUses, completed...)
	}
	cleanText, embeddedToolUses, _ := drainEmbeddedToolText(content.String())
	toolUses = append(toolUses, embeddedToolUses...)
	toolUses = deduplicateToolUses(toolUses)

	if usage.OutputTokens == 0 {
		var outputBuf strings.Builder
		_, _ = outputBuf.WriteString(cleanText)
		for _, tu := range toolUses {
			if b, err := json.Marshal(tu.Input); err == nil {
				_, _ = outputBuf.Write(b)
			}
		}
		if est := anthropictokenizer.CountTokens(outputBuf.String()); est > 0 {
			usage.OutputTokens = est
		}
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	// 上游的原始值是 END_TURN 这类大写枚举，必须归一化成 Anthropic 协议值，
	// 否则非流式响应会把 "END_TURN" 原样下发（流式路径早就做了同样的白名单）。
	stopReason = normalizeKiroUpstreamStopReason(stopReason)
	if stopReason == "" {
		if hasUsableToolUses(toolUses) {
			stopReason = "tool_use"
		} else {
			stopReason = "end_turn"
		}
	}
	stopReason = applyContextWindowStopReason(stopReason, contextWindowFull, hasUsableToolUses(toolUses))
	return cleanText, toolUses, usage, stopReason, upstreamTerminalSignal, nil
}

func buildClaudeResponse(content string, toolUses []KiroToolUse, model string, usage *Usage, stopReason string, requestCtx KiroRequestContext) ([]byte, string) {
	msgID := newClaudeMessageID()
	var blocks []map[string]any
	blocks = append(blocks, extractThinkingBlocksWithSignature(content, model, msgID)...)
	stopSequence := ""
	if len(toolUses) == 0 {
		if nextBlocks, matched := applyStopSequencesToTextBlocks(blocks, requestCtx.StopSequences); matched != "" {
			blocks = nextBlocks
			stopReason = "stop_sequence"
			stopSequence = matched
		}
		if stopSequence == "" {
			if nextBlocks, truncated := applyMaxOutputTokensToTextBlocks(blocks, requestCtx.MaxOutputTokens); truncated {
				blocks = nextBlocks
				stopReason = "max_tokens"
				if usage != nil && usage.OutputTokens > requestCtx.MaxOutputTokens {
					usage.OutputTokens = requestCtx.MaxOutputTokens
					usage.TotalTokens = usage.InputTokens + usage.OutputTokens
				}
			}
		}
	}
	if structuredText, remainingTools, ok := extractStructuredOutputToolText(toolUses, requestCtx); ok {
		if len(blocks) == 1 && blocks[0]["type"] == "text" && blocks[0]["text"] == "" {
			blocks = blocks[:0]
		}
		toolUses = remainingTools
		blocks = append(blocks, map[string]any{"type": "text", "text": structuredText})
		stopReason = "end_turn"
		stopSequence = ""
	}
	usableTools := 0
	for _, tool := range toolUses {
		if !isEmittableToolUse(tool) {
			continue
		}
		if requestCtx.MaxResponseToolUses > 0 && usableTools >= requestCtx.MaxResponseToolUses {
			break
		}
		usableTools++
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    tool.ToolUseID,
			"name":  restoreResponseToolName(tool.Name, requestCtx),
			"input": tool.Input,
		})
	}
	// 移除"thinking-only 强制 max_tokens"误判分支(与流式路径同步)
	// 非流式响应若仅有 thinking 块,补一个空 text 块保证协议完整性,但不强设 stop_reason
	if hasThinkingBlocksOnly(blocks) && usableTools == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}
	if stopReason == "" {
		if usableTools > 0 {
			stopReason = "tool_use"
		} else {
			stopReason = "end_turn"
		}
	}
	response := map[string]any{
		"id":          msgID,
		"type":        "message",
		"role":        "assistant",
		"model":       model,
		"content":     blocks,
		"stop_reason": stopReason,
		"usage":       buildKiroClaudeUsageMap(derefUsage(usage)),
	}
	response["stop_sequence"] = nullableStopSequence(stopSequence)
	result, _ := json.Marshal(response)
	return result, stopReason
}

func derefUsage(usage *Usage) Usage {
	if usage == nil {
		return Usage{}
	}
	return *usage
}

func nullableStopSequence(stopSequence string) any {
	if stopSequence == "" {
		return nil
	}
	return stopSequence
}

func applyStopSequencesToTextBlocks(blocks []map[string]any, stopSequences []string) ([]map[string]any, string) {
	if len(blocks) == 0 || len(stopSequences) == 0 {
		return blocks, ""
	}
	out := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		if block["type"] != "text" {
			out = append(out, block)
			continue
		}
		text, _ := block["text"].(string)
		idx, matched := firstStopSequenceIndex(text, stopSequences)
		if matched == "" {
			out = append(out, block)
			continue
		}
		next := make(map[string]any, len(block))
		for k, v := range block {
			next[k] = v
		}
		next["text"] = text[:idx]
		out = append(out, next)
		return out, matched
	}
	return blocks, ""
}

func applyMaxOutputTokensToTextBlocks(blocks []map[string]any, maxTokens int) ([]map[string]any, bool) {
	if len(blocks) == 0 || maxTokens <= 0 {
		return blocks, false
	}
	remaining := maxTokens
	out := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		if block["type"] != "text" {
			out = append(out, block)
			continue
		}
		text, _ := block["text"].(string)
		if remaining <= 0 {
			return out, true
		}
		tokens := anthropictokenizer.CountTokens(text)
		if tokens <= remaining {
			out = append(out, block)
			remaining -= tokens
			continue
		}
		next := make(map[string]any, len(block))
		for k, v := range block {
			next[k] = v
		}
		next["text"], _ = truncateTextToTokenLimit(text, remaining)
		out = append(out, next)
		return out, true
	}
	return blocks, false
}

func truncateTextToTokenLimit(text string, maxTokens int) (string, bool) {
	if maxTokens <= 0 {
		return "", text != ""
	}
	if anthropictokenizer.CountTokens(text) <= maxTokens {
		return text, false
	}
	runes := []rune(text)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if anthropictokenizer.CountTokens(string(runes[:mid])) <= maxTokens {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(runes[:lo]), true
}

func firstStopSequenceIndex(text string, stopSequences []string) (int, string) {
	bestIdx := -1
	bestSeq := ""
	for _, seq := range stopSequences {
		if seq == "" {
			continue
		}
		idx := strings.Index(text, seq)
		if idx < 0 {
			continue
		}
		if bestIdx == -1 || idx < bestIdx || (idx == bestIdx && len(seq) > len(bestSeq)) {
			bestIdx = idx
			bestSeq = seq
		}
	}
	return bestIdx, bestSeq
}

func stopSequencePotentialSuffix(text string, stopSequences []string) string {
	best := ""
	for _, seq := range stopSequences {
		if seq == "" {
			continue
		}
		limit := len(seq) - 1
		if limit > len(text) {
			limit = len(text)
		}
		for n := limit; n > len(best); n-- {
			if strings.HasSuffix(text, seq[:n]) {
				best = text[len(text)-n:]
				break
			}
		}
	}
	return best
}

func buildKiroClaudeUsageMap(usage Usage) map[string]any {
	usageMap := map[string]any{
		"input_tokens":  usage.InputTokens,
		"output_tokens": usage.OutputTokens,
	}
	addKiroCacheUsageFields(usageMap, usage)
	return usageMap
}

func extractStructuredOutputToolText(toolUses []KiroToolUse, requestCtx KiroRequestContext) (string, []KiroToolUse, bool) {
	if requestCtx.StructuredOutputToolName == "" || len(toolUses) == 0 {
		return "", toolUses, false
	}
	remaining := make([]KiroToolUse, 0, len(toolUses))
	for i, tool := range toolUses {
		if isStructuredOutputToolName(tool.Name, requestCtx) {
			if b, err := json.Marshal(tool.Input); err == nil {
				return string(b), append(remaining, toolUses[i+1:]...), true
			}
			return "{}", append(remaining, toolUses[i+1:]...), true
		}
		remaining = append(remaining, tool)
	}
	return "", toolUses, false
}

func isStructuredOutputToolName(name string, requestCtx KiroRequestContext) bool {
	return requestCtx.StructuredOutputToolName != "" && strings.TrimSpace(name) == requestCtx.StructuredOutputToolName
}

func restoreResponseToolName(name string, requestCtx KiroRequestContext) string {
	name = strings.TrimSpace(name)
	if requestCtx.ToolNameMap == nil {
		return name
	}
	if original := strings.TrimSpace(requestCtx.ToolNameMap[name]); original != "" {
		return original
	}
	return name
}

func hasThinkingBlocksOnly(blocks []map[string]any) bool {
	if len(blocks) == 0 {
		return false
	}
	hasThinking := false
	for _, block := range blocks {
		blockType, _ := block["type"].(string)
		switch blockType {
		case "thinking":
			hasThinking = true
		case "text":
			return false
		default:
			return false
		}
	}
	return hasThinking
}

func extractThinkingBlocks(content string) []map[string]any {
	return extractThinkingBlocksWithSignature(content, "claude", newClaudeMessageID())
}

func extractThinkingBlocksWithSignature(content, model, msgID string) []map[string]any {
	if content == "" {
		return nil
	}
	if findRealThinkingStartTag(content, 0) == -1 {
		return []map[string]any{{"type": "text", "text": content}}
	}
	var blocks []map[string]any
	var pendingThinking strings.Builder
	flushThinking := func() {
		thinking := pendingThinking.String()
		if strings.TrimSpace(thinking) != "" {
			blocks = append(blocks, map[string]any{
				"type":      "thinking",
				"thinking":  thinking,
				"signature": thinkingSignature(thinking, model, msgID),
			})
		}
		pendingThinking.Reset()
	}
	appendText := func(text string) {
		if strings.TrimSpace(text) != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": text})
		}
	}
	pos := 0
	for pos < len(content) {
		start := findRealThinkingStartTag(content, pos)
		if start == -1 {
			flushThinking()
			appendText(content[pos:])
			break
		}
		end := findRealThinkingEndTag(content, start+len(thinkingStartTag))
		if end == -1 {
			flushThinking()
			appendText(content[pos:])
			break
		}
		if text := content[pos:start]; strings.TrimSpace(text) != "" {
			flushThinking()
			appendText(text)
		}
		thinking := strings.TrimPrefix(content[start+len(thinkingStartTag):end], "\n")
		if strings.TrimSpace(thinking) != "" {
			_, _ = pendingThinking.WriteString(thinking)
		}
		pos = end + len(thinkingEndTag)
		if strings.HasPrefix(content[pos:], "\n\n") {
			pos += len("\n\n")
			flushThinking()
		}
	}
	flushThinking()
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}
	return blocks
}

func findRealThinkingStartTag(content string, from int) int {
	return findRealThinkingTag(content, thinkingStartTag, from, false)
}

func findRealThinkingEndTag(content string, from int) int {
	searchFrom := from
	for {
		pos := findRealThinkingTag(content, thinkingEndTag, searchFrom, true)
		if pos == -1 {
			return -1
		}
		after := pos + len(thinkingEndTag)
		if strings.HasPrefix(content[after:], "\n\n") || strings.TrimSpace(content[after:]) == "" {
			return pos
		}
		searchFrom = pos + 1
	}
}

func findStreamThinkingEndTagStrict(content string, from int) int {
	searchFrom := from
	for {
		pos := findRealThinkingTag(content, thinkingEndTag, searchFrom, true)
		if pos == -1 {
			return -1
		}
		after := pos + len(thinkingEndTag)
		if strings.HasPrefix(content[after:], "\n\n") {
			return pos
		}
		searchFrom = pos + 1
	}
}

func findStreamThinkingEndTagAtBufferEnd(content string, from int) int {
	searchFrom := from
	for {
		pos := findRealThinkingTag(content, thinkingEndTag, searchFrom, true)
		if pos == -1 {
			return -1
		}
		after := pos + len(thinkingEndTag)
		if strings.TrimSpace(content[after:]) == "" {
			return pos
		}
		searchFrom = pos + 1
	}
}

func safeThinkingStreamFlushLen(content string, keepBytes int) int {
	if keepBytes <= 0 || len(content) <= keepBytes {
		return 0
	}
	pos := len(content) - keepBytes
	for pos > 0 && !utf8.ValidString(content[:pos]) {
		pos--
	}
	for pos > 0 && !utf8.RuneStart(content[pos]) {
		pos--
	}
	return pos
}

func findRealThinkingTag(content, tag string, from int, allowEndBoundary bool) int {
	if from < 0 {
		from = 0
	}
	isStartTag := tag == thinkingStartTag
	searchFrom := from
	for searchFrom < len(content) {
		rel := strings.Index(content[searchFrom:], tag)
		if rel == -1 {
			return -1
		}
		pos := searchFrom + rel
		after := pos + len(tag)
		if !isThinkingTagQuoted(content, pos, after, isStartTag) &&
			!isInsideMarkdownFence(content, pos) &&
			!isLineBlockQuote(content, pos) &&
			(!allowEndBoundary || after <= len(content)) {
			return pos
		}
		searchFrom = pos + 1
	}
	return -1
}

func isThinkingTagQuoted(content string, start, after int, isStartTag bool) bool {
	if isStartTag && start > 0 && isThinkingQuoteChar(content[start-1]) {
		return true
	}
	return !isStartTag && after < len(content) && isThinkingQuoteChar(content[after])
}

func isThinkingQuoteChar(ch byte) bool {
	switch ch {
	case '`', '"', '\'', '\\':
		return true
	default:
		return false
	}
}

func isInsideMarkdownFence(content string, pos int) bool {
	inFence := false
	lineStart := 0
	for lineStart < pos {
		lineEnd := strings.IndexByte(content[lineStart:], '\n')
		if lineEnd == -1 {
			lineEnd = len(content)
		} else {
			lineEnd += lineStart
		}
		line := strings.TrimSpace(content[lineStart:lineEnd])
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			inFence = !inFence
		}
		lineStart = lineEnd + 1
	}
	return inFence
}

func isLineBlockQuote(content string, pos int) bool {
	lineStart := strings.LastIndexByte(content[:pos], '\n') + 1
	return strings.HasPrefix(strings.TrimLeftFunc(content[lineStart:pos], unicode.IsSpace), ">")
}

func readEventStreamMessage(reader *bufio.Reader) (*eventStreamMessage, error) {
	prelude := make([]byte, 12)
	_, err := io.ReadFull(reader, prelude)
	if err != nil {
		return nil, err
	}
	totalLength := binary.BigEndian.Uint32(prelude[0:4])
	headersLength := binary.BigEndian.Uint32(prelude[4:8])
	if totalLength < minFrameSize || totalLength > maxEventMsgSize {
		return nil, fmt.Errorf("invalid kiro eventstream frame length: %d", totalLength)
	}
	if headersLength > totalLength-16 {
		return nil, fmt.Errorf("invalid kiro eventstream headers length: %d", headersLength)
	}
	remaining := make([]byte, totalLength-12)
	if _, err := io.ReadFull(reader, remaining); err != nil {
		return nil, err
	}
	headerValues := extractStringHeaders(remaining[:headersLength])
	msg := &eventStreamMessage{
		EventType:     headerValues[":event-type"],
		MessageType:   headerValues[":message-type"],
		ExceptionType: headerValues[":exception-type"],
	}
	if msg.ExceptionType == "" {
		msg.ExceptionType = headerValues[":error-code"]
	}
	payloadStart := headersLength
	payloadEnd := uint32(len(remaining)) - 4
	if payloadStart < payloadEnd {
		msg.Payload = remaining[payloadStart:payloadEnd]
	}
	return msg, nil
}

func extractEventType(headers []byte) string {
	return extractStringHeaders(headers)[":event-type"]
}

// extractStringHeaders 解析 eventstream 头部中所有 string 类型（type 7）的值。
func extractStringHeaders(headers []byte) map[string]string {
	values := make(map[string]string, 4)
	offset := 0
	for offset < len(headers) {
		nameLen := int(headers[offset])
		offset++
		if offset+nameLen > len(headers) {
			break
		}
		name := string(headers[offset : offset+nameLen])
		offset += nameLen
		if offset >= len(headers) {
			break
		}
		valueType := headers[offset]
		offset++
		if valueType == 7 {
			if offset+2 > len(headers) {
				break
			}
			valueLen := int(binary.BigEndian.Uint16(headers[offset : offset+2]))
			offset += 2
			if offset+valueLen > len(headers) {
				break
			}
			values[name] = string(headers[offset : offset+valueLen])
			offset += valueLen
			continue
		}
		next, ok := skipHeaderValue(headers, offset, valueType)
		if !ok {
			break
		}
		offset = next
	}
	return values
}

func skipHeaderValue(headers []byte, offset int, valueType byte) (int, bool) {
	switch valueType {
	case 0, 1:
		return offset, true
	case 2:
		if offset+1 > len(headers) {
			return offset, false
		}
		return offset + 1, true
	case 3:
		if offset+2 > len(headers) {
			return offset, false
		}
		return offset + 2, true
	case 4:
		if offset+4 > len(headers) {
			return offset, false
		}
		return offset + 4, true
	case 5, 8:
		if offset+8 > len(headers) {
			return offset, false
		}
		return offset + 8, true
	case 6:
		if offset+2 > len(headers) {
			return offset, false
		}
		length := int(binary.BigEndian.Uint16(headers[offset : offset+2]))
		offset += 2
		if offset+length > len(headers) {
			return offset, false
		}
		return offset + length, true
	case 9:
		if offset+16 > len(headers) {
			return offset, false
		}
		return offset + 16, true
	default:
		return offset, false
	}
}

func processToolUseEvent(event map[string]any, currentTool *toolUseState, processedIDs map[string]bool) ([]KiroToolUse, *toolUseState) {
	tu := nestedEvent(event, "toolUseEvent")
	toolUseID := getString(tu, "toolUseId")
	name := getString(tu, "name")
	isStop, _ := tu["stop"].(bool)

	var inputFragment string
	var inputMap map[string]any
	if inputRaw, ok := tu["input"]; ok {
		switch v := inputRaw.(type) {
		case string:
			inputFragment = v
		case map[string]any:
			inputMap = v
		}
	}

	if toolUseID != "" && name != "" {
		if currentTool == nil || currentTool.ToolUseID != toolUseID {
			if processedIDs[toolUseID] {
				return nil, currentTool
			}
			currentTool = &toolUseState{ToolUseID: toolUseID, Name: name}
		}
	}
	if currentTool != nil && inputFragment != "" {
		_, _ = currentTool.InputBuffer.WriteString(inputFragment)
	}
	if currentTool != nil && inputMap != nil {
		currentTool.InputBuffer.Reset()
		encoded, _ := json.Marshal(inputMap)
		_, _ = currentTool.InputBuffer.Write(encoded)
	}
	if !isStop || currentTool == nil {
		return nil, currentTool
	}
	processedIDs[currentTool.ToolUseID] = true
	return []KiroToolUse{finalizeRawToolUse(currentTool.ToolUseID, currentTool.Name, currentTool.InputBuffer.String())}, nil
}

func extractSemanticEvents(eventType string, event map[string]any, lastContentFragment *string) []kiroSemanticEvent {
	if event == nil {
		return nil
	}
	var out []kiroSemanticEvent
	sourceStopReason := readStopReason(event)

	switch eventType {
	case "assistantResponseEvent":
		assistant := nestedEvent(event, "assistantResponseEvent")
		if sr := readStopReason(assistant); sr != "" {
			sourceStopReason = sr
		}
		if text := getString(assistant, "content"); text != "" {
			dup := lastContentFragment != nil && *lastContentFragment == text
			out = append(out, kiroSemanticEvent{
				Type:               kiroSemanticContent,
				Content:            text,
				SourceStopReason:   sourceStopReason,
				IsDuplicateContent: dup,
			})
		} else if text := getString(event, "content"); text != "" {
			dup := lastContentFragment != nil && *lastContentFragment == text
			out = append(out, kiroSemanticEvent{
				Type:               kiroSemanticContent,
				Content:            text,
				SourceStopReason:   sourceStopReason,
				IsDuplicateContent: dup,
			})
		}
		for _, tool := range readToolUses(assistant, event) {
			toolCopy := tool
			out = append(out, kiroSemanticEvent{
				Type:             kiroSemanticAssistantTU,
				ToolUse:          &toolCopy,
				SourceStopReason: sourceStopReason,
			})
		}
	case "reasoningContentEvent":
		reasoning := nestedEvent(event, "reasoningContentEvent")
		text := getString(reasoning, "text")
		if text == "" {
			text = getString(event, "text")
		}
		if text != "" {
			out = append(out, kiroSemanticEvent{
				Type:             kiroSemanticReasoning,
				Reasoning:        text,
				SourceStopReason: sourceStopReason,
			})
		}
	case "toolUseEvent":
		tu := nestedEvent(event, "toolUseEvent")
		toolUseID := getString(tu, "toolUseId")
		name := getString(tu, "name")
		isStop, _ := tu["stop"].(bool)
		inputSeen := false
		if inputRaw, ok := tu["input"]; ok {
			switch v := inputRaw.(type) {
			case string:
				inputSeen = true
				if toolUseID != "" && name != "" {
					out = append(out, kiroSemanticEvent{
						Type:             kiroSemanticToolUse,
						ToolUseID:        toolUseID,
						ToolName:         name,
						ToolInput:        v,
						ToolStop:         isStop,
						SourceStopReason: sourceStopReason,
					})
				} else if toolUseID != "" {
					out = append(out, kiroSemanticEvent{
						Type:             kiroSemanticToolInput,
						ToolUseID:        toolUseID,
						ToolName:         name,
						ToolInput:        v,
						SourceStopReason: sourceStopReason,
					})
				}
			case map[string]any:
				inputSeen = true
				if toolUseID != "" && name != "" {
					out = append(out, kiroSemanticEvent{
						Type:             kiroSemanticToolUse,
						ToolUseID:        toolUseID,
						ToolName:         name,
						ToolInputMap:     v,
						ToolStop:         isStop,
						SourceStopReason: sourceStopReason,
					})
				} else if toolUseID != "" {
					out = append(out, kiroSemanticEvent{
						Type:             kiroSemanticToolInput,
						ToolUseID:        toolUseID,
						ToolName:         name,
						ToolInputMap:     v,
						SourceStopReason: sourceStopReason,
					})
				}
			}
		}
		if !inputSeen && toolUseID != "" && name != "" {
			out = append(out, kiroSemanticEvent{
				Type:             kiroSemanticToolUse,
				ToolUseID:        toolUseID,
				ToolName:         name,
				SourceStopReason: sourceStopReason,
			})
		}
		if isStop {
			out = append(out, kiroSemanticEvent{
				Type:             kiroSemanticToolStop,
				ToolUseID:        toolUseID,
				ToolName:         name,
				ToolStop:         true,
				SourceStopReason: sourceStopReason,
			})
		}
	case "messageMetadataEvent", "metadataEvent", "supplementaryWebLinksEvent", "usageEvent", "messageStopEvent", "message_stop", "meteringEvent":
		out = append(out, kiroSemanticEvent{
			Type:             kiroSemanticUsage,
			SourceEventType:  eventType,
			RawEvent:         event,
			SourceStopReason: sourceStopReason,
		})
	default:
		out = append(out, kiroSemanticEvent{
			Type:             kiroSemanticUsage,
			SourceEventType:  eventType,
			RawEvent:         event,
			SourceStopReason: sourceStopReason,
		})
	}

	return out
}

func normalizeStreamingToolInput(name, raw string) (string, map[string]any, bool) {
	normalized := strings.TrimSpace(raw)
	if normalized == "" {
		normalized = "{}"
	}
	normalized = escapeControlCharsInStrings(normalized)
	normalized = removeTrailingCommasOutsideStrings(normalized)
	decoder := json.NewDecoder(strings.NewReader(normalized))
	decoder.UseNumber()
	var input map[string]any
	if err := decoder.Decode(&input); err != nil || input == nil {
		return "", nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "", nil, false
	}
	if hasMissingRequiredFields(name, input) {
		return "", nil, false
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", nil, false
	}
	return string(encoded), input, true
}

func repairJSON(input string) string {
	str := strings.TrimSpace(input)
	if str == "" {
		return "{}"
	}
	var parsed any
	if err := json.Unmarshal([]byte(str), &parsed); err == nil {
		return str
	}
	str = escapeControlCharsInStrings(str)
	str = removeTrailingCommasOutsideStrings(str)
	openBraces, openBrackets, inString := jsonBalance(str)
	if inString {
		str += `"`
		openBraces, openBrackets, _ = jsonBalance(str)
	}
	if openBraces > 0 {
		str += strings.Repeat("}", openBraces)
	}
	if openBrackets > 0 {
		str += strings.Repeat("]", openBrackets)
	}
	if err := json.Unmarshal([]byte(str), &parsed); err != nil {
		return strings.TrimSpace(input)
	}
	return str
}

func escapeControlCharsInStrings(input string) string {
	var out strings.Builder
	writeEscapedControl := func(ch byte) {
		switch ch {
		case '\n':
			_, _ = out.WriteString("\\n")
		case '\r':
			_, _ = out.WriteString("\\r")
		case '\t':
			_, _ = out.WriteString("\\t")
		default:
			const hex = "0123456789abcdef"
			_, _ = out.WriteString("\\u00")
			_ = out.WriteByte(hex[ch>>4])
			_ = out.WriteByte(hex[ch&0x0f])
		}
	}
	inString := false
	escape := false
	for i := 0; i < len(input); i++ {
		ch := input[i]
		if escape {
			if inString && ch < 0x20 {
				_ = out.WriteByte('\\')
				writeEscapedControl(ch)
			} else {
				_ = out.WriteByte(ch)
			}
			escape = false
			continue
		}
		if ch == '\\' {
			_ = out.WriteByte(ch)
			escape = true
			continue
		}
		if ch == '"' {
			inString = !inString
			_ = out.WriteByte(ch)
			continue
		}
		if inString && ch < 0x20 {
			writeEscapedControl(ch)
			continue
		}
		_ = out.WriteByte(ch)
	}
	return out.String()
}

func removeTrailingCommasOutsideStrings(input string) string {
	var out strings.Builder
	out.Grow(len(input))
	inString := false
	escape := false
	for i := 0; i < len(input); i++ {
		ch := input[i]
		if inString {
			_ = out.WriteByte(ch)
			if escape {
				escape = false
				continue
			}
			switch ch {
			case '\\':
				escape = true
			case '"':
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			_ = out.WriteByte(ch)
			continue
		}
		if ch == ',' {
			next := i + 1
			for next < len(input) {
				switch input[next] {
				case ' ', '\t', '\n', '\r':
					next++
					continue
				}
				break
			}
			if next < len(input) && (input[next] == '}' || input[next] == ']') {
				continue
			}
		}
		_ = out.WriteByte(ch)
	}
	return out.String()
}

func jsonBalance(input string) (openBraces int, openBrackets int, inString bool) {
	escape := false
	for i := 0; i < len(input); i++ {
		ch := input[i]
		if escape {
			escape = false
			continue
		}
		if ch == '\\' {
			escape = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch ch {
		case '{':
			openBraces++
		case '}':
			openBraces--
		case '[':
			openBrackets++
		case ']':
			openBrackets--
		}
	}
	return openBraces, openBrackets, inString
}

func finalizeRawToolUse(toolUseID, name, rawInput string) KiroToolUse {
	tool := KiroToolUse{
		ToolUseID: toolUseID,
		Name:      normalizeResponseToolName(name),
		Input:     map[string]any{},
	}
	rawInput = strings.TrimSpace(rawInput)
	tool.TruncatedRaw = rawInput
	decoded := false
	repaired := repairJSON(rawInput)
	if strings.TrimSpace(repaired) != "" {
		decoder := json.NewDecoder(strings.NewReader(repaired))
		decoder.UseNumber()
		var input map[string]any
		if err := decoder.Decode(&input); err == nil && input != nil {
			var trailing any
			if err := decoder.Decode(&trailing); err == io.EOF {
				tool.Input = input
				decoded = true
			}
		}
	}
	tool.IsTruncated = !decoded || isTruncatedToolUse(tool.Name, rawInput, tool.Input)
	return tool
}

func finalizeStructuredToolUse(toolUseID, name string, input map[string]any) KiroToolUse {
	if input == nil {
		input = map[string]any{}
	}
	tool := KiroToolUse{
		ToolUseID: toolUseID,
		Name:      normalizeResponseToolName(name),
		Input:     input,
	}
	tool.IsTruncated = hasMissingRequiredFields(tool.Name, tool.Input)
	return tool
}

func normalizeResponseToolName(name string) string {
	name = strings.TrimSpace(name)
	if name == "web_search" {
		return "remote_web_search"
	}
	return name
}

func isEmittableToolUse(tool KiroToolUse) bool {
	return !tool.IsTruncated && strings.TrimSpace(tool.ToolUseID) != "" && strings.TrimSpace(tool.Name) != ""
}

func shouldEmitToolUse(tool KiroToolUse, emittedToolContents map[string]bool) bool {
	if tool.IsTruncated {
		return false
	}
	key := toolUseContentKey(tool)
	if key == "" {
		return false
	}
	if emittedToolContents[key] {
		return false
	}
	emittedToolContents[key] = true
	return true
}

func hasUsableToolUses(toolUses []KiroToolUse) bool {
	for _, tool := range toolUses {
		if isEmittableToolUse(tool) {
			return true
		}
	}
	return false
}

func deduplicateToolUses(toolUses []KiroToolUse) []KiroToolUse {
	seenIDs := make(map[string]bool)
	seenContent := make(map[string]bool)
	out := make([]KiroToolUse, 0, len(toolUses))
	for _, tool := range toolUses {
		if tool.ToolUseID != "" {
			if seenIDs[tool.ToolUseID] {
				continue
			}
			seenIDs[tool.ToolUseID] = true
		}
		key := toolUseContentKey(tool)
		if key != "" && seenContent[key] {
			continue
		}
		if key != "" {
			seenContent[key] = true
		}
		out = append(out, tool)
	}
	return out
}

func toolUseContentKey(tool KiroToolUse) string {
	name := strings.TrimSpace(tool.Name)
	if name == "" {
		return ""
	}
	inputJSON, _ := json.Marshal(tool.Input)
	return name + ":" + string(inputJSON)
}

func drainEmbeddedToolText(text string) (cleanText string, toolUses []KiroToolUse, pending string) {
	complete, pending := splitCompleteEmbeddedToolText(text)
	if strings.TrimSpace(complete) == "" {
		// complete 为纯空白(无内嵌工具调用): 作为普通文本原样返回,
		// 交由下游 writeTextDelta 的缓冲逻辑决定保留(中段空行)还是丢弃(首尾)。
		// 不能在此直接吞掉, 否则标题后的独立 \n\n chunk 会丢失, 破坏 markdown 结构。
		return complete, nil, pending
	}
	cleanText, toolUses = parseEmbeddedToolCalls(complete)
	return cleanText, deduplicateToolUses(toolUses), pending
}

func splitCompleteEmbeddedToolText(text string) (complete string, pending string) {
	searchFrom := 0
	for {
		idx := strings.Index(text[searchFrom:], embeddedToolCallPrefix)
		if idx == -1 {
			return text, ""
		}
		idx += searchFrom
		_, _, end, ok := parseEmbeddedToolCallAt(text, idx)
		if !ok {
			return text[:idx], text[idx:]
		}
		searchFrom = end
	}
}

func parseEmbeddedToolCalls(text string) (string, []KiroToolUse) {
	if !strings.Contains(text, embeddedToolCallPrefix) {
		return text, nil
	}
	var (
		builder  strings.Builder
		toolUses []KiroToolUse
		index    int
	)
	for index < len(text) {
		start := strings.Index(text[index:], embeddedToolCallPrefix)
		if start == -1 {
			_, _ = builder.WriteString(text[index:])
			break
		}
		start += index
		_, _ = builder.WriteString(text[index:start])
		tool, _, end, ok := parseEmbeddedToolCallAt(text, start)
		if !ok {
			_, _ = builder.WriteString(text[start:])
			break
		}
		toolUses = append(toolUses, tool)
		index = end
	}
	return builder.String(), toolUses
}

func parseEmbeddedToolCallAt(text string, start int) (KiroToolUse, int, int, bool) {
	if start < 0 || start >= len(text) || !strings.HasPrefix(text[start:], embeddedToolCallPrefix) {
		return KiroToolUse{}, 0, 0, false
	}
	pos := start + len(embeddedToolCallPrefix)
	argsMarker := " with args:"
	argsIndex := strings.Index(text[pos:], argsMarker)
	if argsIndex == -1 {
		return KiroToolUse{}, 0, 0, false
	}
	argsIndex += pos
	toolName := strings.TrimSpace(text[pos:argsIndex])
	if toolName == "" {
		return KiroToolUse{}, 0, 0, false
	}
	jsonStart := argsIndex + len(argsMarker)
	for jsonStart < len(text) && (text[jsonStart] == ' ' || text[jsonStart] == '\t' || text[jsonStart] == '\n') {
		jsonStart++
	}
	if jsonStart >= len(text) || text[jsonStart] != '{' {
		return KiroToolUse{}, 0, 0, false
	}
	jsonEnd := findMatchingJSONBracket(text, jsonStart)
	if jsonEnd == -1 {
		return KiroToolUse{}, 0, 0, false
	}
	end := jsonEnd + 1
	for end < len(text) && text[end] != ']' {
		end++
	}
	if end >= len(text) {
		return KiroToolUse{}, 0, 0, false
	}
	rawJSON := text[jsonStart : jsonEnd+1]
	tool := finalizeRawToolUse("toolu_"+GenerateToolUseID(), toolName, rawJSON)
	return tool, start, end + 1, true
}

func findMatchingJSONBracket(text string, start int) int {
	depth := 0
	inString := false
	escape := false
	for i := start; i < len(text); i++ {
		ch := text[i]
		if escape {
			escape = false
			continue
		}
		if ch == '\\' {
			escape = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch ch {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func isTruncatedToolUse(name, rawInput string, input map[string]any) bool {
	rawInput = strings.TrimSpace(rawInput)
	if rawInput == "" {
		return hasToolRequirements(name)
	}
	if looksLikeTruncatedJSON(rawInput) {
		return true
	}
	return hasMissingRequiredFields(name, input)
}

func looksLikeTruncatedJSON(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw[0] != '{' {
		return false
	}
	openBraces, openBrackets, inString := jsonBalance(raw)
	if openBraces > 0 || openBrackets > 0 || inString {
		return true
	}
	last := raw[len(raw)-1]
	return last == ':' || last == ','
}

func hasToolRequirements(name string) bool {
	_, ok := requiredToolFields[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

func hasMissingRequiredFields(name string, input map[string]any) bool {
	groups, ok := requiredToolFields[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return false
	}
	for _, group := range groups {
		matched := false
		for _, candidate := range group {
			if _, exists := input[candidate]; exists {
				matched = true
				break
			}
		}
		if !matched {
			return true
		}
	}
	return false
}

func updateUsageFromEvent(usage *Usage, eventType string, event map[string]any) {
	if usage == nil {
		return
	}
	meta := nestedEvent(event, eventType)
	if len(meta) == 0 {
		meta = event
	}
	if tokenUsage, ok := meta["tokenUsage"].(map[string]any); ok {
		if value, ok := toInt(tokenUsage["uncachedInputTokens"]); ok {
			usage.InputTokens = value
		}
		if value, ok := toInt(tokenUsage["outputTokens"]); ok {
			usage.OutputTokens = value
		}
		if value, ok := toInt(tokenUsage["totalTokens"]); ok {
			usage.TotalTokens = value
		}
		// Kiro cache usage is reported only from local emulation. Ignore
		// tokenUsage cache fields even if upstream includes them.
		updateKiroCreditsFromMap(usage, tokenUsage)
	}
	updateKiroCreditsFromMap(usage, event)
	updateKiroCreditsFromMap(usage, meta)
	if value, ok := toInt(event["inputTokens"]); ok && value > 0 {
		usage.InputTokens = value
	}
	if value, ok := toInt(event["outputTokens"]); ok && value > 0 {
		usage.OutputTokens = value
	}
	if value, ok := toInt(event["totalTokens"]); ok && value > 0 {
		usage.TotalTokens = value
	}
	if value, ok := toInt(meta["inputTokens"]); ok && value > 0 {
		usage.InputTokens = value
	}
	if value, ok := toInt(meta["outputTokens"]); ok && value > 0 {
		usage.OutputTokens = value
	}
	if value, ok := toInt(meta["totalTokens"]); ok && value > 0 {
		usage.TotalTokens = value
	}
	if eventType == "meteringEvent" {
		if value, ok := toPositiveFiniteFloat(meta["usage"]); ok {
			usage.KiroCredits += value
		} else if value, ok := toPositiveFiniteFloat(event["usage"]); ok {
			usage.KiroCredits += value
		}
	}
}

func readToolUses(primary, fallback map[string]any) []KiroToolUse {
	var raw []any
	if value, ok := primary["toolUses"].([]any); ok {
		raw = value
	} else if value, ok := fallback["toolUses"].([]any); ok {
		raw = value
	}
	if len(raw) == 0 {
		return nil
	}
	out := make([]KiroToolUse, 0, len(raw))
	for _, item := range raw {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		input := map[string]any{}
		if value, ok := tool["input"].(map[string]any); ok {
			input = value
		}
		out = append(out, finalizeStructuredToolUse(getString(tool, "toolUseId"), getString(tool, "name"), input))
	}
	return out
}

func nestedEvent(event map[string]any, key string) map[string]any {
	if nested, ok := event[key].(map[string]any); ok {
		return nested
	}
	return event
}

func getString(m map[string]any, key string) string {
	if value, ok := m[key].(string); ok {
		return value
	}
	return ""
}

func readStopReason(m map[string]any) string {
	if stop := getString(m, "stop_reason"); stop != "" {
		return stop
	}
	return getString(m, "stopReason")
}

func toInt(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	default:
		return 0, false
	}
}

var kiroCreditUsageFieldNames = [...]string{
	"kiroCredits",
	"credits",
	"creditsUsed",
	"creditUsage",
	"consumedCredits",
}

func updateKiroCreditsFromMap(usage *Usage, values map[string]any) {
	if usage == nil || len(values) == 0 {
		return
	}
	for _, field := range kiroCreditUsageFieldNames {
		value, ok := toPositiveFiniteFloat(values[field])
		if !ok {
			continue
		}
		usage.KiroCredits = value
		return
	}
}

func toPositiveFiniteFloat(value any) (float64, bool) {
	var out float64
	switch v := value.(type) {
	case float64:
		out = v
	case float32:
		out = float64(v)
	case int:
		out = float64(v)
	case int64:
		out = float64(v)
	case json.Number:
		parsed, err := v.Float64()
		if err != nil {
			return 0, false
		}
		out = parsed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, false
		}
		out = parsed
	default:
		return 0, false
	}
	if math.IsNaN(out) || math.IsInf(out, 0) || out <= 0 {
		return 0, false
	}
	return out, true
}

func mergeKiroCacheEmulationUsage(base Usage, simulated *Usage) Usage {
	if simulated == nil {
		return base
	}
	base.InputTokens = simulated.InputTokens
	base.CacheReadInputTokens = simulated.CacheReadInputTokens
	base.CacheCreationInputTokens = simulated.CacheCreationInputTokens
	base.CacheCreation5mInputTokens = simulated.CacheCreation5mInputTokens
	base.CacheCreation1hInputTokens = simulated.CacheCreation1hInputTokens
	base.TotalTokens = base.InputTokens + base.OutputTokens + base.CacheReadInputTokens + base.CacheCreationInputTokens
	return base
}

func addKiroCacheUsageFields(usageMap map[string]any, usage Usage) {
	if usage.CacheCreationInputTokens > 0 {
		usageMap["cache_creation_input_tokens"] = usage.CacheCreationInputTokens
	}
	if usage.CacheReadInputTokens > 0 {
		usageMap["cache_read_input_tokens"] = usage.CacheReadInputTokens
	}
	if usage.CacheCreation5mInputTokens > 0 || usage.CacheCreation1hInputTokens > 0 {
		usageMap["cache_creation"] = map[string]any{
			"ephemeral_5m_input_tokens": usage.CacheCreation5mInputTokens,
			"ephemeral_1h_input_tokens": usage.CacheCreation1hInputTokens,
		}
	}
}
