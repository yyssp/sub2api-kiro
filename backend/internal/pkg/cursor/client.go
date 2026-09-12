package cursor

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	streamChatURL     = "/aiserver.v1.ChatService/StreamUnifiedChatWithTools" // 已退休(Update Required), 仅保留常量
	modelsURL         = "/aiserver.v1.AiService/AvailableModels"
	userMetaURL       = "/aiserver.v1.AuthService/GetUserMeta"
	startSandTrialURL = "/aiserver.v1.DashboardService/StartSandTrial"

	// Cursor 3.x AgentService 同时提供两种传输面：
	// Run 是默认的 HTTP/2/全双工双向流；RunSSE + BidiAppend 是 HTTP/1.1 回退。
	// RunSSE 走独立 agent 网关主机, BidiAppend 走 api2 主机。
	runURL        = "/agent.v1.AgentService/Run"
	runSSEURL     = "/agent.v1.AgentService/RunSSE"
	bidiAppendURL = "/aiserver.v1.BidiService/BidiAppend"
)

// 客户端版本/commit 必须是"真实存在"的版本, 否则上游走慢降级路径甚至 Update Required。
// 默认取写死时的最新真实版本(3.15.6, 2026-08-06); 可用环境变量热调, 无需改代码重编:
//
//	CURSOR_CLIENT_VERSION / CURSOR_CLIENT_COMMIT / CURSOR_AGENT_BASE
var (
	clientVersion = envOr("CURSOR_CLIENT_VERSION", "3.15.6")
	clientCommit  = envOr("CURSOR_CLIENT_COMMIT", "a1f686545fd0ce8917bbd2449f733551a9bce420")
	// 非 global 主机(agentn.api5)实测比 agentn.global.api5 稳定快 ~1.5s, 作默认。
	agentBaseURL = envOr("CURSOR_AGENT_BASE", "https://agentn.api5.cursor.sh")
)

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// Client Cursor 上游客户端。每账号独立连接池, 避免共享 MAX_CONCURRENT_STREAMS 互相饿死。
type Client struct {
	mu           sync.Mutex
	perCred      map[int64]*http.Client
	shared       *http.Client
	agentTimeout time.Duration // 单轮对话硬上限
	firstToken   time.Duration // 首字硬墙(无任何真实产出即快速失败转移)
}

func NewClient() *Client {
	return &Client{
		perCred:      map[int64]*http.Client{},
		shared:       newH2Client(),
		agentTimeout: 600 * time.Second,
		firstToken:   90 * time.Second,
	}
}

func newH2Client() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 60 * time.Second}).DialContext,
			ForceAttemptHTTP2:   true,
			MaxIdleConns:        512,
			MaxIdleConnsPerHost: 128,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 15 * time.Second,
			TLSClientConfig:     &tls.Config{},
		},
		Timeout: 600 * time.Second,
	}
}

