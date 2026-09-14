package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/Wei-Shaw/sub2api/internal/pkg/anthropictokenizer"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// cursorClient 是协议层客户端的惰性单例。Client 内部维护按账号隔离的
// HTTP/2 连接池，必须复用；每次请求新建会让连接池失去意义。
var (
	cursorClientOnce sync.Once
	cursorClientInst *cursor.Client
)

func sharedCursorClient() *cursor.Client {
	cursorClientOnce.Do(func() { cursorClientInst = cursor.NewClient() })
	return cursorClientInst
}

// cursorTraceIDFromContext 取网关请求 ID 作为协议层 trace。
//
// 值由 middleware.RequestLogger 写入 request context；取不到时返回空串，
// 协议层会退回 trace=none（仅影响日志可读性，不影响请求本身）。
func cursorTraceIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	requestID, _ := ctx.Value(ctxkey.RequestID).(string)
	return strings.TrimSpace(requestID)
}

// cursorRequestFromBody 把 Anthropic /v1/messages 请求体转成协议层的 AgentRequest。
//
// ⚠️ 消息拍平必须走 cursor.BuildAgentMessage，不要在这里另写一份：
// agent.v1 是单轮协议，拍平格式（中文角色标签 + 历史边界）是针对真实
// 误判问题调出来的，换成英文 "User:/Assistant:" 会让模型把历史当成
// 用户粘贴的伪造 transcript 而拒答。
func cursorRequestFromBody(body []byte, model string) cursor.AgentRequest {
	root := gjson.ParseBytes(body)

	var msgs []cursor.ChatMessage
	var rawMsgs []cursor.AnthropicRawMessage
	root.Get("messages").ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").String()
		content := json.RawMessage(msg.Get("content").Raw)
		// ⚠️ 必须走 ParseAnthropicMessage 而不是 ParseContentBlocks：
		// 后者不认 tool_use / tool_result 块（两者都没有顶层 text 字段），
		// 会把整轮工具交互解析成空串后丢弃，导致模型重复调用同一个工具。
		// 一轮消息可能展开成多条（工具结果各占一条），故用 append(..., ...)。
		msgs = append(msgs, cursor.ParseAnthropicMessage(role, content)...)
		rawMsgs = append(rawMsgs, cursor.AnthropicRawMessage{Role: role, Content: content})
		return true
	})

	tools := cursorToolsFromBody(root)

	// ⚠️ 下面两步的顺序不可调换，且必须在 BuildAgentMessage / 系统提示词构造之前完成。
	//
	// 1) 先补齐核心工具：Cursor 上游会返回 shell/read 原生分支，本轮 tools 里
	//    没有对应定义时 agent.go 抛 ErrUndeclaredUpstreamTool，整轮夭折。
	// 2) 再抑制本地元工具：Skill 的结果由 Claude Code 本地展开整棵 skill 树，
	//    透给上游会让**下一轮请求在客户端侧**被判 "Prompt is too long" 而硬卡死。
	//
	// 顺序反了会静默失效：抑制摘掉的 Skill/ToolSearch/DeferredToolPlaceholder
	// 正是补齐逻辑用来判定「这是携带动态工具目录的主 Claude Code 请求」的 marker，
	// 先抑制会让 marker 数掉到阈值以下，补齐直接不触发。
	if merged, changed := cursor.MergeClaudeCodeCoreTools(model, tools); changed {
		tools = merged
	}
	if filtered, suppressed := cursor.SuppressClaudeCodeContextExpansionTools(model, tools); len(suppressed) > 0 {
		tools = filtered
		// 必须同时注入约束文案：只摘工具不说明，模型会反复 ToolSearch
		// 去找一个永远拿不到的工具，空转不推进。
		msgs = cursor.AppendClaudeCodeContextExpansionConstraint(msgs)
	}

	// system 只用于网关内部（token 估算/诊断）。普通 Cursor 账号不支持
	// custom_system_prompt，编码进上游会得到 invalid_argument。
	system := cursor.BuildAgentSystemPrompt(cursorSystemText(root), tools)

	// 只把「当前轮」（最后一个 assistant 轮之后）的附件挂到 selected_context：
	// 重放历史图片会让上游报 "Image not found"；而只取最后一条消息会丢掉
	// 同一轮里由 tool_result 带回的图片。
	images, documents := cursor.CollectCurrentTurnAttachments(msgs)

	// effort 档位决定 thinking 变体模型名，漏掉会让 output_config.effort 静默失效。
	resolvedModel := cursor.ApplyClaudeEffortModel(
		cursor.StripPrefix(model),
		root.Get("output_config.effort").String(),
	)

	return cursor.AgentRequest{
		Model:  resolvedModel,
		System: system,
		// MaxMode 必须跟随模型名里的 -max 后缀，否则 max 模型按普通模式跑。
		MaxMode: strings.Contains(strings.ToLower(resolvedModel), "max"),
		Message: cursor.BuildAgentMessage(msgs, tools),
		Tools:   tools,
		// ⚠️ ReadFileContent 不可省：Cursor 原生 Edit 只回传新内容，
		// 必须靠这张表补出 old_string，查不到会直接抛 ErrMalformedUpstreamTool
		// 让整个请求失败（终止错误，不降级）。
		ReadFileContent: cursor.ParseAnthropicReadFileContents(rawMsgs),
		Images:          images,
		Documents:       documents,
	}
}

