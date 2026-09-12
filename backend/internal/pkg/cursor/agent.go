package cursor

// Cursor 3.x 现役聊天协议的原生实现——Qoder 号池风格。
//
// 为什么是这套: 旧端点 /aiserver.v1.ChatService/StreamUnifiedChatWithTools 已被上游退休,
// 任何客户端版本号都返回 "Update Required"。Cursor 3.x 桌面端/agent 均改用:
//   1) 默认 POST agentn.api5.cursor.sh /agent.v1.AgentService/Run —— Connect 双向流
//   2) HTTP/1.1 回退: BidiAppend 提交客户端消息 + RunSSE 读取服务端流
//   3) Sand/Grok Bot 直连模型和 Claude Code 兼容模型走 POST api2.cursor.sh
//      /aiserver.v1.InferenceService/Stream —— Connect 单请求流；当前上游已拒绝
//      AgentService 上的 sand traffic
//   4) AgentService 上游发来 request_context 握手 → 回一个空(headless 无工作区)上下文,
//      模型即开始产出
//   5) AgentService 逐帧读 interaction_update: field1=正文增量, field4=思考增量,
//      field2/3=工具调用
//
// 关键: 全程原生 HTTP/2 直连 + 本地 checksum 签名, 单轮空上下文、真 token 流式,
// 无任何 cursor-agent 子进程、无多步 agent 循环 —— 等同 Qoder "签名器直连"快路径。

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var agentDebug = os.Getenv("CURSOR_AGENT_DEBUG") != "" || os.Getenv("CURSOR_UPSTREAM_DEBUG") != ""

// 官方 Cursor CLI 对 Run 双向流约每 5 秒写入一个空 client_heartbeat。它只保活
// 传输和运行上下文，绝不作为下游模型正文或伪造的模型输出。
var agentBidiHeartbeatInterval = 5 * time.Second

// ErrUndeclaredUpstreamTool 表示 Cursor 上游要求执行本请求未声明的工具。
// 这不是账号故障，继续等待或换号均不能使下游客户端接受该工具调用。
var ErrUndeclaredUpstreamTool = errors.New("cursor upstream requested an undeclared tool")

// ErrMalformedUpstreamTool 表示 Cursor 上游发送了无法安全映射到下游协议的工具负载。
// 与未声明工具一样，必须作为终端协议错误结束本请求，不能伪造成普通文本完成。
var ErrMalformedUpstreamTool = errors.New("cursor upstream sent a malformed tool call")

// ErrIncompleteUpstreamStream 表示 Cursor 上游在发送明确的终结帧前就关闭了流。
// 已产生的正文不能因此被伪造成完整成功响应，也不能把该请求当作账号故障换号重试。
var ErrIncompleteUpstreamStream = errors.New("cursor upstream stream ended before completion")

// ErrInvalidUpstreamRequest 表示 Cursor 上游明确拒绝了本次请求的协议形状。
// 这属于网关协议问题，换账号只会把同一个坏请求重复发送到整个号池，不能计入账号错误。
var ErrInvalidUpstreamRequest = errors.New("cursor upstream rejected request schema")

func safeToolName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-', r == '.', r == ':':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 80 {
			break
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

func upstreamToolError(base error, branch int, native, wire, traceID string) error {
	parts := []string{
		fmt.Sprintf("branch=%d", branch),
		"tool=" + safeToolName(native),
	}
	if strings.TrimSpace(wire) != "" && !strings.EqualFold(strings.TrimSpace(wire), native) {
		parts = append(parts, "wire="+safeToolName(wire))
	}
	if strings.TrimSpace(traceID) != "" {
		parts = append(parts, "trace="+safeToolName(traceID))
	}
	return fmt.Errorf("%w: %s", base, strings.Join(parts, " "))
}

// AgentRequest 一次单轮对话请求(会话已拍平为单条 Message)。
type AgentRequest struct {
	Model           string
	System          string
	Message         string
	Tools           []ToolDef
	MaxMode         bool
	TraceID         string
	ReadFileContent map[string]string
	Images          []ImageAttachment
	Documents       []DocumentAttachment
}

type agentTraceContextKey struct{}

func withAgentTrace(ctx context.Context, traceID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(traceID) == "" {
		return ctx
	}
	return context.WithValue(ctx, agentTraceContextKey{}, strings.TrimSpace(traceID))
}

func agentTraceFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	traceID, _ := ctx.Value(agentTraceContextKey{}).(string)
	return strings.TrimSpace(traceID)
}

func logAgentDebug(traceID, format string, args ...interface{}) {
	if !agentDebug {
		return
	}
	traceID = strings.TrimSpace(traceID)
	if traceID == "" {
		traceID = "none"
	}
	log.Printf("[agent] trace=%s "+format, append([]interface{}{traceID}, args...)...)
}

// buildAgentMessage 把多轮会话拍平为单条消息文本(与上游 Ask 语义一致):
// 单条 user 直接原文; 多轮使用明确的历史边界和中文角色标签。
// system 不在这里处理。普通 Cursor 账号不支持
// AgentRunRequest.custom_system_prompt；该字段会被上游解析成 CLI
// --system-prompt 并返回 invalid_argument。系统提示只保留在网关内部用于亲和键、
// token 估算和诊断，不能伪装成 Cursor 的自定义系统字段。
// 不使用英文的 "User:"/"Assistant:" 对话脚本格式，避免模型把网关压平后的历史
// 误识别为用户粘贴的伪造 transcript，从而拒答或重复声明“未执行过工具”。
func buildAgentMessage(msgs []ChatMessage) string {
	if len(msgs) == 1 && msgs[0].Role == "user" {
		return msgs[0].Content
	}
	var b strings.Builder
	hasAssistant := false
	b.WriteString("[已验证的会话历史开始]\n")
	for _, m := range msgs {
		c := strings.TrimSpace(m.Content)
		if c == "" {
			continue
		}
		if m.Role == "assistant" {
			b.WriteString("[历史助手轮]\n")
			hasAssistant = true
		} else {
			b.WriteString("[历史用户轮]\n")
		}
		b.WriteString(c)
		b.WriteString("\n[历史轮结束]\n\n")
	}
	b.WriteString("[已验证的会话历史结束]\n")
	// 多轮且含历史 Assistant 轮(通常伴随工具调用): agent.v1 当前是单轮协议(conversation_state={}),
	// 整段历史被压成一条 user 文本, 模型易把自己上轮的工具调用误判为"尚未执行", 从而重复宣布计划、
	// 反复输出同一意图却不真正推进(客户反馈: 重复输出 / 只说不做 / 中英夹杂)。追加明确框定:
	// 历史工具调用均已执行、结果已给出, 应基于结果继续, 不重复、不复述。
	if hasAssistant {
		b.WriteString("[系统提示] 以上 Assistant 轮次及其中的工具调用均已实际执行完毕, 对应结果已在随后的 User 轮次中给出。" +
			"请直接基于这些已有结果继续推进任务: 不要重复已执行过的工具调用, 不要重新罗列计划或复述之前已表达过的意图, " +
			"只需给出下一步的实际动作或最终答复。保持与用户一致的语言作答。")
		if repeatedProgressTurns(msgs) {
			b.WriteString(" 历史中已经出现重复的进度套话；本轮禁止只输出“继续”或相同的进度句，必须执行一个具体动作、给出新证据，或明确说明可复现的阻塞原因。")
		}
	}
	return strings.TrimSpace(b.String())
}

func repeatedProgressTurns(msgs []ChatMessage) bool {
	count := 0
	for _, msg := range msgs {
		if msg.Role != "assistant" {
			continue
		}
		text := strings.TrimSpace(msg.Content)
		switch text {
		case "继续", "继续推进", "继续排查", "继续实施", "继续收尾", "继续完成":
			count++
		default:
			if strings.HasPrefix(text, "继续") && len([]rune(text)) <= 32 {
				count++
			}
		}
	}
	return count >= 2
}

const noToolsAgentConstraint = "[System constraint] No tools are available for this request. " +
	"Do not request or call shell, file, browser, MCP, or other tools. " +
	"Answer directly in text using only the supplied conversation."

const declaredToolsAgentConstraint = "[代理协议约束] 只能调用本轮明确声明并可用的工具；不要调用或请求未声明的 Cursor 原生 shell、glob、grep、ls、edit、delete 等工具。" +
	"如果当前只声明了 Read，就只使用 Read 读取目标文件；如果需要其他能力但没有对应声明，请直接说明限制并继续给出已有证据，不要反复尝试未声明工具。Write、TodoWrite、Task/Agent 也必须以本轮声明为准。"

const inferredClaudeAgentToolSchema = `{"type":"object","properties":{"description":{"type":"string"},"prompt":{"type":"string"},"subagent_type":{"type":"string"},"model":{"type":"string","enum":["sonnet","opus","haiku","fable"]},"run_in_background":{"type":"boolean"}},"required":["description","prompt","subagent_type"]}`

// buildAgentMessageForTools 在拍平后的消息上放入当前轮可提取的文件正文，
// 并在没有工具时明确告诉 Cursor 不要自行调用原生工具。这个约束放在
// 用户消息层，不使用普通 Cursor 账号不支持的 custom_system_prompt 字段。
func buildAgentMessageForTools(msgs []ChatMessage, tools []ToolDef) string {
	message := buildAgentMessage(msgs)
	message = appendDocumentContext(message, collectChatDocuments(msgs))
	if len(tools) == 0 {
		if message == "" {
			return noToolsAgentConstraint
		}
		return message + "\n\n" + noToolsAgentConstraint
	}
	return message + "\n\n" + declaredToolsAgentConstraint
}

// buildAgentSystemPrompt 保留网关内部的工具约束文本，供 token 估算和诊断使用；
// 普通账号不会把它编码到 AgentRunRequest.custom_system_prompt。
func buildAgentSystemPrompt(sys string, tools []ToolDef) string {
	parts := make([]string, 0, 3)
	if value := strings.TrimSpace(sys); value != "" {
		parts = append(parts, value)
	}
	if len(tools) == 0 {
		parts = append(parts, noToolsAgentConstraint)
	} else if hint := agentDelegationHint(tools); hint != "" {
		parts = append(parts, hint)
	}
	return strings.Join(parts, "\n\n")
}