func (c *Client) clientFor(a *Account) *http.Client {
	if a == nil || a.ID == 0 {
		return c.shared
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl, ok := c.perCred[a.ID]; ok {
		return cl
	}
	cl := newH2Client()
	c.perCred[a.ID] = cl
	return cl
}

func (c *Client) buildHeaders(a *Account, contentType string) http.Header {
	return c.buildHeadersWithType(a, contentType, "sand")
}

// buildHeadersWithType 构造 Cursor 请求头，并允许调用方按上游 HTTP 协议指定
// x-cursor-client-type。历史上的普通 unary/旧端点继续使用 sand；AgentService
// 的 Claude Code 兼容路径由 agent.go 显式传入 cli。不要把调度/记账额度面
// 与这里的上游客户端身份混用。
func (c *Client) buildHeadersWithType(a *Account, contentType, clientType string) http.Header {
	tok := a.AccessToken
	machID := machineID(a)
	macID := macMachineID(a)
	h := http.Header{}
	h.Set("Content-Type", contentType)
	h.Set("Connect-Protocol-Version", "1")
	h.Set("Connect-Accept-Encoding", "gzip")
	h.Set("Authorization", "Bearer "+tok)
	h.Set("x-cursor-checksum", genChecksum(machID, macID))
	h.Set("x-cursor-client-version", clientVersion)
	if strings.TrimSpace(clientType) == "" {
		clientType = "sand"
	}
	h.Set("x-cursor-client-type", clientType)
	h.Set("x-cursor-client-commit", clientCommit)
	h.Set("x-ghost-mode", "true")
	h.Set("x-cursor-client-device-type", "desktop")
	h.Set("x-cursor-client-os", "windows")
	h.Set("x-cursor-client-arch", "x64")
	h.Set("x-cursor-timezone", "Asia/Shanghai")
	h.Set("x-new-onboarding-completed", "true")
	h.Set("x-client-key", clientKey(tok))
	h.Set("x-session-id", sessionID(tok))
	h.Set("x-request-id", genUUID())
	h.Set("User-Agent", "Cursor/"+clientVersion)
	return h
}

// ---------- 请求消息构造(aiserver.v1.StreamUnifiedChatRequestWithTools) ----------

type ChatMessage struct {
	Role      string
	Content   string
	Images    []ImageAttachment
	Documents []DocumentAttachment
}

// ImageAttachment 是从 Anthropic/OpenAI 内容块解析出的真实图片数据。
// Cursor agent.v1 通过 UserMessage.selected_context.selected_images 承载图片。
type ImageAttachment struct {
	Data     []byte
	MIMEType string
	Width    int
	Height   int
	UUID     string
	Path     string
}

// DocumentAttachment 是来自 Anthropic document / OpenAI file 内容块的文件。
// Cursor agent.v1 当前确认的 selected context 仅包含图片；文本文件/PDF 的提取正文
// 会由请求构造层放入带边界标记的用户文本，无法解码的二进制只保留能力提示。
type DocumentAttachment struct {
	Data     []byte
	Text     string
	MIMEType string
	Filename string
	Path     string
	URL      string
	IsPDF    bool
}

type ToolDef struct {
	Name        string
	Description string
	InputSchema string // JSON schema 字符串
}

const (
	msgTypeHuman = 1
	msgTypeAI    = 2

	unifiedModeChat  = 1
	unifiedModeAgent = 2
)

type convMsg struct {
	Text     string `json:"text"`
	Type     int    `json:"type"`
	BubbleID string `json:"bubbleId"`
}

type modelDetails struct {
	ModelName       string `json:"modelName"`
	EnableGhostMode bool   `json:"enableGhostMode"`
	MaxMode         bool   `json:"maxMode,omitempty"`
}

type mcpTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  string `json:"parameters"`
	ServerName  string `json:"serverName"`
}

type unifiedChatReq struct {
	Conversation      []convMsg    `json:"conversation"`
	ModelDetails      modelDetails `json:"modelDetails"`
	ConversationID    string       `json:"conversationId"`
	IsChat            bool         `json:"isChat"`
	UnifiedMode       int          `json:"unifiedMode"`
	IsAgentic         bool         `json:"isAgentic,omitempty"`
	SupportedTools    []int        `json:"supportedTools,omitempty"`
	MCPTools          []mcpTool    `json:"mcpTools,omitempty"`
	HasMCPDescriptors bool         `json:"hasMcpDescriptors,omitempty"`
}

type withToolsReq struct {
	StreamUnifiedChatRequest unifiedChatReq `json:"streamUnifiedChatRequest"`
}

func roleType(role string) int {
	if role == "assistant" {
		return msgTypeAI
	}
	return msgTypeHuman
}

// buildRequestJSON 构造聊天请求体 JSON
func buildRequestJSON(model string, msgs []ChatMessage, tools []ToolDef, maxMode bool) ([]byte, error) {
	conv := make([]convMsg, 0, len(msgs))
	for _, m := range msgs {
		conv = append(conv, convMsg{Text: m.Content, Type: roleType(m.Role), BubbleID: genUUID()})
	}
	inner := unifiedChatReq{
		Conversation:   conv,
		ModelDetails:   modelDetails{ModelName: model, EnableGhostMode: true, MaxMode: maxMode},
		ConversationID: genUUID(),
		IsChat:         true,
		UnifiedMode:    unifiedModeChat,
	}
	if len(tools) > 0 {
		inner.IsChat = false
		inner.IsAgentic = true
		inner.UnifiedMode = unifiedModeAgent
		inner.SupportedTools = []int{49} // CLIENT_SIDE_TOOL_V2_CALL_MCP_TOOL
		inner.HasMCPDescriptors = true
		inner.MCPTools = make([]mcpTool, 0, len(tools))
		for _, t := range tools {
			params := t.InputSchema
			if strings.TrimSpace(params) == "" {
				params = "{}"
			}
			inner.MCPTools = append(inner.MCPTools, mcpTool{
				Name: t.Name, Description: t.Description, Parameters: params, ServerName: "anthropic",
			})
		}
	}
	return json.Marshal(withToolsReq{StreamUnifiedChatRequest: inner})
}