func cursorSystemText(root gjson.Result) string {
	sys := root.Get("system")
	if !sys.Exists() {
		return ""
	}
	if sys.Type == gjson.String {
		return sys.String()
	}
	var parts []string
	sys.ForEach(func(_, block gjson.Result) bool {
		if t := strings.TrimSpace(block.Get("text").String()); t != "" {
			parts = append(parts, t)
		}
		return true
	})
	return strings.Join(parts, "\n\n")
}

func cursorToolsFromBody(root gjson.Result) []cursor.ToolDef {
	var tools []cursor.ToolDef
	root.Get("tools").ForEach(func(_, tool gjson.Result) bool {
		name := strings.TrimSpace(tool.Get("name").String())
		if name == "" {
			return true
		}
		// ToolDef.InputSchema 是 JSON 字符串而非对象。
		schema := strings.TrimSpace(tool.Get("input_schema").Raw)
		if schema == "" {
			schema = "{}"
		}
		tools = append(tools, cursor.ToolDef{
			Name:        name,
			Description: tool.Get("description").String(),
			InputSchema: schema,
		})
		return true
	})
	return tools
}

// cursorAnthropicEmitter 把协议层的三个回调塑形成 Anthropic SSE 事件序列。
//
// ⚠️ 内容块索引必须「先文本后工具」而不是真实到达顺序：协议层的
// onText/onReasoning 是流式实时回调，而 onTool 是在流结束后一次性补发的
// （见 agent.go 的 toolAcc 批量 flush）。按到达顺序编号会产生
// 交错且不闭合的块，Claude Code 会解析失败。
type cursorAnthropicEmitter struct {
	mu      sync.Mutex
	write   func(event string, data any) error
	model   string
	msgID   string
	started bool

	blockIdx      int
	textOpen      bool
	reasoningOpen bool

	// ⚠️ 缓冲正文/思考原文而不是只累计长度：token 必须在流结束后对完整文本
	// 整体计数。BPE 合并跨越片段边界，逐块计数再求和会严重高估（Cursor 的
	// text_delta 粒度很细，实测可高估 300%+）。cursorUsageTextCap 兜住内存。
	textBuf      strings.Builder
	reasoningBuf strings.Builder
	textLen      int
	reasoningLen int
	toolBytes    int
	toolCount    int
	stopReason   string

	firstTokenAt time.Time
	start        time.Time
}

func newCursorAnthropicEmitter(write func(string, any) error, model string, start time.Time) *cursorAnthropicEmitter {
	return &cursorAnthropicEmitter{
		write:      write,
		model:      model,
		msgID:      fmt.Sprintf("msg_%d", time.Now().UnixNano()),
		blockIdx:   -1,
		stopReason: "end_turn",
		start:      start,
	}
}

// cursorUsageTextCap 是为计费缓冲的输出原文上限（字节）。
//
// ⚠️ 为什么要设上限：emitter 会把整轮输出留在内存里以便流末整体分词。
// 不封顶时，超长回复（或上游异常吐流）会让单个请求的驻留内存无界增长，
// 高并发下直接打爆进程。
//
// ⚠️ 为什么取 1MB：远超正常回复（约 25 万 token），正常流量永远碰不到；
// 真触顶时超出部分回退到 len/3 估算——计费略有偏差，好过 OOM。
const cursorUsageTextCap = 1 << 20