func agentDelegationHint(tools []ToolDef) string {
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if strings.EqualFold(name, "Agent") || strings.EqualFold(name, "Task") {
			return "[System hint] Task/Agent 是 Claude Code 的子任务委派工具。用户要求分派、并行检查或让子 Agent 独立完成局部任务时，必须调用该工具，并提供 description、prompt、subagent_type；不要把委派需求降级为 Bash/Read。"
		}
	}
	return ""
}

// bidiAppend 通过 BidiService/BidiAppend 把一条 AgentClientMessage 追加到 run 的客户端流。
// data_binary=字段4, request_id=字段2, append_seqno=字段3。
func (c *Client) bidiAppend(ctx context.Context, httpClient *http.Client, a *Account, requestID string, seqno uint64, clientType string, clientMessage []byte) error {
	body := pbBytes(4, clientMessage)
	body = append(body, pbBytes(2, pbString(1, requestID))...)
	if seqno > 0 {
		body = append(body, pbField(3, 0, pbVarint(seqno))...)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", cursorAPIURL(bidiAppendURL), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header = c.buildHeadersWithType(a, "application/proto", clientType)
	applyCursorProxyAuth(req)
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("BidiAppend: %w", err)
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := summarizeUpstreamHTTPError(respBody)
		if detail != "" {
			return fmt.Errorf("BidiAppend HTTP %d: %s", resp.StatusCode, detail)
		}
		return fmt.Errorf("BidiAppend HTTP %d", resp.StatusCode)
	}
	return nil
}

// summarizeUpstreamHTTPError 保留有限、可分类的上游错误正文，避免把整段响应或控制字符
// 带入日志/错误链。错误正文只用于额度、认证、模型名等分类，不作为用户输出。
func summarizeUpstreamHTTPError(body []byte) string {
	s := strings.TrimSpace(strings.ToValidUTF8(string(body), "�"))
	if s == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' || r >= 0x20 {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
		if b.Len() >= 4096 {
			break
		}
	}
	s = strings.TrimSpace(b.String())
	if len(s) > 4096 {
		s = s[:4096]
	}
	return s
}

// buildRequestContextResult 应答上游 request_context_args: 回一个空的 headless 上下文,
// 使模型跳过工作区索引直接生成回复。exec id / exec_id 必须原样回传到同一条
// AgentService/Run 双向流；RunSSE 兼容路径也复用该正确关联信息。
//
//	结构: AgentClientMessage{ exec_client_message(2)={ request_context_result(10)={
//	  success(1)={ request_context(1)={} } } } }
func buildRequestContextResult(execParts []pbPart) []byte {
	success := pbBytes(1, []byte(nil)) // request_context = {}
	success = append(success, protoBool(2, false)...)
	result := pbBytes(1, success) // success = {...}
	execClient := make([]byte, 0, len(result)+32)
	if id, ok := pbFirst(execParts, 1); ok && id.Wire == 0 {
		execClient = append(execClient, pbField(1, 0, pbVarint(id.Value))...)
	}
	if execID, ok := pbFirst(execParts, 15); ok && execID.Wire == 2 {
		execClient = append(execClient, pbBytes(15, execID.Data)...)
	}
	execClient = append(execClient, pbBytes(10, result)...)
	return pbBytes(2, execClient) // exec_client_message
}

func buildAgentClientHeartbeat() []byte {
	// AgentClientMessage.client_heartbeat = tag 7; 不能误用 exec message(tag 2)。
	return pbBytes(7, nil)
}

// buildAgentClientMessage 构造首轮 AgentClientMessage(field1=RunAgentRequest)。
func buildAgentClientMessage(model string, in AgentRequest) []byte {
	user := protoString(1, appendDocumentContext(in.Message, in.Documents))
	user = append(user, protoString(2, genUUID())...)
	selectedContext := encodeSelectedImages(in.Images)
	selectedContext = append(selectedContext, encodeSelectedDocuments(in.Documents)...)
	if len(selectedContext) > 0 {
		user = append(user, protoMessage(3, selectedContext)...)
	}
	userAction := protoMessage(1, user)
	action := protoMessage(1, userAction)

	modelDetails := protoString(1, model)
	modelDetails = append(modelDetails, protoString(3, model)...)
	modelDetails = append(modelDetails, protoString(4, model)...)
	modelDetails = append(modelDetails, protoString(5, model)...)
	if in.MaxMode {
		modelDetails = append(modelDetails, protoBool(7, true)...)
	}
	requested := protoString(1, model)
	if in.MaxMode {
		requested = append(requested, protoBool(2, true)...)
	}
	requested = append(requested, protoBool(7, true)...)

	run := protoMessage(1, nil) // conversation_state = {}
	run = append(run, protoMessage(2, action)...)
	run = append(run, protoMessage(3, modelDetails)...)
	run = append(run, protoString(5, genUUID())...)
	// 普通 Cursor 账号不支持 custom_system_prompt(field 8)，绝不发送该字段。
	// McpTools(field 4) 使用单一 wrapper 承载 repeated tool definitions；即使无
	// 工具也发送空 wrapper，阻止 Cursor 默认注入 shell/grep/glob 等原生工具。
	mcpTools := []byte(nil)
	for _, tool := range in.Tools {
		wireName := toolWireName(tool)
		schema := tool.InputSchema
		if strings.TrimSpace(schema) == "" {
			schema = `{"type":"object","properties":{}}`
		}
		def := protoString(1, wireName)
		// Claude Code 的客户端工具由本地网关执行，使用 Cursor agent.v1
		// 约定的 provider_identifier，不能伪装成 Anthropic 服务端工具。
		def = append(def, protoString(4, "claude-local")...)
		def = append(def, protoString(5, wireName)...)
		def = append(def, protoString(2, tool.Description)...)
		// agent.v1.McpToolDefinition.input_schema 是
		// google.protobuf.Value(Struct)，不是 JSON 字符串。错误地放到
		// field 6 会使 Cursor 忽略 MCP 目录，随后生成未声明的 native
		// read/shell，Claude Code 只能看到 mid-response 错误。
		def = append(def, protoMessage(3, encodeMCPInputSchema(schema))...)
		mcpTools = append(mcpTools, protoMessage(1, def)...)
	}
	run = append(run, protoMessage(4, mcpTools)...)
	run = append(run, protoMessage(9, requested)...)
	return protoMessage(1, run)
}

// encodeMCPInputSchema 将 Anthropic/OpenAI 的 JSON Schema 编码为
// agent.v1.McpToolDefinition.input_schema 的
// google.protobuf.Value(struct_value)。
//
// 该字段不是 JSON 字符串：Value(5=Struct(fields=...))，Struct 的每个
// map entry 为 {1:key, 2:Value}。保持手写 protobuf 编码，避免为一个
// 已有最小线协议引入额外依赖。
func encodeMCPInputSchema(schema string) []byte {
	var raw any
	if err := json.Unmarshal([]byte(schema), &raw); err != nil {
		raw = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if obj, ok := raw.(map[string]any); !ok || obj == nil {
		raw = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return encodeProtoValue(raw)
}

func encodeProtoValue(value any) []byte {
	switch v := value.(type) {
	case nil:
		// google.protobuf.NullValue.NULL_VALUE = 0
		return pbField(1, 0, pbVarint(0))
	case bool:
		if v {
			return pbField(4, 0, pbVarint(1))
		}
		return pbField(4, 0, pbVarint(0))
	case string:
		return pbString(3, v)
	case float64:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(v))
		return pbField(2, 1, buf[:])
	case float32:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(float64(v)))
		return pbField(2, 1, buf[:])
	case int:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(float64(v)))
		return pbField(2, 1, buf[:])
	case int64:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(float64(v)))
		return pbField(2, 1, buf[:])
	case uint64:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(float64(v)))
		return pbField(2, 1, buf[:])
	case []any:
		var list []byte
		for _, item := range v {
			list = append(list, protoMessage(1, encodeProtoValue(item))...)
		}
		return protoMessage(6, list)
	case map[string]any:
		return protoMessage(5, encodeProtoStruct(v))
	default:
		return encodeProtoValue(nil)
	}
}

// encodeProtoStruct serializes google.protobuf.Struct.fields. Some Cursor
// schemas need the Struct itself, while google.protobuf.Value wraps it in
// field 5; keep that distinction explicit at call sites.
func encodeProtoStruct(value map[string]any) []byte {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var fields []byte
	for _, key := range keys {
		entry := protoString(1, key)
		entry = append(entry, protoMessage(2, encodeProtoValue(value[key]))...)
		fields = append(fields, protoMessage(1, entry)...)
	}
	return fields
}

// encodeSelectedImages 按 agent.v1.UserMessage.selected_context.selected_images 编码图片。
// 线协议字段与当前 Cursor agent.v1 schema 对齐：uuid=2、path=3、dimension=4、
// mime_type=7、data=8 bytes。不要改成其他版本的 data 字段，也不要把图片历史轮重新发送。
func encodeSelectedImages(images []ImageAttachment) []byte {
	var selectedContext []byte
	for _, image := range images {
		if len(image.Data) == 0 {
			continue
		}
		uuid := strings.TrimSpace(image.UUID)
		if uuid == "" {
			uuid = attachmentUUID(image.Data, image.Path)
		}
		path := strings.TrimSpace(image.Path)
		if path == "" {
			path = "claude-image-" + uuid + ".img"
		}
		mimeType := strings.TrimSpace(image.MIMEType)
		if mimeType == "" {
			mimeType = http.DetectContentType(image.Data)
		}
		selected := protoString(2, uuid)
		selected = append(selected, protoString(3, path)...)
		if image.Width > 0 && image.Height > 0 {
			dim := protoVarintMessage(1, uint64(image.Width))
			dim = append(dim, protoVarintMessage(2, uint64(image.Height))...)
			selected = append(selected, protoMessage(4, dim)...)
		}
		selected = append(selected, protoString(7, mimeType)...)
		selected = append(selected, pbBytes(8, image.Data)...)
		selectedContext = append(selectedContext, protoMessage(1, selected)...)
	}
	if len(selectedContext) == 0 {
		return nil
	}
	return selectedContext
}

