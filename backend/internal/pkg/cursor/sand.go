package cursor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	sandStreamURL = "/aiserver.v1.InferenceService/Stream"

	sandRoleUser      = 1
	sandRoleAssistant = 2
	sandRoleTool      = 3
	sandRoleSystem    = 4
)

// buildSandInferenceRequest constructs aiserver.v1.InferenceStreamRequest.
// This is a different protobuf message from agent.v1.AgentService/Run; the
// two wire formats must not be mixed.
func buildSandInferenceRequest(model string, in AgentRequest, conversationID string) []byte {
	resolvedModel := sandResolvedModel(model)
	selection := parseSandModelSelection(resolvedModel)
	wireModel := selection.ModelID
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		conversationID = genUUID()
	}
	var body []byte
	if system := strings.TrimSpace(in.System); system != "" {
		body = append(body, protoMessage(1, buildSandMessage(sandRoleSystem, system))...)
	}
	if message := in.Message; message != "" {
		body = append(body, protoMessage(1, buildSandMessage(sandRoleUser, message))...)
	}
	// Cursor 的 Sand InferenceService/Stream 不是 Anthropic tool schema 的透传面。
	// 真实 CLI 请求里把 Claude Code 工具定义写入 field 2 会触发 provider error，
	// 参考 Cursor Sand 客户端也通过本地 promptToolSession 桥接工具，而不是在
	// InferenceStreamRequest 中声明 Anthropic 工具。
	// InferenceService/Stream 的 field 7 是 requested_model 消息，而不是裸
	// model_id 字符串。Sand 客户端直连逻辑会读取 requestedModel.modelId 与
	// requestedModel.parameters；以字符串编码会被上游按错误 schema 拒绝。
	// field 8 是 conversation_id；缺失时上游返回 invalid_argument。
	body = append(body, protoString(8, conversationID)...)
	body = append(body, protoMessage(7, buildSandRequestedModelWithParameters(
		wireModel, selection.Parameters, selection.MaxMode || in.MaxMode,
	))...)
	return body
}

type sandModelParameter struct {
	ID    string
	Value string
}

// buildSandRequestedModel 编码 Sand InferenceStreamRequest.requested_model：
//
//	message RequestedModel {
//	  string model_id = 1;
//	  optional bool max_mode = 2;
//	  repeated ModelParameter parameters = 3;
//	}
//
// 参数只从 Cursor 模型目录 ID 推导，不能把 Anthropic 生成参数伪装成 Cursor
// requested_model 参数。即使没有参数，也必须发送该嵌套消息本身。
func buildSandRequestedModel(modelID, parameterSource string, maxMode bool) []byte {
	selection := parseSandModelSelection(parameterSource)
	return buildSandRequestedModelWithParameters(
		modelID, selection.Parameters, maxMode || selection.MaxMode,
	)
}

func buildSandRequestedModelWithParameters(modelID string, parameters []sandModelParameter, maxMode bool) []byte {
	requested := protoString(1, modelID)
	// Cursor 3.21.x 的显式 context 变体不允许同时声明 max_mode=true。
	// 这同时覆盖 1m、200k、272k、300k 及未来目录新增的 context 值。
	if maxMode && !sandHasParameterID(parameters, "context") {
		requested = append(requested, protoBool(2, true)...)
	}
	for _, parameter := range parameters {
		item := protoString(1, parameter.ID)
		item = append(item, protoString(2, parameter.Value)...)
		requested = append(requested, protoMessage(3, item)...)
	}
	return requested
}

type sandModelSelection struct {
	ModelID    string
	Parameters []sandModelParameter
	MaxMode    bool
}

var sandModelBracketRE = regexp.MustCompile(`^(.+?)\[([^\]]*)\]$`)