// appendUsageText 把流式片段追加进计费缓冲，超过上限后停止追加。
// 调用方必须已持有 emitter 的锁。
func appendUsageText(buf *strings.Builder, piece string) {
	if buf.Len() >= cursorUsageTextCap {
		return
	}
	if remain := cursorUsageTextCap - buf.Len(); len(piece) > remain {
		// ⚠️ 按 rune 边界截断：直接切字节会在多字节字符中间断开，
		// 留下半个 UTF-8 序列，分词器只能把它当成替换字符处理。
		piece = piece[:trimToRuneBoundary(piece, remain)]
	}
	_, _ = buf.WriteString(piece)
}

// trimToRuneBoundary 返回 ≤ n 且落在 UTF-8 字符边界上的最大截断位置。
func trimToRuneBoundary(s string, n int) int {
	if n >= len(s) {
		return len(s)
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}

// cursorSSEHeartbeatInterval 是首字到达前的 SSE 保活间隔。
// 取 15s：远小于反代常见的 100s 空闲窗口，也不至于把日志刷爆。
const cursorSSEHeartbeatInterval = 15 * time.Second

// startPreStreamHeartbeat 在首个真实事件到达之前周期性写入 SSE 注释行保活，
// 返回停止函数（幂等，必须在 RunAgentStream 返回后立即调用）。
//
// ⚠️ 必须和内容事件共用 e.mu：心跳跑在独立 goroutine 上，与 OnText/OnReasoning
// 并发写同一个 http.ResponseWriter。不加锁会把注释行插进某个 data 帧中间，
// 产生客户端无法解析的半截事件。
//
// ⚠️ 首个真实事件发出后就不再写心跳：此后流本身就在持续产生字节，
// 继续插注释只是噪音；ensureStarted 置 started 即为分界。
func (e *cursorAnthropicEmitter) startPreStreamHeartbeat(
	ctx context.Context, w io.Writer, interval time.Duration,
) func() {
	if interval <= 0 {
		return func() {}
	}

	// 头已经发出，立刻写一个前导注释：让反代和客户端马上看到字节，
	// 而不是等到第一次 tick。
	e.mu.Lock()
	_, _ = io.WriteString(w, ": processing\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	e.mu.Unlock()

	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.mu.Lock()
				if e.started {
					// 真实事件已经开始推送，心跳完成使命。
					e.mu.Unlock()
					return
				}
				_, err := io.WriteString(w, ": processing\n\n")
				if err == nil {
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
				}
				e.mu.Unlock()
				if err != nil {
					// 客户端已断开，继续写没有意义。
					return
				}
			}
		}
	}()

	return stop
}

// ensureStarted 发送 message_start。延迟到第一个内容到达时才发，
// 这样换号/首字等待期间还能改写错误状态码。
func (e *cursorAnthropicEmitter) ensureStarted() error {
	if e.started {
		return nil
	}
	e.started = true
	e.firstTokenAt = time.Now()
	return e.write("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": e.msgID, "type": "message", "role": "assistant", "model": e.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]int{"input_tokens": 1, "output_tokens": 0},
		},
	})
}

func (e *cursorAnthropicEmitter) closeText() error {
	if !e.textOpen {
		return nil
	}
	e.textOpen = false
	return e.write("content_block_stop", map[string]any{"type": "content_block_stop", "index": e.blockIdx})
}

func (e *cursorAnthropicEmitter) closeReasoning() error {
	if !e.reasoningOpen {
		return nil
	}
	e.reasoningOpen = false
	return e.write("content_block_stop", map[string]any{"type": "content_block_stop", "index": e.blockIdx})
}

func (e *cursorAnthropicEmitter) OnText(piece string) {
	if piece == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.ensureStarted(); err != nil {
		return
	}
	if err := e.closeReasoning(); err != nil {
		return
	}
	if !e.textOpen {
		e.blockIdx++
		e.textOpen = true
		if err := e.write("content_block_start", map[string]any{
			"type": "content_block_start", "index": e.blockIdx,
			"content_block": map[string]any{"type": "text", "text": ""},
		}); err != nil {
			return
		}
	}
	if err := e.write("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": e.blockIdx,
		"delta": map[string]any{"type": "text_delta", "text": piece},
	}); err != nil {
		return
	}
	e.textLen += len(piece)
	appendUsageText(&e.textBuf, piece)
}