// encodeSelectedDocuments 按当前 Cursor agent.v1.SelectedContext 编码文本文件与 PDF。
// 普通文件走 files=field 4，PDF 走 external_links=field 9；同时请求构造层会把正文
// 放入带边界的用户文本，以便兼容对 selected context 展示不一致的上游版本。
func encodeSelectedDocuments(documents []DocumentAttachment) []byte {
	var selectedContext []byte
	for _, document := range documents {
		text := strings.TrimSpace(document.Text)
		if document.IsPDF {
			if text == "" && strings.TrimSpace(document.URL) == "" {
				continue
			}
			link := protoString(1, document.URL)
			link = append(link, protoString(2, attachmentUUID(document.Data, document.Filename))...)
			link = append(link, protoString(3, text)...)
			link = append(link, protoBool(4, true)...)
			link = append(link, protoString(5, document.Filename)...)
			selectedContext = append(selectedContext, protoMessage(9, link)...)
			continue
		}
		if text == "" {
			continue
		}
		file := protoString(1, text)
		file = append(file, protoString(2, nonEmpty(document.Path, document.Filename))...)
		if document.Path != "" && document.Filename != "" && document.Path != document.Filename {
			file = append(file, protoString(3, document.Filename)...)
		}
		selectedContext = append(selectedContext, protoMessage(4, file)...)
	}
	return selectedContext
}

func protoVarintMessage(field int, value uint64) []byte {
	return pbField(field, 0, pbVarint(value))
}

// RunAgentStream 执行一次单轮 agent 对话: onText 收到每个正文增量(真流式),
// onReasoning 收思考增量(thinking 模型), onTool 收到本轮可交付的工具调用。
// 返回 produced(是否产出任何内容, 供上层对空响应故障转移)与错误。
func (c *Client) RunAgentStream(ctx context.Context, a *Account, in AgentRequest,
	onText func(string), onReasoning func(string), onTool func(ToolCall)) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if a == nil || strings.TrimSpace(a.AccessToken) == "" {
		return false, fmt.Errorf("missing access token")
	}
	traceID := strings.TrimSpace(in.TraceID)
	if traceID == "" {
		traceID = agentTraceFromContext(ctx)
	}
	if agentDebug && traceID == "" {
		traceID = genUUID()
	}
	model := in.Model
	if model == "" {
		model = "default"
	}
	quotaSurface := cursorClientTypeForModel(model)
	toolByLower := make(map[string]ToolDef, len(in.Tools))
	effectiveTools := append([]ToolDef(nil), in.Tools...)
	if inferred, ok := inferClaudeAgentTool(model, effectiveTools); ok {
		effectiveTools = append(effectiveTools, inferred)
		logAgentDebug(traceID, "tools inferred name=%s reason=claude-code-native-task-compat", inferred.Name)
	}
	directSand := useSandInferenceServiceForRequest(model, effectiveTools)
	upstreamClientType := quotaSurface
	if !directSand {
		upstreamClientType = agentServiceClientTypeForModel(model)
	}
	for _, td := range effectiveTools {
		toolByLower[strings.ToLower(td.Name)] = td
	}
	if agentDebug {
		names := make([]string, 0, len(effectiveTools))
		for _, td := range effectiveTools {
			if n := strings.TrimSpace(td.Name); n != "" {
				names = append(names, n)
			}
		}
		imageBytes := 0
		for _, image := range in.Images {
			imageBytes += len(image.Data)
		}
		documentBytes, documentTextBytes := 0, 0
		for _, document := range in.Documents {
			documentBytes += len(document.Data)
			documentTextBytes += len(document.Text)
		}
		logAgentDebug(traceID, "request model=%s quota_surface=%s upstream_client_type=%s direct_sand=%v tools=%d names=%s system_bytes=%d images=%d image_bytes=%d documents=%d document_bytes=%d document_text_bytes=%d",
			model, quotaSurface, upstreamClientType, directSand, len(effectiveTools), strings.Join(names, ","), len(in.System), len(in.Images), imageBytes, len(in.Documents), documentBytes, documentTextBytes)
	}
	in.Tools = effectiveTools
	requestID := genUUID()
	reqStart := time.Now()
	agentHTTP := *c.clientFor(a) // 复制以便设置本轮 Timeout, 不动共享连接池
	if c.agentTimeout > 0 {
		agentHTTP.Timeout = c.agentTimeout
	}
	if directSand {
		// Sand/Grok Bot 与 Claude Code 兼容模型只接受 InferenceService/Stream；不能把
		// 这类 direct Sand traffic 发到 AgentService/Run，否则会得到
		// HTTP 200 + trailer invalid_argument: Sand traffic is not supported
		// on this endpoint。
		return c.runSandStream(ctx, &agentHTTP, a, in, model, requestID, traceID, reqStart,
			onText, onReasoning, onTool)
	}
	produced, err := c.runAgentBidiStream(ctx, &agentHTTP, a, in, model, upstreamClientType, requestID, traceID, reqStart,
		toolByLower, onText, onReasoning, onTool)
	if err == nil || produced || !shouldFallbackAgentRun(err) {
		return produced, err
	}
	logAgentDebug(traceID, "Run 不可用，回退 RunSSE request_id=%s err=%v", requestID, err)
	return c.runAgentSSEStream(ctx, &agentHTTP, a, in, model, upstreamClientType, requestID, traceID, reqStart,
		toolByLower, onText, onReasoning, onTool)
}

func (c *Client) runAgentSSEStream(ctx context.Context, agentHTTP *http.Client, a *Account, in AgentRequest,
	model, clientType, requestID, traceID string, reqStart time.Time, toolByLower map[string]ToolDef,
	onText func(string), onReasoning func(string), onTool func(ToolCall)) (bool, error) {
	if err := c.bidiAppend(ctx, agentHTTP, a, requestID, 0, clientType, buildAgentClientMessage(model, in)); err != nil {
		logAgentDebug(traceID, "bidiAppend(初始)失败 request_id=%s model=%s err=%v", requestID, model, err)
		return false, err
	}
	logAgentDebug(traceID, "+%s bidiAppend(初始)完成 request_id=%s model=%s surface=%s max=%v tools=%d",
		time.Since(reqStart).Truncate(time.Millisecond), requestID, model, clientType, in.MaxMode, len(in.Tools))

	req, err := http.NewRequestWithContext(ctx, "POST", cursorAgentURL(runSSEURL), bytes.NewReader(wrapFrame(protoString(1, requestID))))
	if err != nil {
		logAgentDebug(traceID, "RunSSE 请求构造失败 request_id=%s err=%v", requestID, err)
		return false, err
	}
	req.Header = c.buildHeadersWithType(a, "application/connect+proto", clientType)
	req.Header.Set("Accept", "application/connect+proto")
	applyCursorProxyAuth(req)
	resp, err := agentHTTP.Do(req)
	if err != nil {
		logAgentDebug(traceID, "RunSSE HTTP 失败 request_id=%s err=%v", requestID, err)
		return false, fmt.Errorf("RunSSE: %w", err)
	}
	logAgentDebug(traceID, "+%s RunSSE 返回 HTTP %d request_id=%s", time.Since(reqStart).Truncate(time.Millisecond), resp.StatusCode, requestID)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		logAgentDebug(traceID, "RunSSE HTTP 非成功 request_id=%s status=%d", requestID, resp.StatusCode)
		return false, fmt.Errorf("RunSSE HTTP %d: %s", resp.StatusCode, string(body))
	}

	var seqno uint64 = 1
	sendContext := func(execParts []pbPart) error {
		if err := c.bidiAppend(ctx, agentHTTP, a, requestID, seqno, clientType, buildRequestContextResult(execParts)); err != nil {
			return err
		}
		seqno++
		return nil
	}
	return c.consumeAgentResponse(ctx, resp, requestID, traceID, reqStart, toolByLower, in.ReadFileContent,
		sendContext, onText, onReasoning, onTool)
}

