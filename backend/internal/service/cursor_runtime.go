package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

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

// cursorRequestFromBody 把 Anthropic /v1/messages 请求体转成协议层的 AgentRequest。
//
// ⚠️ 消息拍平必须走 cursor.BuildAgentMessage，不要在这里另写一份：
// agent.v1 是单轮协议，拍平格式（中文角色标签 + 历史边界）是针对真实
// 误判问题调出来的，换成英文 "User:/Assistant:" 会让模型把历史当成
// 用户粘贴的伪造 transcript 而拒答。
func cursorRequestFromBody(body []byte, model string) cursor.AgentRequest {
	root := gjson.ParseBytes(body)

	var msgs []cursor.ChatMessage
	root.Get("messages").ForEach(func(_, msg gjson.Result) bool {
		role := strings.TrimSpace(msg.Get("role").String())
		if role == "" {
			role = "user"
		}
		text, images, documents := cursor.ParseContentBlocks(json.RawMessage(msg.Get("content").Raw))
		// 工具结果块解析后可能没有可见文本，但仍要保留该轮，
		// 否则历史里会缺一轮、模型会重复调用同一个工具。
		if strings.TrimSpace(text) == "" && len(images) == 0 && len(documents) == 0 {
			return true
		}
		msgs = append(msgs, cursor.ChatMessage{
			Role: role, Content: text, Images: images, Documents: documents,
		})
		return true
	})

	tools := cursorToolsFromBody(root)

	// system 只用于网关内部（token 估算/诊断）。普通 Cursor 账号不支持
	// custom_system_prompt，编码进上游会得到 invalid_argument。
	system := cursor.BuildAgentSystemPrompt(cursorSystemText(root), tools)

	// 只把「当前轮」的附件挂到 selected_context：重放历史图片会让上游
	// 报 "Image not found"。
	var images []cursor.ImageAttachment
	var documents []cursor.DocumentAttachment
	if n := len(msgs); n > 0 {
		images = msgs[n-1].Images
		documents = msgs[n-1].Documents
	}

	return cursor.AgentRequest{
		Model:     cursor.StripPrefix(model),
		System:    system,
		Message:   cursor.BuildAgentMessage(msgs, tools),
		Tools:     tools,
		Images:    images,
		Documents: documents,
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

	textLen    int
	toolBytes  int
	toolCount  int
	stopReason string

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

func (e *cursorAnthropicEmitter) usage(inputTokens int) ClaudeUsage {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := cursor.EstimateOutputUsage(e.textLen, e.toolBytes, e.toolCount)
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
	body := parsed.Body.Bytes()

	token, err := s.cursorAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}

	protoAccount := cursorProtocolAccount(account)
	protoAccount.AccessToken = token
	agentReq := cursorRequestFromBody(body, mappedModel)

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

	_, runErr := sharedCursorClient().RunAgentStream(ctx, protoAccount, agentReq,
		emitter.OnText, emitter.OnReasoning, emitter.OnTool)

	usage := emitter.usage(inputTokens)
	mergeAndCommitCachePlan(c, &usage, true)

	if runErr != nil {
		classified := classifyCursorError(mappedModel, runErr.Error())
		s.recordCursorFailure(ctx, c, account, classified)
		if emitter.started {
			// 已经推流：只能在流内报错，不能再改状态码。
			emitter.failMidStream(classified.ErrorType, classified.Message)
			return nil, fmt.Errorf("cursor upstream failed: %s", sanitizeUpstreamErrorMessage(runErr.Error()))
		}
		emitter.failMidStream(classified.ErrorType, classified.Message)
		return nil, fmt.Errorf("cursor upstream failed: %s", sanitizeUpstreamErrorMessage(runErr.Error()))
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
		func(piece string) { mu.Lock(); text.WriteString(piece); mu.Unlock() },
		func(piece string) { mu.Lock(); reasoning.WriteString(piece); mu.Unlock() },
		func(tc cursor.ToolCall) {
			mu.Lock()
			toolCalls = append(toolCalls, tc)
			toolBytes += cursor.ToolCallUsageBytes(tc)
			stopReason = "tool_use"
			mu.Unlock()
		})

	if runErr != nil {
		classified := classifyCursorError(mappedModel, runErr.Error())
		s.recordCursorFailure(ctx, c, account, classified)
		c.JSON(classified.StatusCode, gin.H{
			"type":  "error",
			"error": gin.H{"type": classified.ErrorType, "message": classified.Message},
		})
		return nil, fmt.Errorf("cursor upstream failed: %s", sanitizeUpstreamErrorMessage(runErr.Error()))
	}

	out := cursor.EstimateOutputUsage(text.Len(), toolBytes, len(toolCalls))
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
// ⚠️ Cursor agent.v1 不返回任何可信的 token 用量字段，输入输出都只能估算
// （约 3 字节/token），不可当作上游精确用量。
func estimateCursorInputTokens(req cursor.AgentRequest) int {
	n := len(req.Message) + len(req.System)
	for _, t := range req.Tools {
		n += len(t.Name) + len(t.Description) + len(t.InputSchema)
	}
	if n <= 0 {
		return 1
	}
	if tokens := n / 3; tokens > 0 {
		return tokens
	}
	return 1
}