func (e *cursorAnthropicEmitter) OnReasoning(piece string) {
	if piece == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.ensureStarted(); err != nil {
		return
	}
	if err := e.closeText(); err != nil {
		return
	}
	if !e.reasoningOpen {
		e.blockIdx++
		e.reasoningOpen = true
		if err := e.write("content_block_start", map[string]any{
			"type": "content_block_start", "index": e.blockIdx,
			"content_block": map[string]any{"type": "thinking", "thinking": ""},
		}); err != nil {
			return
		}
	}
	// ⚠️ 思考内容同样是计费的输出 token。漏记会让 thinking 模型的
	// output_tokens 被系统性低估（长思考 + 短回答时几乎归零），直接导致少计费。
	e.reasoningLen += len(piece)
	appendUsageText(&e.reasoningBuf, piece)
	_ = e.write("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": e.blockIdx,
		"delta": map[string]any{"type": "thinking_delta", "thinking": piece},
	})
}

func (e *cursorAnthropicEmitter) OnTool(tc cursor.ToolCall) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.ensureStarted(); err != nil {
		return
	}
	if err := e.closeText(); err != nil {
		return
	}
	if err := e.closeReasoning(); err != nil {
		return
	}
	e.blockIdx++
	id := strings.TrimSpace(tc.ID)
	if id == "" {
		id = fmt.Sprintf("toolu_%d_%d", time.Now().UnixNano(), e.blockIdx)
	}
	args := strings.TrimSpace(string(tc.Input))
	if args == "" {
		args = "{}"
	}
	if err := e.write("content_block_start", map[string]any{
		"type": "content_block_start", "index": e.blockIdx,
		"content_block": map[string]any{"type": "tool_use", "id": id, "name": tc.Name, "input": map[string]any{}},
	}); err != nil {
		return
	}
	if err := e.write("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": e.blockIdx,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
	}); err != nil {
		return
	}
	if err := e.write("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": e.blockIdx,
	}); err != nil {
		return
	}
	e.toolBytes += cursor.ToolCallUsageBytes(tc)
	e.toolCount++
	e.stopReason = "tool_use"
}

// finish 闭合所有块并发送 message_delta/message_stop。
func (e *cursorAnthropicEmitter) finish(usage ClaudeUsage) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.ensureStarted(); err != nil {
		return err
	}
	if err := e.closeText(); err != nil {
		return err
	}
	if err := e.closeReasoning(); err != nil {
		return err
	}
	if err := e.write("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": e.stopReason, "stop_sequence": nil},
		"usage": map[string]int{"output_tokens": usage.OutputTokens},
	}); err != nil {
		return err
	}
	return e.write("message_stop", map[string]any{"type": "message_stop"})
}

// failMidStream 在已经开始输出后遇到错误时收尾。
//
// ⚠️ 绝不能在这里补发 message_delta/message_stop：那会让 Claude Code 把
// 一次失败请求误判成正常结束的一轮（end_turn），从而丢失重试机会。
// 只闭合已开的块，然后发 error 事件。
func (e *cursorAnthropicEmitter) failMidStream(errorType, message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.closeText()
	_ = e.closeReasoning()
	_ = e.write("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errorType, "message": message},
	})
}

// realOutputWritten 报告是否已经推出过**语义内容**（message_start 及其后的块）。
//
// ⚠️ 必须走这个带锁的读取口，不要直接读 e.started：OnText/OnReasoning/OnTool
// 由上游流的 goroutine 回调，心跳 goroutine 也会读它，裸读是数据竞争
// （-race 下必挂，生产里则是偶发的错误分支走偏）。
//
// ⚠️ 只写过 SSE 保活注释不算真实输出：注释按规范被客户端忽略，
// 此时这条流还没有任何语义内容，收尾方式与"已经吐了一半答案"完全不同。
func (e *cursorAnthropicEmitter) realOutputWritten() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.started
}

func (e *cursorAnthropicEmitter) usage(inputTokens int) ClaudeUsage {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := cursor.EstimateOutputUsageFromText(
		e.textBuf.String()+e.reasoningBuf.String(), e.toolBytes, e.toolCount)
	return ClaudeUsage{InputTokens: inputTokens, OutputTokens: out.OutputTokens}
}

func (e *cursorAnthropicEmitter) firstTokenMs() *int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.firstTokenAt.IsZero() {
		return nil
	}
	ms := int(e.firstTokenAt.Sub(e.start).Milliseconds())
	return &ms
}