// runAgentBidiStream 使用 AgentService/Run 的长连接双向流。每条客户端帧通过
// 同一个 HTTP 请求体发送：首帧是 run_request，随后按需回 request_context_result，
// 并周期性发送 tag 7 client_heartbeat。该实现不混用 BidiAppend / RunSSE。
func (c *Client) runAgentBidiStream(ctx context.Context, agentHTTP *http.Client, a *Account, in AgentRequest,
	model, clientType, requestID, traceID string, reqStart time.Time, toolByLower map[string]ToolDef,
	onText func(string), onReasoning func(string), onTool func(ToolCall)) (bool, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	bodyReader, bodyWriter := io.Pipe()
	outbound := make(chan []byte, 16)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer func() {
			if err := streamCtx.Err(); err != nil {
				_ = bodyWriter.CloseWithError(err)
				return
			}
			_ = bodyWriter.Close()
		}()
		for {
			select {
			case <-streamCtx.Done():
				return
			case frame, ok := <-outbound:
				if !ok {
					return
				}
				if _, err := bodyWriter.Write(frame); err != nil {
					return
				}
			}
		}
	}()
	defer bodyReader.Close()

	queueFrame := func(message []byte) error {
		frame := wrapFrame(message)
		select {
		case <-streamCtx.Done():
			return streamCtx.Err()
		case <-writerDone:
			return fmt.Errorf("AgentService/Run 客户端流已关闭")
		case outbound <- frame:
			return nil
		}
	}
	if err := queueFrame(buildAgentClientMessage(model, in)); err != nil {
		logAgentDebug(traceID, "Run 初始帧写入失败 request_id=%s err=%v", requestID, err)
		return false, err
	}

	runEndpoint := cursorAgentURL(runURL)
	req, err := http.NewRequestWithContext(streamCtx, "POST", runEndpoint, bodyReader)
	if err != nil {
		logAgentDebug(traceID, "Run 请求构造失败 request_id=%s err=%v", requestID, err)
		return false, err
	}
	req.Header = c.buildHeadersWithType(a, "application/connect+proto", clientType)
	req.Header.Set("Accept", "application/connect+proto")
	req.Header.Set("x-cursor-streaming", "true")
	req.Header.Set("x-original-request-id", requestID)
	if strings.HasPrefix(strings.ToLower(runEndpoint), "http://") {
		req.Header.Set("Expect", "100-continue")
	}
	applyCursorProxyAuth(req)
	resp, err := agentHTTP.Do(req)
	if err != nil {
		cancel()
		_ = bodyReader.CloseWithError(err)
		<-writerDone
		logAgentDebug(traceID, "Run HTTP 失败 request_id=%s err=%v", requestID, err)
		return false, fmt.Errorf("Run: %w", err)
	}
	logAgentDebug(traceID, "+%s Run 返回 HTTP %d request_id=%s surface=%s", time.Since(reqStart).Truncate(time.Millisecond), resp.StatusCode, requestID, clientType)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cancel()
		_ = bodyReader.CloseWithError(fmt.Errorf("Run HTTP %d", resp.StatusCode))
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		<-writerDone
		logAgentDebug(traceID, "Run HTTP 非成功 request_id=%s status=%d", requestID, resp.StatusCode)
		return false, fmt.Errorf("Run HTTP %d: %s", resp.StatusCode, string(body))
	}

	heartbeatStop := make(chan struct{})
	defer close(heartbeatStop)
	go func() {
		ticker := time.NewTicker(agentBidiHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatStop:
				return
			case <-streamCtx.Done():
				return
			case <-ticker.C:
				// 心跳队列满时保留业务帧优先级；下一个周期仍会继续保活。
				select {
				case outbound <- wrapFrame(buildAgentClientHeartbeat()):
				default:
				}
			}
		}
	}()

	sendContext := func(execParts []pbPart) error {
		return queueFrame(buildRequestContextResult(execParts))
	}
	return c.consumeAgentResponse(ctx, resp, requestID, traceID, reqStart, toolByLower, in.ReadFileContent,
		sendContext, onText, onReasoning, onTool)
}

func shouldFallbackAgentRun(err error) bool {
	if err == nil ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrInvalidUpstreamRequest) ||
		errors.Is(err, ErrIncompleteUpstreamStream) ||
		errors.Is(err, ErrUndeclaredUpstreamTool) ||
		errors.Is(err, ErrMalformedUpstreamTool) {
		return false
	}
	detail := strings.ToLower(err.Error())
	for _, marker := range []string{
		"run http 404",
		"run http 405",
		"run http 501",
		"run http 505",
		"unsupported cursor proxy path",
		"method not allowed",
		"not implemented",
	} {
		if strings.Contains(detail, marker) {
			return true
		}
	}
	return false
}

func agentServiceUpstreamError(detail string) error {
	if isSandRequestSchemaError(detail) {
		return fmt.Errorf("%w: AgentService: %s", ErrInvalidUpstreamRequest, detail)
	}
	return fmt.Errorf("%s", detail)
}

type agentFrameMessage struct {
	flag    byte
	payload []byte
	err     error
}

func (c *Client) consumeAgentResponse(ctx context.Context, resp *http.Response, requestID, traceID string,
	reqStart time.Time, toolByLower map[string]ToolDef, readFileContent map[string]string,
	sendContext func([]pbPart) error, onText func(string), onReasoning func(string), onTool func(ToolCall)) (bool, error) {
	defer resp.Body.Close()
	reader := NewStreamReader(resp.Body)
	frames := make(chan agentFrameMessage, 16)
	done := make(chan struct{})
	go func() {
		for {
			flag, payload, ferr := reader.ReadFrame()
			select {
			case frames <- agentFrameMessage{flag, payload, ferr}:
			case <-done:
				return
			}
			if ferr != nil {
				return
			}
		}
	}()
	defer close(done)

	hardTimeout := 600 * time.Second
	if c.agentTimeout > 0 {
		hardTimeout = c.agentTimeout
	}
	firstTokenHard := 90 * time.Second
	if c.firstToken > 0 {
		firstTokenHard = c.firstToken
	}
	const silenceGap = 45 * time.Second // 完全静默(连帧都收不到)=死连接; 大上下文/reasoning 间隔需足够
	const idleGap = 30 * time.Second    // 首字后静默这么久=轮次结束; coding agent 工具/文本间隔可达 10-20s
	hard := time.NewTimer(hardTimeout)
	defer hard.Stop()
	firstByte := time.NewTimer(firstTokenHard)
	defer firstByte.Stop()
	activity := time.NewTimer(silenceGap)
	defer activity.Stop()

	contextSent := false
	producedText := false
	producedReasoning := false
	var toolAcc []ToolCall
	var loopErr error
	firstFrameMS := int64(-1)
	firstReasoningMS := int64(-1)
	firstToolMS := int64(-1)
	firstTextMS := int64(-1)

	// 上下文握手必须"被动"响应: 等服务器发来 request_context_args 再回空上下文。
	// (实测提前预发会导致上游永不产出→首字超时, 故不可预发。)
	startAt := time.Now()
	exitReason := "eos"
	frameCount := 0
loop:
	for {
		select {
		case <-ctx.Done():
			exitReason = "context-canceled"
			loopErr = ctx.Err()
			break loop
		case <-hard.C:
			exitReason = "hard-timeout"
			loopErr = fmt.Errorf("对话超时: 超过 %v 硬上限", hardTimeout)
			break loop
		case <-firstByte.C:
			if !producedText && !producedReasoning && len(toolAcc) == 0 {
				exitReason = "first-token-timeout"
				loopErr = fmt.Errorf("首字超时: %v 内未产出任何内容", firstTokenHard)
				break loop
			}
		case <-activity.C:
			switch {
			case producedText || len(toolAcc) > 0:
				exitReason = "idle-gap" // 正常结束
			case producedReasoning:
				exitReason = "reasoning-stall"
				loopErr = fmt.Errorf("上游思考后静默: %v 内未产出正文", silenceGap)
			default:
				exitReason = "silence-timeout"
				loopErr = fmt.Errorf("上游静默: %v 内无任何数据帧", silenceGap)
			}
			break loop
		case fm := <-frames:
			frameCount++
			if fm.err != nil {
				if errors.Is(fm.err, io.EOF) || errors.Is(fm.err, io.ErrUnexpectedEOF) {
					// 只有 interaction_update.field14、tool-call-ready 或明确
					// trailer 才能结束本轮。正文后的直接/截断 EOF 仍是不完整流，
					// 不得按 idle-gap 或成功 end_turn 处理。
					loopErr = ErrIncompleteUpstreamStream
				} else {
					loopErr = fm.err
				}
				break loop
			}
			if firstFrameMS < 0 {
				firstFrameMS = time.Since(reqStart).Milliseconds()
				logAgentDebug(traceID, "+%dms first_frame flag=%d len=%d request_id=%s",
					firstFrameMS, fm.flag, len(fm.payload), requestID)
			}
			if fm.flag&0x02 != 0 { // end-of-stream trailer(Connect: JSON 编码, 复用旧解析器提取错误)
				if se := parseStreamError(fm.payload); se != "" && !producedText && len(toolAcc) == 0 {
					loopErr = agentServiceUpstreamError(se)
				}
				break loop
			}
			parts, perr := pbParse(fm.payload)
			if perr != nil {
				if producedText || len(toolAcc) > 0 {
					activity.Reset(idleGap)
				} else {
					activity.Reset(silenceGap)
				}
				continue
			}
			logAgentDebug(traceID, "+%s frame flag=%d len=%d pb=%s",
				time.Since(reqStart).Truncate(time.Millisecond), fm.flag, len(fm.payload), describePBParts(parts, 0))
			for _, p := range parts {
				switch p.Num {
				case 2: // exec_server_message
					execParts, e := pbParse(p.Data)
					if e != nil {
						continue
					}
					if _, ok := pbFirst(execParts, 10); ok && !contextSent { // request_context_args
						if err := sendContext(execParts); err != nil {
							loopErr = err
							break loop
						}
						contextSent = true
						logAgentDebug(traceID, "+%s context握手已回", time.Since(reqStart).Truncate(time.Millisecond))
					}
				case 1: // interaction_update
					if piece := parseInteractionText(p.Data); piece != "" {
						if agentDebug && !producedText {
							firstTextMS = time.Since(reqStart).Milliseconds()
							logAgentDebug(traceID, "+%dms 首字到达", firstTextMS)
						}
						if onText != nil {
							onText(piece)
						}
						producedText = true
					}
					if rtext, ok := parseInteractionReasoning(p.Data); ok {
						if firstReasoningMS < 0 {
							firstReasoningMS = time.Since(reqStart).Milliseconds()
							logAgentDebug(traceID, "+%dms first_reasoning text_bytes=%d", firstReasoningMS, len(rtext))
						}
						producedReasoning = true
						if onReasoning != nil && rtext != "" {
							onReasoning(rtext)
						}
					}
					updates, toolErr := parseInteractionToolUpdatesWithContext(p.Data, toolByLower, traceID, readFileContent)
					if toolErr != nil {
						// 上游已进入等待工具结果的状态；把未声明工具交给下游会造成
						// Claude Code 的 Unknown tool 循环，而静默忽略又会一直等待。
						// 因此必须以明确、不可换号的协议错误立刻结束本轮。
						exitReason = "unsupported-tool"
						loopErr = toolErr
						toolAcc = nil
						break loop
					}
					for _, update := range updates {
						toolAcc = appendToolCall(toolAcc, update.Call)
						logAgentDebug(traceID, "tool update name=%s completed=%v input_bytes=%d",
							update.Call.Name, update.Completed, len(update.Call.Input))
					}
					if len(updates) > 0 && firstToolMS < 0 {
						firstToolMS = time.Since(reqStart).Milliseconds()
						logAgentDebug(traceID, "+%dms first_tool updates=%d", firstToolMS, len(updates))
					}
					// Cursor 在工具调用已可解析后会等待客户端执行工具；此时不再保证发送
					// field14 的最终统计帧。继续等会让 Anthropic 下游永远收不到 tool_use。
					// 已验证的 tool-start/tool-completed update 均携带完整 call id、名称和参数，
					// 因此应立即结束本轮，让下游返回 tool_use 后开启下一轮。
					if len(updates) > 0 {
						exitReason = "tool-call-ready"
						break loop
					}
					if isInteractionFinal(p.Data) && (producedText || producedReasoning || len(toolAcc) > 0) {
						exitReason = "final-usage"
						break loop
					}
				default:
				}
			}
			if producedText || len(toolAcc) > 0 {
				activity.Reset(idleGap)
			} else if producedReasoning {
				activity.Reset(120 * time.Second) // reasoning 阶段用更长窗口: 复杂思考块间隔可达 1-2 分钟
			} else {
				activity.Reset(silenceGap)
			}
		}
	}
	logAgentDebug(traceID, "exit reason=%s elapsed=%s frames=%d text=%v reasoning=%v tools=%d first_frame_ms=%d first_reasoning_ms=%d first_tool_ms=%d first_text_ms=%d err=%v",
		exitReason, time.Since(startAt).Truncate(time.Millisecond), frameCount, producedText, producedReasoning,
		len(toolAcc), firstFrameMS, firstReasoningMS, firstToolMS, firstTextMS, loopErr)
	for i := range toolAcc {
		if onTool != nil {
			onTool(toolAcc[i])
		}
	}
	return producedText || len(toolAcc) > 0, loopErr
}