// parseSandModelSelection accepts both legacy slugs
// (claude-opus-5-thinking-high) and bracket spelling
// (claude-opus-5[effort=high,context=1m]).
func parseSandModelSelection(model string) sandModelSelection {
	raw := strings.TrimSpace(model)
	if raw == "" {
		return sandModelSelection{}
	}
	var explicit []sandModelParameter
	if match := sandModelBracketRE.FindStringSubmatch(raw); len(match) == 3 {
		raw = strings.TrimSpace(match[1])
		for _, item := range strings.Split(match[2], ",") {
			parts := strings.SplitN(strings.TrimSpace(item), "=", 2)
			if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
				continue
			}
			explicit = append(explicit, sandModelParameter{
				ID: strings.TrimSpace(parts[0]), Value: strings.TrimSpace(parts[1]),
			})
		}
	}
	parameters := explicit
	catalogMaxMode := false
	if len(parameters) == 0 {
		parameters, catalogMaxMode = sandModelCatalogSelection(raw)
	}
	if len(parameters) == 0 {
		parameters = sandRequestedModelParameters(raw)
	}
	return sandModelSelection{
		ModelID:    sandCatalogModelID(raw),
		Parameters: parameters,
		MaxMode: catalogMaxMode ||
			sandHasParameter(parameters, "effort", "max") ||
			sandHasParameter(parameters, "reasoning", "max"),
	}
}

func sandHasParameter(parameters []sandModelParameter, id, value string) bool {
	for _, parameter := range parameters {
		if strings.EqualFold(strings.TrimSpace(parameter.ID), id) &&
			strings.EqualFold(strings.TrimSpace(parameter.Value), value) {
			return true
		}
	}
	return false
}

func sandHasParameterID(parameters []sandModelParameter, id string) bool {
	for _, parameter := range parameters {
		if strings.EqualFold(strings.TrimSpace(parameter.ID), id) {
			return true
		}
	}
	return false
}

func sandModelCatalogParameters(model string) []sandModelParameter {
	parameters, _ := sandModelCatalogSelection(model)
	return parameters
}

func sandModelCatalogSelection(model string) ([]sandModelParameter, bool) {
	if strings.TrimSpace(model) == "" {
		return nil, false
	}
	for _, meta := range claudeResolutionModels() {
		for _, alias := range meta.Aliases {
			if strings.EqualFold(alias, model) {
				return modelParameterValuesToSand(meta.Defaults.NonMax), false
			}
		}
		if strings.EqualFold(meta.ID, model) && len(meta.Defaults.NonMax) > 0 {
			return modelParameterValuesToSand(meta.Defaults.NonMax), false
		}
		for _, variant := range meta.Variants {
			if strings.EqualFold(variant.Slug, model) {
				return modelParameterValuesToSand(variant.Parameters), variant.MaxMode
			}
		}
	}
	return nil, false
}

func modelParameterValuesToSand(values []ModelParameterValue) []sandModelParameter {
	out := make([]sandModelParameter, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value.ID) == "" {
			continue
		}
		out = append(out, sandModelParameter{ID: value.ID, Value: value.Value})
	}
	return out
}

func sandRequestedModelParameters(model string) []sandModelParameter {
	lower := strings.ToLower(strings.TrimSpace(model))
	var parameters []sandModelParameter
	normalized := strings.NewReplacer("_", "-", " ", "-").Replace(lower)
	if strings.Contains(normalized, "nothinking") {
		parameters = append(parameters, sandModelParameter{ID: "thinking", Value: "false"})
	} else if strings.Contains(normalized, "thinking") {
		parameters = append(parameters, sandModelParameter{ID: "thinking", Value: "true"})
	}

	effort := ""
	switch {
	case strings.Contains(normalized, "extra-high") || strings.Contains(normalized, "extrahigh"):
		effort = "xhigh"
	case strings.Contains(normalized, "-xhigh") || strings.HasSuffix(normalized, "xhigh"):
		effort = "xhigh"
	case strings.Contains(normalized, "-max") || strings.HasSuffix(normalized, "-max") || strings.Contains(normalized, "thinking-max"):
		effort = "max"
	case strings.Contains(normalized, "-high") || strings.HasSuffix(normalized, "-high"):
		effort = "high"
	case strings.Contains(normalized, "-medium") || strings.HasSuffix(normalized, "-medium"):
		effort = "medium"
	case strings.Contains(normalized, "-low") || strings.HasSuffix(normalized, "-low"):
		effort = "low"
	case strings.Contains(normalized, "-minimal") || strings.HasSuffix(normalized, "minimal"):
		effort = "minimal"
	case strings.Contains(normalized, "-none") || strings.HasSuffix(normalized, "none"):
		effort = "none"
	case strings.Contains(normalized, "-fast") || strings.HasSuffix(normalized, "fast"):
		effort = "fast"
	}
	if effort != "" {
		parameters = append(parameters, sandModelParameter{ID: "effort", Value: effort})
	}
	for _, context := range []string{"300k", "272k", "200k", "1m"} {
		if strings.Contains(normalized, context) {
			parameters = append(parameters, sandModelParameter{ID: "context", Value: context})
			break
		}
	}
	if strings.Contains(normalized, "fable") && !sandHasParameter(parameters, "context", "1m") {
		parameters = append(parameters, sandModelParameter{ID: "context", Value: "1m"})
	}
	return parameters
}