// forwardCursorMessages 是 Cursor 平台的转发入口。
//
// 它只在**接入面**与其它平台保持同构（函数签名、cache plan、ops 错误事件、
// ForwardResult 用量上报），以便复用 sub2api 的调度/计费/监控系统。
//
// ⚠️ 协议转换部分完全遵循 Cursor 自身特性，不要照搬其它平台的做法：
//   - agent.v1 是单轮协议：整段会话被拍平成一条 user 文本（见 BuildAgentMessage），
//     不存在 Kiro/Anthropic 那种逐条 messages 数组。
//   - system 不发往上游：普通账号会对 custom_system_prompt 返回 invalid_argument。
//   - 工具调用在流结束后批量补发，因此内容块必须「先文本后工具」编号。
//   - 上游不返回可信 token 用量，输入输出均为估算值。
func (s *GatewayService) forwardCursorMessages(ctx context.Context, c *gin.Context, account *Account, parsed *ParsedRequest, startTime time.Time) (*ForwardResult, error) {
	if account == nil || parsed == nil {
		return nil, fmt.Errorf("cursor forward: missing account or request")
	}

	originalModel := parsed.Model
	mappedModel := originalModel
	if next := account.GetMappedModel(originalModel); next != "" {
		mappedModel = next
	}

	// ⚠️ 必须在映射之后、进协议层之前校验：网关只接受标准 Claude 协议模型。
	//
	// 放行 auto/default/composer 会让 Cursor 服务端自行选路，而 agent.v1 响应
	// 信封里没有 model 字段，我们无从得知实际服务方，只能整单按最贵模型兜底
	// 计费——对用户是无声的超额扣费。宁可在入口 400 明确拒绝。
	//
	// 校验 mappedModel 而不是 originalModel：映射是管理员配置的最终生效值，
	// 只校验原始名会让一条 "claude-opus-4-6 -> auto" 的映射绕过整道闸门。
	if err := cursor.ValidateDownstreamModel(mappedModel); err != nil {
		// 复用 BetaBlockedError 的契约：handler 对它是 400 invalid_request_error
		// 且**不 failover**。这点必须保证——模型名是客户端错误，逐个换号重试只会
		// 把整个号池烧一遍，每个号都失败在同一个原因上。
		return nil, &BetaBlockedError{Message: cursorInvalidModelMessage(mappedModel, err)}
	}

	body := parsed.Body.Bytes()

	token, err := s.cursorAccessToken(ctx, account)
	if err != nil {
		// ⚠️ 必须转成凭证级 failover 契约再返回。
		//
		// 直接 return err 的话 handler 的 errors.As 不匹配，会当场结束请求：
		// 号池里其它健康账号一个都用不上，而 token provider 恰恰刚把这个号
		// SetError 停用了——用户看到的是"有号可用却报错"。
		return nil, cursorCredentialFailover(c, account, err)
	}

	protoAccount := cursorProtocolAccount(account)
	protoAccount.AccessToken = token
	agentReq := cursorRequestFromBody(body, mappedModel)

	// TraceID 串上网关请求 ID：协议层的 logAgentDebug 全部以它为前缀。
	// 不串的话上游调试日志只会打 trace=none，一次线上排障拿到的几十条
	// [agent] 日志无法归属到具体请求，与网关侧日志也对不上。
	agentReq.TraceID = cursorTraceIDFromContext(ctx)

	inputTokens := estimateCursorInputTokens(agentReq)
	cachePlan := prepareCachePlanForContext(
		ctx, c, account, parsed.Group, body, mappedModel,
		"anthropic_messages", inputTokens,
	)

	logger.L().Debug("gateway forward_cursor_messages: request prepared",
		zap.Int64("account_id", account.ID),
		zap.String("requested_model", originalModel),
		zap.String("mapped_model", mappedModel),
		zap.String("upstream_model", agentReq.Model),
		zap.Int("tools", len(agentReq.Tools)),
	)

	if parsed.Stream {
		return s.streamCursorMessages(ctx, c, account, parsed, agentReq, &protoAccount,
			originalModel, mappedModel, inputTokens, cachePlan, startTime)
	}
	return s.blockCursorMessages(ctx, c, account, parsed, agentReq, &protoAccount,
		originalModel, mappedModel, inputTokens, cachePlan, startTime)
}