// ---------- interaction_update 解析 ----------

// parseInteractionText 取正文增量(field1 text_delta -> {1=text})
func parseInteractionText(data []byte) string {
	iu, err := pbParse(data)
	if err != nil {
		return ""
	}
	if td, ok := pbFirst(iu, 1); ok {
		if tdp, e := pbParse(td.Data); e == nil {
			if txt, ok := pbFirst(tdp, 1); ok && txt.Wire == 2 {
				return string(txt.Data)
			}
		}
	}
	return ""
}

// isInteractionFinal 识别 agent.v1 本轮结束统计帧。
// 实测 Cursor 在正文完成后发送 interaction_update.field14，随后连接仍可能持续心跳。
func isInteractionFinal(data []byte) bool {
	iu, err := pbParse(data)
	if err != nil {
		return false
	}
	_, ok := pbFirst(iu, 14)
	return ok
}

// parseInteractionReasoning 判断是否携带思考(field4 -> {1=text}); ok=true 表示确有 reasoning 结构。
func parseInteractionReasoning(data []byte) (string, bool) {
	iu, err := pbParse(data)
	if err != nil {
		return "", false
	}
	td, ok := pbFirst(iu, 4)
	if !ok || td.Wire != 2 {
		return "", false
	}
	if tdp, e := pbParse(td.Data); e == nil {
		if txt, ok := pbFirst(tdp, 1); ok && txt.Wire == 2 {
			return string(txt.Data), true
		}
	}
	return "", true
}

type interactionToolUpdate struct {
	Call      ToolCall
	Completed bool
}

// parseInteractionToolUpdates 取可交付工具调用(field2 started / field3 completed)。
// 工具更新在单个 interaction_update 内可以重复出现，不能只读取第一个同号字段。
//
// Cursor 原生工具优先映射到客户端显式声明的同名工具；当 Claude Code 没有声明
// grep/glob/ls/delete/read 但声明了 Bash 时，使用等价 Bash 命令降级，避免 Cursor
// 原生工具导致 mid-response 协议错误。未知原生工具和未声明 MCP 工具仍严格拒绝。
func parseInteractionToolUpdates(data []byte, toolByLower map[string]ToolDef) ([]interactionToolUpdate, error) {
	return parseInteractionToolUpdatesWithContext(data, toolByLower, "", nil)
}

func parseInteractionToolUpdatesWithTrace(data []byte, toolByLower map[string]ToolDef, traceID string) ([]interactionToolUpdate, error) {
	return parseInteractionToolUpdatesWithContext(data, toolByLower, traceID, nil)
}

func parseInteractionToolUpdatesWithContext(data []byte, toolByLower map[string]ToolDef, traceID string, readFileContent map[string]string) ([]interactionToolUpdate, error) {
	iu, err := pbParse(data)
	if err != nil {
		return nil, nil
	}
	var res []interactionToolUpdate
	for _, tc := range iu {
		if tc.Num != 2 && tc.Num != 3 {
			continue
		}
		call, callErr := parseToolCallUpdateWithContext(tc.Data, toolByLower, traceID, readFileContent)
		if callErr != nil {
			return nil, callErr
		}
		if call.Name != "" {
			res = append(res, interactionToolUpdate{Call: call, Completed: tc.Num == 3})
		}
	}
	return res, nil
}

// parseToolCallUpdate 解析 ToolCall{Started,Completed}Update: field1=call_id, field2=具体工具(oneof)。
func parseToolCallUpdate(data []byte, toolByLower map[string]ToolDef) (call ToolCall, retErr error) {
	return parseToolCallUpdateWithContext(data, toolByLower, "", nil)
}

func parseToolCallUpdateWithTrace(data []byte, toolByLower map[string]ToolDef, traceID string) (call ToolCall, retErr error) {
	return parseToolCallUpdateWithContext(data, toolByLower, traceID, nil)
}

func parseToolCallUpdateWithContext(data []byte, toolByLower map[string]ToolDef, traceID string, readFileContent map[string]string) (call ToolCall, retErr error) {
	parts, err := pbParse(data)
	if err != nil {
		return call, ErrMalformedUpstreamTool
	}
	if cid, ok := pbFirst(parts, 1); ok && cid.Wire == 2 {
		call.ID = string(cid.Data)
	}
	tcMsg, ok := pbFirst(parts, 2)
	if !ok {
		return call, ErrMalformedUpstreamTool
	}
	inner, e := pbParse(tcMsg.Data)
	if e != nil || len(inner) == 0 {
		return call, ErrMalformedUpstreamTool
	}
	branch := inner[0]

	// field15 = mcp_tool_call(自定义/ccx_ 工具): name + args(Struct) 均完整
	if branch.Num == 15 {
		if mc, err := pbParse(branch.Data); err == nil {
			if argsMsg, ok := pbFirst(mc, 1); ok {
				if args, err := pbParse(argsMsg.Data); err == nil {
					if nm, ok := pbFirst(args, 1); ok && nm.Wire == 2 {
						call.Name = string(nm.Data)
					}
					if m := mapEntriesToMap(args, 2); len(m) > 0 {
						if b, err := json.Marshal(m); err == nil {
							call.Input = json.RawMessage(b)
						}
					}
				}
			}
		}
		wireName := call.Name
		if strings.TrimSpace(wireName) == "" {
			logAgentDebug(traceID, "tool decode branch=15 malformed=true")
			return ToolCall{}, upstreamToolError(ErrMalformedUpstreamTool, branch.Num, "mcp", wireName, traceID)
		}
		call.Name = strings.TrimPrefix(call.Name, toolWirePrefix)
		if td, ok := resolveDeclaredTaskTool(call.Name, toolByLower); ok {
			call.Name = td.Name
			logAgentDebug(traceID, "tool decode branch=15 wire=%s mapped=%s declared=true", wireName, call.Name)
			return call, nil
		}
		logAgentDebug(traceID, "tool decode branch=15 wire=%s declared=false", wireName)
		return ToolCall{}, upstreamToolError(ErrUndeclaredUpstreamTool, branch.Num, "mcp", wireName, traceID)
	}

	// 内置 field 工具: Cursor 以原生类型返回, 客户端 schema 的参数被丢弃, 按 schema 属性名补回。
	native := toolCallFieldName(branch.Num)
	if td, ok := resolveClientTool(branch.Num, native, toolByLower); ok {
		call.Name = td.Name
		if branch.Num == 19 {
			call.Input = decodeNativeTaskToolArgs(branch.Data, td)
		} else {
			call.Input = decodeNativeToolArgs(branch.Num, branch.Data, td)
		}
		if branch.Num == 12 && isClaudeEditToolName(call.Name) {
			var editOK bool
			call.Input, editOK = completeNativeEditInput(call.Input, readFileContent)
			if !editOK {
				logAgentDebug(traceID, "tool decode native_edit_complete=false")
				return ToolCall{}, upstreamToolError(ErrMalformedUpstreamTool, branch.Num, native, "", traceID)
			}
		}
		if agentDebug && branch.Num == 12 {
			shape := "-"
			if bp, e := pbParse(branch.Data); e == nil {
				if argsMsg, ok := pbFirst(bp, 1); ok && argsMsg.Wire == 2 {
					if am, e := pbParse(argsMsg.Data); e == nil {
						shape = describePBParts(am, 2)
					}
				}
			}
			logAgentDebug(traceID, "tool decode edit_shape=%s", shape)
		}
		logAgentDebug(traceID, "tool decode branch=%d native=%s mapped=%s declared=true args_bytes=%d",
			branch.Num, native, call.Name, len(call.Input))
		return call, nil
	}
	if td, input, ok := resolveNativeBashFallback(branch.Num, branch.Data, toolByLower); ok {
		call.Name = td.Name
		call.Input = input
		logAgentDebug(traceID, "tool decode branch=%d native=%s mapped=%s fallback=bash declared=true args_bytes=%d",
			branch.Num, native, call.Name, len(call.Input))
		return call, nil
	}
	shape := "-"
	if nested, err := pbParse(branch.Data); err == nil {
		shape = describePBParts(nested, 1)
	}
	logAgentDebug(traceID, "tool decode branch=%d native=%s declared=false shape=%s",
		branch.Num, native, shape)
	return ToolCall{}, upstreamToolError(ErrUndeclaredUpstreamTool, branch.Num, native, "", traceID)
}