// encodeSandToolParameters encodes InferenceAgentTool.parameters, which is a
// google.protobuf.Struct, not a JSON string and not google.protobuf.Value.
//
//nolint:unused // retained for compatibility with legacy Sand tool encoding.
func encodeSandToolParameters(schema string) []byte {
	var value any
	if err := json.Unmarshal([]byte(schema), &value); err != nil {
		value = nil
	}
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		object = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return encodeProtoStruct(object)
}

func buildSandMessage(role int, text string) []byte {
	body := pbField(1, 0, pbVarint(uint64(role)))
	if text != "" {
		body = append(body, protoString(2, text)...)
	}
	return body
}

func sandWireModel(model string) string {
	return sandCatalogModelID(sandResolvedModel(model))
}

func sandResolvedModel(model string) string {
	model = strings.TrimSpace(model)
	if resolved, ok := ResolveClaudeCodeModel(model); ok {
		return resolved
	}
	if fallback, ok := fallbackClaudeCodeModel(model); ok {
		return fallback
	}
	return normalizeCursorModel(model)
}

func sandCatalogModelID(model string) string {
	model = strings.TrimSpace(model)
	for _, meta := range claudeResolutionModels() {
		for _, alias := range meta.Aliases {
			if strings.EqualFold(alias, model) {
				return meta.ID
			}
		}
		for _, variant := range meta.Variants {
			if strings.EqualFold(variant.Slug, model) {
				return meta.ID
			}
		}
	}
	if familyOf(model) != "claude" {
		return model
	}
	lower := strings.ToLower(model)
	fastStripped := strings.TrimSuffix(lower, "-fast")
	for _, suffix := range []string{
		"-thinking-xhigh",
		"-thinking-medium",
		"-thinking-high",
		"-thinking-low",
		"-thinking-max",
		"-xhigh-thinking",
		"-medium-thinking",
		"-high-thinking",
		"-low-thinking",
		"-max-thinking",
		"-thinking",
		"-xhigh",
		"-minimal",
		"-medium",
		"-high",
		"-low",
		"-max",
		"-none",
	} {
		if strings.HasSuffix(fastStripped, suffix) {
			return model[:len(model)-(len(lower)-len(strings.TrimSuffix(fastStripped, suffix)))]
		}
	}
	return model
}

type sandResponseEvent struct {
	Text      string
	Model     string
	RequestID string
	ToolParts []sandToolPart
}

type sandToolPart struct {
	ID        string
	Name      string
	Arguments string
	Final     bool
}