func (s *GatewayService) streamCursorMessages(
	ctx context.Context, c *gin.Context, account *Account, parsed *ParsedRequest,
	agentReq cursor.AgentRequest, protoAccount *cursor.Account,
	originalModel, mappedModel string, inputTokens int, cachePlan *cacheEmulationPlan, startTime time.Time,
) (*ForwardResult, error) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache, no-transform")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	write := func(event string, data any) error {
		return flushSSEJSON(c.Writer, event, data)
	}
	emitter := newCursorAnthropicEmitter(write, originalModel, startTime)

	// ⚠️ 头发完就立刻写一个 SSE 注释，并在首个真实事件到达前周期性续写。
	//
	// agent.v1 是单轮 agentic 调用，长 prompt 的首字延迟常达数十秒，协议层的
	// 首字硬墙是 90s；而 Cloudflare 等反代的空闲连接窗口是 100s，且 message_start
	// 被刻意延后到第一个内容才发（见 ensureStarted：延后才能在出错时改状态码）。
	// 三者叠加的结果是：慢但正常的请求会在反代处被掐断，客户端看到的是连接重置
	// 而不是「慢」。SSE 注释行（":" 开头）不是事件，任何合规客户端都会忽略，
	// 只用来让连接保持活跃。
	stopHeartbeat := emitter.startPreStreamHeartbeat(ctx, c.Writer, cursorSSEHeartbeatInterval)

	_, runErr := sharedCursorClient().RunAgentStream(ctx, protoAccount, agentReq,
		emitter.OnText, emitter.OnReasoning, emitter.OnTool)
	stopHeartbeat()

	usage := emitter.usage(inputTokens)
	mergeAndCommitCachePlan(c, &usage, true)

	if runErr != nil {
		classified := classifyCursorRunError(mappedModel, runErr)
		s.recordCursorFailure(ctx, c, account, classified)

		// ⚠️ 收尾方式按"是否已推出真实内容"分成两条，不能合并。
		//
		// 通用 handler 判定能否换号的依据是 c.Writer.Size() 有没有变化，而**不是**
		// SafeToFailoverAfterWrite（那个字段只有 OpenAI 侧的 handler 会读）。
		// 而 startPreStreamHeartbeat 在发头之后立刻无条件写了一个 ": processing"，
		// 于是流式路径的 Writer.Size() 必然已经变化 —— handler 一定走
		// handleFailoverExhausted，并且它会自己补写一帧 SSE error。
		//
		// 所以这里再调一次 failMidStream 就是第二帧 error：客户端连着收到两个
		// error 事件，严格的 SDK 会直接判协议错误。
		if realOutput := emitter.realOutputWritten(); realOutput {
			// 已经开了 content_block：handler 只会追加一帧 error，不会闭合块。
			// 必须由 emitter 自己闭合，否则客户端停在一个永不收尾的块上。
			// 这条流已经无法换号（内容撤不回），返回普通 error 即可。
			emitter.failMidStream(classified.ErrorType, classified.Message)
			return nil, fmt.Errorf("cursor upstream failed: %s", sanitizeUpstreamErrorMessage(runErr.Error()))
		}

		// 只写过心跳注释：没有任何待闭合的块，交给 handler 渲染错误帧。
		// 返回 failover 错误才能让账号进冷却、并在 Writer 未被写时换号重试。
		return nil, cursorFailoverError(classified, false)
	}

	if err := emitter.finish(usage); err != nil {
		return nil, err
	}

	return &ForwardResult{
		RequestID:     emitter.msgID,
		Usage:         usage,
		Model:         originalModel,
		UpstreamModel: agentReq.Model,
		Stream:        true,
		Duration:      time.Since(startTime),
		FirstTokenMs:  emitter.firstTokenMs(),
	}, nil
}