// resolveNativeBashFallback 将 Cursor 原生文件/搜索操作降级为 Claude Code 已声明的
// Bash 工具。Claude Code 的默认工具集合通常不声明 Grep/Glob/Ls/Delete，但 Bash
// 是稳定存在的等价执行入口。只有已声明 Bash 才允许降级，未知工具不会被伪装。
func resolveNativeBashFallback(fieldNum int, branchData []byte, toolByLower map[string]ToolDef) (ToolDef, json.RawMessage, bool) {
	td, ok := resolveClientTool(1, "shell", toolByLower)
	if !ok {
		return ToolDef{}, nil, false
	}
	args, ok := decodeNativeFallbackArgs(fieldNum, branchData)
	if !ok {
		return ToolDef{}, nil, false
	}
	command := ""
	switch fieldNum {
	case 3: // delete
		path, _ := args["path"].(string)
		command = "rm -f -- " + shellQuote(path)
	case 4: // glob
		path, _ := args["path"].(string)
		pattern, _ := args["pattern"].(string)
		if strings.TrimSpace(path) == "" {
			path = "."
		}
		if strings.TrimSpace(pattern) == "" {
			pattern = "*"
		}
		command = "find " + shellQuote(path) + " -type f -name " + shellQuote(pattern) + " -print"
	case 5: // grep
		pattern, _ := args["pattern"].(string)
		path, _ := args["path"].(string)
		glob, _ := args["glob"].(string)
		if strings.TrimSpace(path) == "" {
			path = "."
		}
		parts := []string{"rg", "--line-number", "--color", "never"}
		if ci, _ := args["case_insensitive"].(bool); ci {
			parts = append(parts, "--ignore-case")
		}
		if strings.TrimSpace(glob) != "" {
			parts = append(parts, "--glob", shellQuote(glob))
		}
		parts = append(parts, shellQuote(pattern), shellQuote(path))
		command = strings.Join(parts, " ")
	case 8: // read
		path, _ := args["path"].(string)
		command = "cat -- " + shellQuote(path)
	case 13: // ls
		path, _ := args["path"].(string)
		if strings.TrimSpace(path) == "" {
			path = "."
		}
		command = "ls -la -- " + shellQuote(path)
	default:
		return ToolDef{}, nil, false
	}
	if strings.TrimSpace(command) == "" {
		return ToolDef{}, nil, false
	}
	prop := firstSchemaProp(schemaRequiredProps(td.InputSchema), "command", "cmd", "script")
	if prop == "" {
		prop = "command"
	}
	input, err := json.Marshal(map[string]string{prop: command})
	if err != nil {
		return ToolDef{}, nil, false
	}
	return td, json.RawMessage(input), true
}