// parseSandInferenceResponse parses one InferenceStreamResponse protobuf
// payload. Text and tool calls are deliberately read from the InferenceService
// shape, not interaction_update from agent.v1.
func parseSandInferenceResponse(data []byte) (sandResponseEvent, error) {
	parts, err := pbParse(data)
	if err != nil {
		return sandResponseEvent{}, err
	}
	var event sandResponseEvent
	for _, part := range parts {
		switch part.Num {
		case 1:
			nested, nestedErr := pbParse(part.Data)
			if nestedErr != nil {
				continue
			}
			if text, ok := pbFirst(nested, 1); ok && text.Wire == 2 {
				event.Text += string(text.Data)
			}
		case 2:
			nested, nestedErr := pbParse(part.Data)
			if nestedErr != nil {
				continue
			}
			var tool sandToolPart
			for _, field := range nested {
				switch field.Num {
				case 1:
					if field.Wire == 2 {
						tool.ID = string(field.Data)
					}
				case 2:
					if field.Wire == 2 {
						tool.Name = string(field.Data)
					}
				case 3:
					if field.Wire == 2 {
						tool.Arguments += string(field.Data)
					}
				case 4:
					tool.Final = field.Wire == 0 && field.Value != 0
				}
			}
			event.ToolParts = append(event.ToolParts, tool)
		case 4:
			nested, nestedErr := pbParse(part.Data)
			if nestedErr != nil {
				continue
			}
			if model, ok := pbFirst(nested, 2); ok && model.Wire == 2 {
				event.Model = string(model.Data)
			}
		case 7:
			nested, nestedErr := pbParse(part.Data)
			if nestedErr != nil {
				continue
			}
			if requestID, ok := pbFirst(nested, 1); ok && requestID.Wire == 2 {
				event.RequestID = string(requestID.Data)
			}
		}
	}
	return event, nil
}

func mergeSandToolParts(parts []sandToolPart) []ToolCall {
	type accumulated struct {
		id   string
		name string
		args string
	}
	byID := make(map[string]*accumulated)
	var order []string
	for _, part := range parts {
		id := strings.TrimSpace(part.ID)
		if newline := strings.IndexByte(id, '\n'); newline >= 0 {
			id = strings.TrimSpace(id[:newline])
		}
		if id == "" {
			id = fmt.Sprintf("sand-tool-%d", len(order)+1)
		}
		item, ok := byID[id]
		if !ok {
			item = &accumulated{id: id}
			byID[id] = item
			order = append(order, id)
		}
		if strings.TrimSpace(part.Name) != "" {
			item.name = strings.TrimSpace(part.Name)
		}
		fragment := strings.TrimSpace(part.Arguments)
		if fragment == "" {
			continue
		}
		if item.args == "" {
			item.args = fragment
			continue
		}
		if json.Valid([]byte(fragment)) && (!json.Valid([]byte(item.args)) || len(fragment) >= len(item.args)) {
			item.args = fragment
		} else if !json.Valid([]byte(item.args)) {
			item.args += fragment
		}
	}
	out := make([]ToolCall, 0, len(order))
	for _, id := range order {
		item := byID[id]
		if item.name == "" {
			continue
		}
		args := strings.TrimSpace(item.args)
		if !json.Valid([]byte(args)) {
			if start, end := strings.Index(args, "{"), strings.LastIndex(args, "}"); start >= 0 && end > start {
				candidate := args[start : end+1]
				if json.Valid([]byte(candidate)) {
					args = candidate
				}
			}
		}
		if !json.Valid([]byte(args)) {
			args = "{}"
		}
		out = append(out, ToolCall{ID: id, Name: item.name, Input: json.RawMessage(args)})
	}
	return out
}