func (s *GatewayService) blockCursorMessages(
	ctx context.Context, c *gin.Context, account *Account, parsed *ParsedRequest,
	agentReq cursor.AgentRequest, protoAccount *cursor.Account,
	originalModel, mappedModel string, inputTokens int, cachePlan *cacheEmulationPlan, startTime time.Time,
) (*ForwardResult, error) {
	var (
		mu         sync.Mutex
		text       strings.Builder
		reasoning  strings.Builder
		toolCalls  []cursor.ToolCall
		toolBytes  int
		stopReason = "end_turn"
	)

	_, runErr := sharedCursorClient().RunAgentStream(ctx, protoAccount, agentReq,
		func(piece string) { mu.Lock(); _, _ = text.WriteString(piece); mu.Unlock() },
		func(piece string) { mu.Lock(); _, _ = reasoning.WriteString(piece); mu.Unlock() },
		func(tc cursor.ToolCall) {
			mu.Lock()
			toolCalls = append(toolCalls, tc)
			toolBytes += cursor.ToolCallUsageBytes(tc)
			stopReason = "tool_use"
			mu.Unlock()
		})

	if runErr != nil {
		classified := classifyCursorRunError(mappedModel, runErr)
		s.recordCursorFailure(ctx, c, account, classified)
		// ⚠️ 这里绝不能自己写响应体。
		//
		// 返回 *UpstreamFailoverError 后，handler 会继续换号重试；只有在
		// failover 全部用尽时才由 handleFailoverExhausted 渲染最终响应。
		// 若此处先写一份 c.JSON，换号成功的请求会先收到一个错误体、再收到
		// 正常结果（两份 body 拼在一条响应里），换号失败则是两份错误体。
		// 这也是 Kiro 的做法：runtime 只返回错误，响应一律交给 handler。
		//
		// 非流式路径尚未写出任何字节，换号永远是安全的。
		return nil, cursorFailoverError(classified, false)
	}

	// 思考内容计入输出 token，与流式路径保持一致；漏掉会少计费。
	out := cursor.EstimateOutputUsageFromText(text.String()+reasoning.String(), toolBytes, len(toolCalls))
	usage := ClaudeUsage{InputTokens: inputTokens, OutputTokens: out.OutputTokens}
	mergeAndCommitCachePlan(c, &usage, true)

	content := make([]map[string]any, 0, 2+len(toolCalls))
	if r := reasoning.String(); r != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": r})
	}
	if t := text.String(); t != "" {
		content = append(content, map[string]any{"type": "text", "text": t})
	}
	for i, tc := range toolCalls {
		id := strings.TrimSpace(tc.ID)
		if id == "" {
			id = fmt.Sprintf("toolu_%d_%d", time.Now().UnixNano(), i)
		}
		var input any = map[string]any{}
		if raw := strings.TrimSpace(string(tc.Input)); raw != "" {
			var decoded any
			if json.Unmarshal([]byte(raw), &decoded) == nil {
				input = decoded
			}
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": id, "name": tc.Name, "input": input,
		})
	}

	// Anthropic 的 Message 保证 content 至少有一个块，官方 SDK 直接按
	// message.content[0] 取值。空轮次（模型什么都没产出）如果回 "content": []
	// 会让客户端在解包时崩溃，所以补一个空文本块。
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}

	msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	c.Header("Content-Type", "application/json")
	c.JSON(http.StatusOK, map[string]any{
		"id": msgID, "type": "message", "role": "assistant", "model": originalModel,
		"content": content, "stop_reason": stopReason, "stop_sequence": nil,
		"usage": map[string]int{
			"input_tokens":  usage.InputTokens,
			"output_tokens": usage.OutputTokens,
		},
	})

	return &ForwardResult{
		RequestID:     msgID,
		Usage:         usage,
		Model:         originalModel,
		UpstreamModel: agentReq.Model,
		Stream:        false,
		Duration:      time.Since(startTime),
	}, nil
}

// recordCursorFailure 上报运维错误事件，并在「确认凭证失效」时才停用账号。
//
// ⚠️ 额度耗尽只标记对应的桶，不停号——三桶相互独立，
// 单桶耗尽时账号对其它桶的模型仍然可调度。
func (s *GatewayService) recordCursorFailure(ctx context.Context, c *gin.Context, account *Account, classified cursorErrorClassification) {
	safeErr := sanitizeUpstreamErrorMessage(classified.Message)
	setOpsUpstreamError(c, classified.StatusCode, safeErr, "")
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: classified.StatusCode,
		Kind:               classified.Category,
		Message:            safeErr,
	})

	// 终端协议错误只上报，不落任何账号状态：它描述的是请求本身不可服务，
	// 账号是健康的。写账号状态会让一个坏请求污染号池的可调度性。
	if classified.Terminal {
		return
	}

	if cursorShouldDisableAccount(classified) && s.accountRepo != nil {
		_ = s.accountRepo.SetError(ctx, account.ID, safeErr)
		return
	}
	if classified.QuotaBucket != "" {
		s.markCursorBucketExhausted(ctx, account, classified.QuotaBucket)
	}
}