// wrapFrame 用 Connect 5 字节帧头封装(flag=0 未压缩数据帧)
func wrapFrame(data []byte) []byte {
	buf := make([]byte, 5+len(data))
	buf[0] = 0
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(data)))
	copy(buf[5:], data)
	return buf
}

// StreamChat 发送流式聊天请求
func (c *Client) StreamChat(a *Account, model string, msgs []ChatMessage, tools []ToolDef) (*http.Response, error) {
	body, err := buildRequestJSON(model, msgs, tools, false)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", cursorAPIURL(streamChatURL), bytes.NewReader(wrapFrame(body)))
	if err != nil {
		return nil, err
	}
	req.Header = c.buildHeaders(a, "application/connect+json")
	applyCursorProxyAuth(req)
	return c.clientFor(a).Do(req)
}

// ---------- 响应帧读取与解析 ----------

// StreamReader 逐帧读取 Connect-RPC 流
type StreamReader struct {
	body io.ReadCloser
}

func NewStreamReader(body io.ReadCloser) *StreamReader { return &StreamReader{body: body} }

// ReadFrame 读取一帧: flag bit0=gzip, bit1=end-of-stream。压缩帧自动解压并清标志位。
func (sr *StreamReader) ReadFrame() (byte, []byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(sr.body, header); err != nil {
		return 0, nil, err
	}
	flag := header[0]
	length := binary.BigEndian.Uint32(header[1:5])
	if length > 50*1024*1024 {
		return 0, nil, fmt.Errorf("帧大小超限: %d", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(sr.body, payload); err != nil {
		return 0, nil, err
	}
	if flag&0x01 != 0 {
		if zr, err := gzip.NewReader(bytes.NewReader(payload)); err == nil {
			if dec, derr := io.ReadAll(zr); derr == nil {
				payload = dec
				flag &^= 0x01
			}
			zr.Close()
		}
	}
	return flag, payload, nil
}

func (sr *StreamReader) Close() error { return sr.body.Close() }

// ToolCall 解析出的工具调用
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ChatFrame 一帧解析结果
type ChatFrame struct {
	Text     string
	ToolCall *ToolCall
}

type frameResp struct {
	ClientSideToolV2Call *struct {
		ToolCallID        string          `json:"toolCallId"`
		Name              string          `json:"name"`
		RawArgs           json.RawMessage `json:"rawArgs"`
		CallMCPToolParams *struct {
			ToolName string          `json:"toolName"`
			ToolArgs json.RawMessage `json:"toolArgs"`
		} `json:"callMcpToolParams"`
	} `json:"clientSideToolV2Call"`
	StreamUnifiedChatResponse struct {
		Text string `json:"text"`
	} `json:"streamUnifiedChatResponse"`
	Text string `json:"text"`
}

func parseFrame(payload []byte) ChatFrame {
	var r frameResp
	if err := json.Unmarshal(payload, &r); err != nil {
		return ChatFrame{}
	}
	f := ChatFrame{Text: r.StreamUnifiedChatResponse.Text}
	if f.Text == "" {
		f.Text = r.Text
	}
	if call := r.ClientSideToolV2Call; call != nil {
		name, input := call.Name, normalizeToolInput(call.RawArgs)
		if call.CallMCPToolParams != nil {
			if call.CallMCPToolParams.ToolName != "" {
				name = call.CallMCPToolParams.ToolName
			}
			if len(call.CallMCPToolParams.ToolArgs) > 0 {
				input = normalizeToolInput(call.CallMCPToolParams.ToolArgs)
			}
		}
		f.ToolCall = &ToolCall{ID: call.ToolCallID, Name: name, Input: input}
	}
	return f
}

func normalizeToolInput(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil && json.Valid([]byte(encoded)) {
		return json.RawMessage(encoded)
	}
	if json.Valid(raw) {
		return raw
	}
	return json.RawMessage(`{}`)
}

// parseStreamError 解析 end-stream 帧错误
func parseStreamError(payload []byte) string {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details []struct {
				Debug struct {
					Error   string `json:"error"`
					Details struct {
						Title  string `json:"title"`
						Detail string `json:"detail"`
					} `json:"details"`
				} `json:"debug"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &e); err != nil {
		return ""
	}
	if len(e.Error.Details) > 0 {
		d := e.Error.Details[0].Debug
		if d.Details.Detail != "" {
			code := d.Error
			if code == "" {
				code = e.Error.Code
			}
			msg := d.Details.Detail
			if d.Details.Title != "" {
				msg = d.Details.Title + " - " + msg
			}
			return code + ": " + msg
		}
	}
	if e.Error.Code != "" || e.Error.Message != "" {
		return strings.TrimPrefix(e.Error.Code+": "+e.Error.Message, ": ")
	}
	return ""
}

// ListModels 拉取账号可用模型
func (c *Client) ListModels(a *Account) ([]string, error) {
	req, err := http.NewRequest("POST", cursorAPIURL(modelsURL), bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header = c.buildHeaders(a, "application/json")
	applyCursorProxyAuth(req)
	resp, err := c.clientFor(a).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("AvailableModels: HTTP %d %s", resp.StatusCode, string(body))
	}
	var r struct {
		ModelNames []string `json:"modelNames"`
		Models     []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if len(r.ModelNames) > 0 {
		return r.ModelNames, nil
	}
	var out []string
	for _, m := range r.Models {
		if m.Name != "" {
			out = append(out, m.Name)
		}
	}
	return out, nil
}

// ListModelsFull 拉取账号可用模型的完整元信息(动态模型清单来源)。
func (c *Client) ListModelsFull(a *Account) ([]ModelMeta, error) {
	req, err := http.NewRequest("POST", cursorAPIURL(modelsURL), bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header = c.buildHeaders(a, "application/json")
	applyCursorProxyAuth(req)
	resp, err := c.clientFor(a).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("AvailableModels: HTTP %d %s", resp.StatusCode, string(body))
	}
	var r struct {
		Models []struct {
			Name             string `json:"name"`
			SupportsImages   bool   `json:"supportsImages"`
			SupportsThinking bool   `json:"supportsThinking"`
			SupportsAgent    bool   `json:"supportsAgent"`
			SupportsMaxMode  bool   `json:"supportsMaxMode"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	out := make([]ModelMeta, 0, len(r.Models))
	for _, m := range r.Models {
		if m.Name == "" {
			continue
		}
		out = append(out, ModelMeta{
			ID: m.Name, Family: familyOf(m.Name),
			Vision: m.SupportsImages, Tools: m.SupportsAgent,
			Think: m.SupportsThinking, MaxMode: m.SupportsMaxMode,
		})
	}
	return out, nil
}

// RefreshModelsFrom 用指定账号拉取动态模型清单并刷新全局缓存。
func RefreshModelsFrom(c *Client, acc *Account) (int, error) {
	ms, err := c.ListModelsFull(acc)
	if err != nil {
		return 0, err
	}
	if len(ms) > 0 {
		SetLiveModels(ms)
	}
	return len(ms), nil
}

// ⚠️ 已移除 RefreshModels(c, *Store)：依赖 ai2api JSON 号池。
// sub2api 侧用 RefreshModelsFrom(c, *Account) 取模型清单，由 service 层缓存进 Redis。

// GetUserMeta 拉取账号邮箱
func (c *Client) GetUserMeta(a *Account) (string, error) {
	req, err := http.NewRequest("POST", cursorAPIURL(userMetaURL), bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	req.Header = c.buildHeaders(a, "application/json")
	applyCursorProxyAuth(req)
	resp, err := c.clientFor(a).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("GetUserMeta: HTTP %d", resp.StatusCode)
	}
	var meta struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return "", err
	}
	return meta.Email, nil
}

// HealthCheck 用 ListModels 验证凭证
func (c *Client) HealthCheck(a *Account) error {
	_, err := c.ListModels(a)
	return err
}

// StartSandTrial 激活 Sand 试用($250 FREE_CREDIT), 使 sand 路由(cursor 模型)有额度可用。
// 端点: POST /aiserver.v1.DashboardService/StartSandTrial, 空 body {}; 复用 buildHeaders(已带 x-cursor-client-type: sand)。
// 幂等: 已激活账号返回 HTTP 400 "already funded" 等, 由上层归类为成功。
func (c *Client) StartSandTrial(a *Account) (string, error) {
	req, err := http.NewRequest("POST", cursorAPIURL(startSandTrialURL), bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	req.Header = c.buildHeaders(a, "application/json")
	applyCursorProxyAuth(req)
	resp, err := c.clientFor(a).Do(req)
	if err != nil {
		return "", fmt.Errorf("StartSandTrial: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("StartSandTrial: HTTP %d %s", resp.StatusCode, string(body))
	}
	return string(body), nil
}

// PeriodUsage 当前计费周期三类额度快照。auto=Cursor Models(Grok/Composer), api=Other Models(非 Claude 的第三方 named models);
// Sand/GrokBot 额度由 GetSandUsage 单独回填。
type PeriodUsage struct {
	OK             bool
	TotalSpend     float64
	IncludedSpend  float64
	BonusSpend     float64
	Limit          float64
	AutoPercent    float64 // Cursor Models 使用率
	APIPercent     float64 // Other Models 使用率
	TotalPercent   float64
	DisplayMessage string
}

// GetCurrentPeriodUsage 拉取当前周期三类额度(现役 dashboard 额度体系)。
// 端点: POST /aiserver.v1.DashboardService/GetCurrentPeriodUsage, 空 body, 复用 buildHeaders(Bearer+checksum)。
func (c *Client) GetCurrentPeriodUsage(a *Account) PeriodUsage {
	var pu PeriodUsage
	req, err := http.NewRequest("POST", cursorAPIURL("/aiserver.v1.DashboardService/GetCurrentPeriodUsage"), bytes.NewReader([]byte("{}")))
	if err != nil {
		logSandUsageDebug(a, "period request build failed: %v", err)
		return pu
	}
	req.Header = c.buildHeaders(a, "application/json")
	applyCursorProxyAuth(req)
	resp, err := c.clientFor(a).Do(req)
	if err != nil {
		logSandUsageDebug(a, "period request failed: %v", err)
		return pu
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		logSandUsageDebug(a, "period response status=%d content_type=%q bytes=%d", resp.StatusCode, resp.Header.Get("Content-Type"), len(body))
		return pu
	}
	var r struct {
		PlanUsage struct {
			TotalSpend       float64 `json:"totalSpend"`
			IncludedSpend    float64 `json:"includedSpend"`
			BonusSpend       float64 `json:"bonusSpend"`
			Limit            float64 `json:"limit"`
			AutoPercentUsed  float64 `json:"autoPercentUsed"`
			APIPercentUsed   float64 `json:"apiPercentUsed"`
			TotalPercentUsed float64 `json:"totalPercentUsed"`
		} `json:"planUsage"`
		DisplayMessage string `json:"displayMessage"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		logSandUsageDebug(a, "period response parse failed status=%d content_type=%q bytes=%d err=%v", resp.StatusCode, resp.Header.Get("Content-Type"), len(body), err)
		return pu
	}
	logSandUsageDebug(a, "period response status=%d content_type=%q bytes=%d fields=planUsage", resp.StatusCode, resp.Header.Get("Content-Type"), len(body))
	pu.OK = true
	pu.TotalSpend = r.PlanUsage.TotalSpend
	pu.IncludedSpend = r.PlanUsage.IncludedSpend
	pu.BonusSpend = r.PlanUsage.BonusSpend
	pu.Limit = r.PlanUsage.Limit
	pu.AutoPercent = r.PlanUsage.AutoPercentUsed
	pu.APIPercent = r.PlanUsage.APIPercentUsed
	pu.TotalPercent = r.PlanUsage.TotalPercentUsed
	pu.DisplayMessage = r.DisplayMessage
	return pu
}

// SandUsage Grok Bot Trial(Sand)额度。
type SandUsage struct {
	OK                  bool
	UsagePercent        float64
	UsagePercentPresent bool
	HasAvailable        bool
	HasAvailablePresent bool
	State               string
	HTTPStatus          int
	Error               string
}

// EffectiveState returns a non-fabricated Sand state. A 200 response without
// entitlement fields is unknown, not "0% used and unavailable".
func (su SandUsage) EffectiveState() string {
	switch su.State {
	case sandStateAvailable, sandStateExhausted, sandStateUnavailable, sandStateRequestFailed, sandStateUnknown:
		return su.State
	}
	if !su.OK {
		return sandStateRequestFailed
	}
	// Cursor 偶尔会在额度已到 100% 时仍返回 hasAvailableUsage=true。
	// 百分比是已用额度的硬上限，优先判定为 exhausted，避免把已耗尽
	// 的账号重新放回 Sand 候选集并反复触发上游 quota 错误。
	if su.UsagePercentPresent && su.UsagePercent >= 100 {
		return sandStateExhausted
	}
	if su.HasAvailable {
		return sandStateAvailable
	}
	if su.HasAvailablePresent || su.UsagePercentPresent {
		return sandStateUnavailable
	}
	return sandStateUnknown
}

// GetSandUsage 拉取 Grok Bot Trial(Sand)额度使用率。
// 端点: POST /aiserver.v1.DashboardService/GetSandUsageStatus, 空 body。
func (c *Client) GetSandUsage(a *Account) SandUsage {
	var su SandUsage
	req, err := http.NewRequest("POST", cursorAPIURL("/aiserver.v1.DashboardService/GetSandUsageStatus"), bytes.NewReader([]byte("{}")))
	if err != nil {
		su.Error = err.Error()
		logSandUsageDebug(a, "request build failed: %v", err)
		return su
	}
	req.Header = c.buildHeaders(a, "application/json")
	applyCursorProxyAuth(req)
	resp, err := c.clientFor(a).Do(req)
	if err != nil {
		su.Error = err.Error()
		logSandUsageDebug(a, "request failed: %v", err)
		return su
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	su.HTTPStatus = resp.StatusCode
	if resp.StatusCode != 200 {
		su.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		logSandUsageDebug(a, "response status=%d content_type=%q bytes=%d", resp.StatusCode, resp.Header.Get("Content-Type"), len(body))
		return su
	}
	var r struct {
		UsagePercent      float64 `json:"usagePercent"`
		HasAvailableUsage bool    `json:"hasAvailableUsage"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		su.Error = err.Error()
		logSandUsageDebug(a, "response parse failed status=%d content_type=%q bytes=%d err=%v", resp.StatusCode, resp.Header.Get("Content-Type"), len(body), err)
		return su
	}
	su.OK = true
	su.UsagePercent = r.UsagePercent
	su.HasAvailable = r.HasAvailableUsage
	su.UsagePercentPresent = jsonFieldPresent(body, "usagePercent")
	su.HasAvailablePresent = jsonFieldPresent(body, "hasAvailableUsage")
	su.State = su.EffectiveState()
	logSandUsageDebug(a, "response status=%d content_type=%q bytes=%d usagePercent_present=%v hasAvailableUsage_present=%v usagePercent=%.6f hasAvailable=%v",
		resp.StatusCode, resp.Header.Get("Content-Type"), len(body),
		su.UsagePercentPresent, su.HasAvailablePresent,
		su.UsagePercent, su.HasAvailable)
	return su
}

func jsonFieldPresent(body []byte, field string) bool {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return false
	}
	_, ok := raw[field]
	return ok
}

func logSandUsageDebug(a *Account, format string, args ...interface{}) {
	if !agentDebug {
		return
	}
	var id int64
	if a != nil {
		id = a.ID
	}
	log.Printf("[CursorSand] account=%d "+format, append([]interface{}{id}, args...)...)
}