func (c *Client) runSandStream(ctx context.Context, agentHTTP *http.Client, a *Account, in AgentRequest,
	model, requestID, traceID string, reqStart time.Time, onText func(string),
	onReasoning func(string), onTool func(ToolCall)) (bool, error) {
	_ = requestID
	_ = onReasoning
	body := wrapFrame(buildSandInferenceRequest(model, in, requestID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cursorAPIURL(sandStreamURL), bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header = c.buildHeadersWithType(a, "application/connect+proto", "sand")
	req.Header.Set("Accept", "application/connect+proto")
	req.Header.Set("x-cursor-streaming", "true")
	applyCursorProxyAuth(req)
	resp, err := agentHTTP.Do(req)
	if err != nil {
		logAgentDebug(traceID, "InferenceService/Stream HTTP 失败 err=%v", err)
		return false, fmt.Errorf("InferenceService/Stream: %w", err)
	}
	logAgentDebug(traceID, "+%s InferenceService/Stream 返回 HTTP %d model=%s",
		time.Since(reqStart).Truncate(time.Millisecond), resp.StatusCode, sandWireModel(model))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		detail := summarizeUpstreamHTTPError(responseBody)
		if detail != "" {
			return false, sandUpstreamError(fmt.Sprintf("InferenceService/Stream HTTP %d: %s", resp.StatusCode, detail))
		}
		return false, sandUpstreamError(fmt.Sprintf("InferenceService/Stream HTTP %d", resp.StatusCode))
	}
	defer func() { _ = resp.Body.Close() }()

	reader := NewStreamReader(resp.Body)
	hardTimeout := c.agentTimeout
	if hardTimeout <= 0 {
		hardTimeout = 600 * time.Second
	}
	firstTokenTimeout := c.firstToken
	if firstTokenTimeout <= 0 {
		firstTokenTimeout = 90 * time.Second
	}
	hard := time.NewTimer(hardTimeout)
	defer hard.Stop()
	first := time.NewTimer(firstTokenTimeout)
	defer first.Stop()
	const silenceGap = 45 * time.Second
	activity := time.NewTimer(silenceGap)
	defer activity.Stop()

	type frameResult struct {
		flag    byte
		payload []byte
		err     error
	}
	frameCh := make(chan frameResult, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			flag, payload, readErr := reader.ReadFrame()
			select {
			case frameCh <- frameResult{flag: flag, payload: payload, err: readErr}:
			case <-ctx.Done():
				return
			}
			if readErr != nil {
				return
			}
		}
	}()
	defer func() {
		_ = reader.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}()

	produced := false
	var toolParts []sandToolPart
	firstC := first.C
loop:
	for {
		select {
		case <-ctx.Done():
			return produced, ctx.Err()
		case <-hard.C:
			return produced, fmt.Errorf("对话超时: 超过 %v 硬上限", hardTimeout)
		case <-activity.C:
			return produced, fmt.Errorf("上游静默: %v 内无任何数据帧", silenceGap)
		case firstEvent := <-firstC:
			_ = firstEvent
			if !produced {
				return false, fmt.Errorf("首字超时: %v 内未产出任何内容", firstTokenTimeout)
			}
		case result := <-frameCh:
			if result.err != nil {
				if result.err == io.EOF || result.err == io.ErrUnexpectedEOF {
					return produced, ErrIncompleteUpstreamStream
				}
				return produced, result.err
			}
			if result.flag&0x02 != 0 {
				if detail := parseStreamError(result.payload); detail != "" {
					return produced, sandUpstreamError(fmt.Sprintf("InferenceService/Stream: %s", detail))
				}
				break loop
			}
			event, parseErr := parseSandInferenceResponse(result.payload)
			if parseErr != nil {
				activity.Reset(silenceGap)
				continue
			}
			if event.Text != "" {
				produced = true
				if onText != nil {
					onText(event.Text)
				}
			}
			if len(event.ToolParts) > 0 {
				produced = true
				toolParts = append(toolParts, event.ToolParts...)
			}
			activity.Reset(silenceGap)
			if produced {
				if !first.Stop() {
					select {
					case <-first.C:
					default:
					}
				}
				firstC = nil
			}
			continue
		}
	}
	for _, call := range mergeSandToolParts(toolParts) {
		if onTool != nil {
			onTool(call)
		}
	}
	logAgentDebug(traceID, "InferenceService/Stream 完成 elapsed=%s model=%s text_or_tools=%v tools=%d",
		time.Since(reqStart).Truncate(time.Millisecond), sandWireModel(model), produced, len(toolParts))
	return produced, nil
}

// sandUpstreamError preserves the upstream detail for diagnostics, but marks
// known request-schema rejections as terminal so scheduling never burns every
// account on an identical malformed Sand payload.
func sandUpstreamError(detail string) error {
	if isSandRequestSchemaError(detail) {
		return fmt.Errorf("%w: %s", ErrInvalidUpstreamRequest, detail)
	}
	return fmt.Errorf("%s", detail)
}

func isSandRequestSchemaError(detail string) bool {
	lower := strings.ToLower(detail)
	for _, marker := range []string{
		"requested_model is required",
		"requested model is required",
		"requested_model",
		"requested model",
		"sand traffic is not supported on this endpoint",
		"invalid protobuf",
		"invalid end group tag",
		"failed to parse protobuf",
		"cannot parse protobuf",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