// markCursorBucketExhausted 只把命中的那个桶标成耗尽并落库。
func (s *GatewayService) markCursorBucketExhausted(ctx context.Context, account *Account, bucket string) {
	if s.accountRepo == nil {
		return
	}
	q := readCursorQuota(account)
	now := time.Now().UTC()
	switch bucket {
	case cursor.QuotaBucketCursor:
		q.Cursor = CursorBucketQuota{State: CursorQuotaStateExhausted, Percent: 100}
	case cursor.QuotaBucketOther:
		q.Other = CursorBucketQuota{State: CursorQuotaStateExhausted, Percent: 100}
	case cursor.QuotaBucketGrokBot:
		q.GrokBot = CursorBucketQuota{State: CursorQuotaStateExhausted, Percent: 100, Enabled: q.GrokBot.Enabled}
	default:
		return
	}
	q.FetchedAt = &now
	writeCursorQuota(account, q)
	if err := s.accountRepo.Update(ctx, account); err != nil {
		logger.L().Warn("cursor: failed to persist exhausted quota bucket",
			zap.Int64("account_id", account.ID), zap.String("bucket", bucket), zap.Error(err))
	}
}

// cursorAccessToken 取可用的 access token。
func (s *GatewayService) cursorAccessToken(ctx context.Context, account *Account) (string, error) {
	if s.cursorTokenProvider != nil {
		return s.cursorTokenProvider.GetAccessToken(ctx, account)
	}
	token := strings.TrimSpace(account.GetCredential(CursorCredAccessToken))
	if token == "" {
		return "", fmt.Errorf("cursor account %d has no access token", account.ID)
	}
	return token, nil
}

// estimateCursorInputTokens 估算输入 token。
//
// ⚠️ Cursor agent.v1 不返回任何可信的 token 用量字段，输入输出都只能估算，
// 不可当作上游精确用量。
//
// ⚠️ 用 anthropictokenizer（Anthropic 官方 tokenizer 的本地 BPE 移植）而不是
// 任何按长度的启发式。此前的「非 ASCII 记 2、ASCII 记 1，再 /4」口径对中文
// **高估整整一倍**：100 个汉字真实约 25 token，该口径算出 50。中文 prompt
// 会被系统性多计费一倍，而这恰恰是本项目的主要使用场景。
//
// ⚠️ 必须整段计数，不能分段累加后求和：BPE 的合并跨越片段边界，
// 逐块计数会把每个片段的边界都变成 token 边界（实测按单字符切分时
// 高估 300%+）。这也是输出侧必须先缓冲再计数的原因。
//
// 与 ai2api 的差异（有意）：这里把 tools 的 schema 也计入。工具定义确实随
// 请求发给上游、确实消耗输入 token，ai2api 漏算了这部分。
func estimateCursorInputTokens(req cursor.AgentRequest) int {
	var sb strings.Builder
	_, _ = sb.WriteString(req.Message)
	_, _ = sb.WriteString("\n")
	_, _ = sb.WriteString(req.System)
	for _, t := range req.Tools {
		_, _ = sb.WriteString("\n")
		_, _ = sb.WriteString(t.Name)
		_, _ = sb.WriteString("\n")
		_, _ = sb.WriteString(t.Description)
		_, _ = sb.WriteString("\n")
		_, _ = sb.WriteString(t.InputSchema)
	}
	if tokens := anthropictokenizer.CountTokens(sb.String()); tokens > 0 {
		return tokens
	}
	// ⚠️ 兜底为 1 而不是 0：0 会让计费与限流把请求当成空请求。
	return 1
}

// cursorInvalidModelMessage 把协议层的模型校验错误转成面向客户端的文案。
// 对服务端选路别名要说清「为什么不给用」，否则用户只会反复重试同一个 auto。
func cursorInvalidModelMessage(model string, err error) string {
	if errors.Is(err, cursor.ErrServerSideRoutedModel) {
		return fmt.Sprintf(
			"Model %q is a Cursor server-side routing alias and is not supported. "+
				"Cursor does not report which model actually served the request, so usage cannot be billed accurately. "+
				"Please request an explicit model (e.g. claude-opus-4-6, claude-sonnet-4-5).",
			model,
		)
	}
	return fmt.Sprintf(
		"Model %q is not supported on the cursor platform. "+
			"Please request a standard Claude model (e.g. claude-opus-4-6, claude-sonnet-4-5).",
		model,
	)
}