// decodeNativeFallbackArgs 读取已知 Cursor 原生工具的稳定字段。不要使用下游
// schema 的属性顺序推断上游参数，否则 Grep/Glob 的字段会被错绑。
func decodeNativeFallbackArgs(fieldNum int, branchData []byte) (map[string]any, bool) {
	outer, err := pbParse(branchData)
	if err != nil {
		return nil, false
	}
	// Cursor 的不同上游版本存在两种 wire 形状：
	// 1. branch {1: args_message{...}}（旧夹具/部分工具）；
	// 2. branch {1: path, 3: ...}（真实 read 负载直接把参数放在分支上）。
	// 先尝试嵌套 args，失败时将 branch 本身视为参数消息，避免把合法
	// native read 误判为未声明工具。
	args := outer
	if argsMsg, ok := pbFirst(outer, 1); ok && argsMsg.Wire == 2 {
		if nested, nestedErr := pbParse(argsMsg.Data); nestedErr == nil && len(nested) > 0 {
			args = nested
		}
	}
	out := map[string]any{}
	getText := func(field int) string {
		if p, ok := pbFirst(args, field); ok && p.Wire == 2 {
			return string(p.Data)
		}
		return ""
	}
	switch fieldNum {
	case 3:
		out["path"] = getText(1)
	case 4:
		out["path"] = getText(1)
		out["pattern"] = getText(2)
	case 5:
		out["pattern"] = getText(1)
		out["path"] = getText(2)
		out["glob"] = getText(3)
		if p, ok := pbFirst(args, 8); ok && p.Wire == 0 {
			out["case_insensitive"] = p.Value != 0
		}
	case 8:
		out["path"] = getText(1)
	case 13:
		out["path"] = getText(1)
	default:
		return nil, false
	}
	for key, value := range out {
		if s, ok := value.(string); ok && strings.TrimSpace(s) == "" && key != "path" {
			delete(out, key)
		}
	}
	return out, true
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// mapEntriesToMap 读 proto3 map<string,Value>(repeated field, 每项 {1:key,2:Value})
func mapEntriesToMap(parts []pbPart, field int) map[string]any {
	out := map[string]any{}
	for _, entry := range parts {
		if entry.Num != field || entry.Wire != 2 {
			continue
		}
		kv, err := pbParse(entry.Data)
		if err != nil {
			continue
		}
		var key string
		var val any
		for _, f := range kv {
			switch f.Num {
			case 1:
				key = string(f.Data)
			case 2:
				val = structValue(f.Data)
			}
		}
		if key != "" {
			out[key] = val
		}
	}
	return out
}

// structValue 解 google.protobuf.Value oneof
func structValue(data []byte) any {
	parts, err := pbParse(data)
	if err != nil || len(parts) == 0 {
		return nil
	}
	f := parts[0]
	switch f.Num {
	case 1:
		return nil
	case 2:
		if f.Wire == 1 && len(f.Data) == 8 {
			return math.Float64frombits(binary.LittleEndian.Uint64(f.Data))
		}
		return nil
	case 3:
		return string(f.Data)
	case 4:
		return f.Value != 0
	case 5:
		if sp, err := pbParse(f.Data); err == nil {
			return mapEntriesToMap(sp, 1)
		}
		return nil
	case 6:
		var list []any
		if lp, err := pbParse(f.Data); err == nil {
			for _, item := range lp {
				if item.Num == 1 && item.Wire == 2 {
					list = append(list, structValue(item.Data))
				}
			}
		}
		return list
	}
	return nil
}

func toolCallFieldName(num int) string {
	switch num {
	case 1:
		return "shell"
	case 3:
		return "delete"
	case 4:
		return "glob"
	case 5:
		return "grep"
	case 8:
		return "read"
	case 9:
		return "update_todos"
	case 10:
		return "read_todos"
	case 12:
		return "edit"
	case 13:
		return "ls"
	case 14:
		return "read_lints"
	case 19:
		return "task"
	default:
		return fmt.Sprintf("tool_%d", num)
	}
}

var nativeAliases = map[int][]string{
	1:  {"shell", "bash", "run_terminal_cmd", "runterminalcmd", "terminal", "runcommand"},
	3:  {"delete", "delete_file", "deletefile", "rm"},
	4:  {"glob", "globtool", "glob_file_search", "findfiles", "find"},
	5:  {"grep", "grep_search", "grepsearch", "ripgrep", "search"},
	8:  {"read", "read_file", "readfile", "view"},
	9:  {"update_todos", "todowrite", "todo_write"},
	10: {"read_todos", "todoread", "todo_read"},
	12: {"edit", "edit_file", "editfile", "str_replace", "str_replace_editor", "apply_patch", "write", "write_file", "writefile"},
	13: {"ls", "list_dir", "listdir", "list_directory"},
	14: {"read_lints", "readlints", "diagnostics"},
	19: {"agent", "task"},
}

var nativeArgSubfields = map[int][]int{
	1: {1}, // shell: command
	4: {2}, // glob:  pattern
	8: {1}, // read:  path
}

func defaultToolDef(fieldNum int) ToolDef {
	switch fieldNum {
	case 1:
		return ToolDef{Name: "shell", InputSchema: `{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`}
	case 4:
		return ToolDef{Name: "glob", InputSchema: `{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}`}
	case 5:
		return ToolDef{Name: "grep", InputSchema: `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"}},"required":["pattern","path"]}`}
	case 8:
		return ToolDef{Name: "read", InputSchema: `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`}
	case 12:
		return ToolDef{Name: "edit", InputSchema: `{"type":"object","properties":{"path":{"type":"string"},"old":{"type":"string"},"new":{"type":"string"}},"required":["path","old","new"]}`}
	case 13:
		return ToolDef{Name: "ls", InputSchema: `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`}
	case 19:
		return ToolDef{Name: "task", InputSchema: `{"type":"object","properties":{}}`}
	case 9:
		return ToolDef{Name: "update_todos", InputSchema: `{"type":"object","properties":{"todos":{"type":"array"}},"required":["todos"]}`}
	case 10:
		return ToolDef{Name: "read_todos", InputSchema: `{"type":"object","properties":{}}`}
	default:
		return ToolDef{Name: toolCallFieldName(fieldNum)}
	}
}

// claudeCodeCoreToolDefs 返回 Claude Code CLI 的标准核心工具集。
//
// 这组工具只在 Claude Code 的无显式工具请求中补齐，用来吸收 Cursor
// 上游仍可能返回的 shell/read/edit/ls/glob/grep/delete/task 等原生工具，
// 避免把本应可继续完成的会话中途打成 "undeclared tool"。
func claudeCodeCoreToolDefs() []ToolDef {
	return []ToolDef{
		{Name: "Bash", InputSchema: `{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`},
		{Name: "Read", InputSchema: `{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}`},
		{Name: "Edit", InputSchema: `{"type":"object","properties":{"file_path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["file_path","old_string","new_string"]}`},
		{Name: "Write", InputSchema: `{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}`},
		{Name: "TodoWrite", InputSchema: `{"type":"object","properties":{"todos":{"type":"array","items":{"type":"object"}}},"required":["todos"]}`},
		{Name: "Task", InputSchema: inferredClaudeAgentToolSchema},
		{Name: "LS", InputSchema: `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`},
		{Name: "Glob", InputSchema: `{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}`},
		{Name: "Grep", InputSchema: `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"glob":{"type":"string"},"case_insensitive":{"type":"boolean"}},"required":["pattern","path","glob"]}`},
		{Name: "Delete", InputSchema: `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`},
		{Name: "Agent", InputSchema: inferredClaudeAgentToolSchema},
	}
}

// claudeCodeCoreToolDefsForMerge 返回网关允许在请求中补齐的最小兼容集合。
// Claude Code 会根据 --tools、权限和 deferred tool 状态动态声明工具；网关
// 不能把 LS/Glob/Grep/Delete 等未声明工具伪造给 Cursor，否则上游一旦选择
// 它们，CLI 会以 "No such tool available" 拒绝整轮。Bash/Read/Agent 是
// 当前协议转换仍需兜底的稳定工具，其他工具必须以客户端本轮声明为准。
func claudeCodeCoreToolDefsForMerge() []ToolDef {
	all := claudeCodeCoreToolDefs()
	allowed := map[string]bool{"bash": true, "read": true, "agent": true}
	merged := make([]ToolDef, 0, len(allowed))
	for _, td := range all {
		if allowed[strings.ToLower(strings.TrimSpace(td.Name))] {
			merged = append(merged, td)
		}
	}
	return merged
}

func resolveClientTool(fieldNum int, native string, toolByLower map[string]ToolDef) (ToolDef, bool) {
	if td, ok := toolByLower[strings.ToLower(native)]; ok {
		return td, true
	}
	for _, a := range nativeAliases[fieldNum] {
		if td, ok := toolByLower[a]; ok {
			return td, true
		}
	}
	return ToolDef{}, false
}

// inferClaudeAgentTool 兼容 Claude Code 的动态工具目录。Claude Code 会把
// Skill/ToolSearch/DeferredToolPlaceholder 等工具按需放入请求，Agent/Task
// 可能暂时不在本轮 tools 数组中；Cursor 仍可能返回 native task(branch 19)。
// 只有同时满足 Claude 模型和至少两个 Claude Code 特征工具时才推断，避免
// 给普通 Anthropic/OpenAI 客户端凭空增加 Agent 能力。
func inferClaudeAgentTool(model string, tools []ToolDef) (ToolDef, bool) {
	for _, td := range tools {
		if strings.EqualFold(strings.TrimSpace(td.Name), "Agent") ||
			strings.EqualFold(strings.TrimSpace(td.Name), "Task") {
			return ToolDef{}, false
		}
	}
	lowerModel := strings.ToLower(strings.TrimSpace(model))
	if !strings.HasPrefix(lowerModel, "claude-") {
		return ToolDef{}, false
	}
	if claudeCodeToolMarkerCount(tools) < 2 {
		return ToolDef{}, false
	}
	return ToolDef{
		Name:        "Agent",
		Description: "Delegate a focused task to a Claude Code subagent.",
		InputSchema: inferredClaudeAgentToolSchema,
	}, true
}

func claudeCodeToolMarkerCount(tools []ToolDef) int {
	markers := map[string]bool{}
	for _, td := range tools {
		switch strings.ToLower(strings.TrimSpace(td.Name)) {
		case "skill", "toolsearch", "deferredtoolplaceholder", "taskoutput",
			"askuserquestion", "enterplanmode",
			"exitplanmode", "reportfindings", "workflow":
			markers[strings.ToLower(strings.TrimSpace(td.Name))] = true
		}
	}
	return len(markers)
}

func claudeCodeToolBridgeCount(tools []ToolDef) int {
	recognized := map[string]bool{}
	for _, td := range tools {
		if isClaudeCodeCoreOrMarkerToolName(td.Name) {
			name := strings.ToLower(strings.TrimSpace(td.Name))
			if name != "" {
				recognized[name] = true
			}
		}
	}
	return len(recognized)
}

// resolveDeclaredTaskTool 处理 Claude Code 在不同版本中对内置子任务工具使用
// Agent 或 Task 两个名称的差异。只有请求明确声明了其中一个名称时才做语义等价
// 映射，其他自定义工具仍保持严格按名称匹配，避免把未知工具伪装成子 Agent。
func resolveDeclaredTaskTool(name string, toolByLower map[string]ToolDef) (ToolDef, bool) {
	name = strings.TrimSpace(name)
	if td, ok := toolByLower[strings.ToLower(name)]; ok {
		return td, true
	}
	if strings.EqualFold(name, "Agent") {
		if td, ok := toolByLower["task"]; ok {
			return td, true
		}
	}
	if strings.EqualFold(name, "Task") {
		if td, ok := toolByLower["agent"]; ok {
			return td, true
		}
	}
	return ToolDef{}, false
}

func decodeNativeToolArgs(fieldNum int, branchData []byte, td ToolDef) json.RawMessage {
	// TodoWrite/TodoRead remain supported when the client explicitly declares them,
	// but they are intentionally not part of the default bridge set.
	if fieldNum == 9 && strings.EqualFold(strings.TrimSpace(td.Name), "TodoWrite") {
		return decodeNativeTodoWriteArgs(branchData)
	}
	bp, err := pbParse(branchData)
	if err != nil {
		return nil
	}
	// Cursor 原生工具存在两种 wire 形状：branch {1: args_message{...}}
	// 和 branch 直接携带参数字段。两者都归一化为参数消息再按 schema 解码。
	am := bp
	if argsMsg, ok := pbFirst(bp, 1); ok && argsMsg.Wire == 2 {
		if nested, nestedErr := pbParse(argsMsg.Data); nestedErr == nil && len(nested) > 0 {
			am = nested
		}
	}
	props := schemaRequiredProps(td.InputSchema)
	if len(props) == 0 {
		return nil
	}
	out := map[string]any{}
	if fieldNum == 12 {
		// Cursor 原生 EditArgs 只有 path(field 1) 与 stream_content(field 6)。
		// 不能再按下游 schema 的属性顺序绑定，否则 file_path/old_string/
		// new_string 会把 field 6 错误写入 old_string，进而触发客户端重复调用。
		pathProp := firstSchemaProp(props, "file_path", "path")
		newProp := firstSchemaProp(props, "new_string", "new", "content")
		if v, ok := pbFirst(am, 1); ok && v.Wire == 2 && pathProp != "" {
			out[pathProp] = string(v.Data)
		}
		if v, ok := pbFirst(am, 6); ok && v.Wire == 2 && newProp != "" {
			out[newProp] = string(v.Data)
		}
	} else if subs, ok := nativeArgSubfields[fieldNum]; ok {
		for i, sf := range subs {
			if i >= len(props) {
				break
			}
			if v, ok := pbFirst(am, sf); ok && v.Wire == 2 {
				out[props[i]] = string(v.Data)
			}
		}
	} else {
		vals := make([]string, 0, len(props))
		for _, f := range am {
			if len(vals) >= len(props) {
				break
			}
			if f.Wire == 2 && isTexty(f.Data) {
				vals = append(vals, string(f.Data))
			}
		}
		for i, p := range props {
			if i < len(vals) {
				out[p] = vals[i]
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return json.RawMessage(b)
}

func decodeNativeTodoWriteArgs(branchData []byte) json.RawMessage {
	outer, err := pbParse(branchData)
	if err != nil {
		return nil
	}
	args := outer
	if argsMsg, ok := pbFirst(outer, 1); ok && argsMsg.Wire == 2 {
		if nested, nestedErr := pbParse(argsMsg.Data); nestedErr == nil && len(nested) > 0 {
			args = nested
		}
	}
	todos := make([]map[string]any, 0, len(args))
	for idx, part := range args {
		if part.Wire != 2 {
			continue
		}
		values := collectTextLeafValues(part.Data)
		if len(values) == 0 {
			continue
		}
		item := map[string]any{"id": fmt.Sprintf("todo-%d", idx+1), "priority": "medium", "status": "pending", "content": values[0]}
		if len(values) > 1 {
			item["status"] = values[len(values)-1]
		}
		if len(values) > 2 {
			item["priority"] = values[0]
			item["content"] = values[1]
		}
		todos = append(todos, item)
	}
	if len(todos) == 0 {
		return nil
	}
	b, err := json.Marshal(map[string]any{"todos": todos})
	if err != nil {
		return nil
	}
	return json.RawMessage(b)
}

func collectTextLeafValues(data []byte) []string {
	parts, err := pbParse(data)
	if err != nil || len(parts) == 0 {
		if isTexty(data) {
			return []string{string(data)}
		}
		return nil
	}
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Wire != 2 {
			continue
		}
		if nested, nestedErr := pbParse(part.Data); nestedErr == nil && len(nested) > 0 {
			values = append(values, collectTextLeafValues(part.Data)...)
		} else if isTexty(part.Data) {
			values = append(values, string(part.Data))
		}
	}
	return values
}

// decodeNativeTaskToolArgs 解码 Cursor agent.v1 的 native task(branch 19)。
// 常见 wire 是 branch{1: args{...}}，但不同 Cursor 版本对可选字段使用过
// f5/f6(resume) 与 f7/f11(background) 两组标签；因此只读取稳定文本字段，
// 并对 background 同时接受 varint、"true"/"false" 和单字节长度字段。
func decodeNativeTaskToolArgs(branchData []byte, td ToolDef) json.RawMessage {
	outer, err := pbParse(branchData)
	if err != nil {
		return nil
	}
	args := outer
	if wrapper, ok := pbFirst(outer, 1); ok && wrapper.Wire == 2 {
		if nested, nestedErr := pbParse(wrapper.Data); nestedErr == nil && len(nested) > 0 {
			args = nested
		}
	}
	getText := func(fields ...int) string {
		for _, field := range fields {
			if p, ok := pbFirst(args, field); ok && p.Wire == 2 && isTexty(p.Data) {
				return strings.TrimSpace(string(p.Data))
			}
		}
		return ""
	}
	out := map[string]any{}
	allow := func(name string) bool {
		return taskToolSchemaAllows(td.InputSchema, name)
	}
	if value := getText(1); value != "" && allow("description") {
		out["description"] = value
	}
	if value := getText(2); value != "" && allow("prompt") {
		out["prompt"] = value
	}
	if value := getText(4); value != "" && allow("subagent_type") {
		if normalized := normalizeClaudeAgentSubagentType(value); normalized != "" {
			out["subagent_type"] = normalized
		}
	}
	if value := getText(3); value != "" && allow("model") {
		if normalized := normalizeClaudeAgentModel(value); normalized != "" {
			out["model"] = normalized
		}
	}
	if value := getText(5, 6); value != "" {
		// Claude Code Agent/Task 没有 Cursor 的 resume 字段，保留在中间
		// 结构会使严格客户端拒绝，因此只用于诊断，不向下游输出。
		_ = value
	}
	if background, ok := decodeNativeTaskBackground(args, 7, 11); ok && allow("run_in_background") {
		out["run_in_background"] = background
	}
	if len(out) == 0 {
		return nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return json.RawMessage(b)
}

func taskToolSchemaAllows(schema, name string) bool {
	var parsed struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if strings.TrimSpace(schema) == "" || json.Unmarshal([]byte(schema), &parsed) != nil || len(parsed.Properties) == 0 {
		return true
	}
	for prop := range parsed.Properties {
		if strings.EqualFold(strings.TrimSpace(prop), name) {
			return true
		}
	}
	return false
}

func decodeNativeTaskBackground(parts []pbPart, fields ...int) (bool, bool) {
	for _, field := range fields {
		p, ok := pbFirst(parts, field)
		if !ok {
			continue
		}
		if p.Wire == 0 {
			return p.Value != 0, true
		}
		if p.Wire != 2 {
			continue
		}
		raw := strings.TrimSpace(string(p.Data))
		if strings.EqualFold(raw, "true") || raw == "1" {
			return true, true
		}
		if strings.EqualFold(raw, "false") || raw == "0" {
			return false, true
		}
		if len(p.Data) == 1 && (p.Data[0] == 0 || p.Data[0] == 1) {
			return p.Data[0] == 1, true
		}
		if nested, err := pbParse(p.Data); err == nil {
			if value, ok := pbFirst(nested, 1); ok && value.Wire == 0 {
				return value.Value != 0, true
			}
		}
	}
	return false, false
}

func normalizeClaudeAgentModel(value string) string {
	lower := strings.ToLower(strings.TrimSpace(value))
	switch {
	case strings.Contains(lower, "sonnet"):
		return "sonnet"
	case strings.Contains(lower, "opus"):
		return "opus"
	case strings.Contains(lower, "haiku"):
		return "haiku"
	case strings.Contains(lower, "fable"):
		return "fable"
	default:
		switch lower {
		case "sonnet", "opus", "haiku", "fable":
			return lower
		default:
			return ""
		}
	}
}

func normalizeClaudeAgentSubagentType(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "claude-") || strings.HasPrefix(lower, "cursor-") ||
		strings.HasPrefix(lower, "gpt-") || strings.HasPrefix(lower, "gemini-") ||
		strings.HasPrefix(lower, "grok-") || strings.HasPrefix(lower, "fable-") {
		return "general-purpose"
	}
	return value
}

func firstSchemaProp(props []string, names ...string) string {
	for _, name := range names {
		for _, prop := range props {
			if strings.EqualFold(prop, name) {
				return prop
			}
		}
	}
	return ""
}

// completeNativeEditInput 将 Cursor 原生 EditArgs 转换为 Claude Code Edit 合同。
// Cursor 只发送完整的新文件内容，并不发送 old_string。只有同一请求历史中已经有
// 同路径 Read 工具的成功结果，才可以安全补齐 old_string；无法证明旧内容时失败，
// 避免向客户端伪造一个可能修改错误范围的替换指令。
func completeNativeEditInput(raw json.RawMessage, readFileContent map[string]string) (json.RawMessage, bool) {
	var input map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &input) != nil {
		return nil, false
	}
	readString := func(nonEmpty bool, names ...string) (string, bool) {
		for _, name := range names {
			value, ok := input[name]
			if !ok {
				continue
			}
			var text string
			if json.Unmarshal(value, &text) == nil && (!nonEmpty || text != "") {
				return text, true
			}
		}
		return "", false
	}
	path, ok := readString(true, "file_path", "path")
	if !ok {
		return nil, false
	}
	newContent, ok := readString(false, "new_string", "new")
	if !ok {
		return nil, false
	}
	oldContent, ok := readFileContent[path]
	if !ok {
		oldContent, ok = readFileContent[filepath.Clean(path)]
	}
	if !ok {
		return nil, false
	}
	result, err := json.Marshal(map[string]string{
		"file_path":  path,
		"old_string": oldContent,
		"new_string": newContent,
	})
	if err != nil {
		return nil, false
	}
	return json.RawMessage(result), true
}

func isClaudeEditToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "edit", "edit_file", "editfile", "str_replace", "str_replace_editor", "apply_patch":
		return true
	default:
		return false
	}
}

func schemaRequiredProps(schema string) []string {
	if strings.TrimSpace(schema) == "" {
		return nil
	}
	var s struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal([]byte(schema), &s) != nil {
		return nil
	}
	if len(s.Required) > 0 {
		return s.Required
	}
	keys := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys) // 稳定顺序: 消除 Go map 迭代乱序导致的内置工具参数错绑
	return keys
}

func appendToolCall(list []ToolCall, tc ToolCall) []ToolCall {
	for i, existing := range list {
		if existing.ID != "" && existing.ID == tc.ID {
			if tc.Name != "" && !strings.HasPrefix(tc.Name, "tool_") {
				list[i].Name = tc.Name
			}
			if len(tc.Input) > 0 && string(tc.Input) != "{}" {
				list[i].Input = tc.Input
			}
			return list
		}
	}
	return append(list, tc)
}

func isTexty(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	printable := 0
	for _, c := range b {
		if c == '\n' || c == '\t' || c == '\r' || (c >= 0x20 && c < 0x7f) || c >= 0x80 {
			printable++
		}
	}
	return printable*10 >= len(b)*8
}

// describePBParts 输出仅用于调试的 protobuf 结构摘要。它绝不输出文本、token 或原始字段值，
// 以免真实请求的系统提示、工具参数、模型输出或账号相关数据进入服务日志。
func describePBParts(parts []pbPart, depth int) string {
	if len(parts) == 0 {
		return "-"
	}
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteString(", ")
		}
		if b.Len() > 1200 {
			b.WriteString("…")
			break
		}
		switch p.Wire {
		case 0:
			fmt.Fprintf(&b, "f%d=%d", p.Num, p.Value)
		case 1, 5:
			fmt.Fprintf(&b, "f%d<w%d bytes=%d>", p.Num, p.Wire, len(p.Data))
		case 2:
			if len(p.Data) == 0 {
				fmt.Fprintf(&b, "f%d<bytes=0>", p.Num)
				continue
			}
			if depth < 2 {
				if nested, err := pbParse(p.Data); err == nil && len(nested) > 0 {
					fmt.Fprintf(&b, "f%d={%s}", p.Num, describePBParts(nested, depth+1))
					continue
				}
			}
			if isTexty(p.Data) {
				fmt.Fprintf(&b, "f%d<text bytes=%d>", p.Num, len(p.Data))
				continue
			}
			fmt.Fprintf(&b, "f%d<bytes=%d>", p.Num, len(p.Data))
		default:
			fmt.Fprintf(&b, "f%d<w%d>", p.Num, p.Wire)
		}
	}
	return b.String()
}

// toolWirePrefix 给发往 Cursor 的工具名加命名空间前缀, 避免与内置客户端工具重名(重名会让
// 服务端等待真实 IDE 执行而永久挂起); 收到调用时再去前缀还原客户端原始名。
const toolWirePrefix = "ccx_"

func toolWireName(tool ToolDef) string {
	name := strings.TrimSpace(tool.Name)
	if strings.EqualFold(name, "Agent") || strings.EqualFold(name, "Task") {
		// Cursor agent.v1 目前识别的自定义委派工具 wire 名称是 Agent。
		// Claude Code 不同版本可能把客户端工具名称为 Task 或 Agent，
		// 但不能把尚未被 Cursor 接受的 Task wire 名称直接发给上游。
		return "Agent"
	}
	return toolWirePrefix + name
}

// ── 业务层入口（sub2api 新增）─────────────────────────────────────────

// BuildAgentMessage 把多轮会话拍平为 AgentRequest.Message。
//
// ⚠️ 不要在 service 层另写一份拍平逻辑。这里的格式（中文角色标签、
// [已验证的会话历史开始] 边界、重复进度句的额外约束）是针对 agent.v1
// 单轮协议的真实问题调出来的：用英文 "User:/Assistant:" 脚本格式会让模型
// 把历史误判为用户粘贴的伪造 transcript 而拒答或重复声明"未执行过工具"。
func BuildAgentMessage(msgs []ChatMessage, tools []ToolDef) string {
	return buildAgentMessageForTools(msgs, tools)
}

// BuildAgentSystemPrompt 返回仅供网关内部使用的系统提示文本
// （token 估算、亲和键、诊断）。
//
// ⚠️ 普通 Cursor 账号不支持 AgentRunRequest.custom_system_prompt，
// 该字段会被上游解析成 CLI --system-prompt 并返回 invalid_argument。
// 因此它不会被编码进上游请求，调用方也不要尝试塞进去。
func BuildAgentSystemPrompt(sys string, tools []ToolDef) string {
	return buildAgentSystemPrompt(sys, tools)
}

// BuildUpstreamClientMessageForTest 返回实际发往上游的 agent.v1 请求字节，
// 仅供测试断言使用（例如验证 system 不会被编码进上游请求）。
func BuildUpstreamClientMessageForTest(in AgentRequest) []byte {
	model := in.Model
	if model == "" {
		model = "default"
	}
	return buildAgentClientMessage(model, in)
}
