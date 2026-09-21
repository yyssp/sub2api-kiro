package cursor

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ⚠️ 以下测试因依赖 ai2api 的 Handler/Manager/Store（未移植）而移除：
//   - TestSandQuotaSelectionAndFailureStayOnSandSurface
// 对应行为应在 sub2api 侧用 service 层 + 网关做集成测试覆盖。

//   - TestSandSchemaErrorIsTerminalProtocolError（依赖未移植符号 protocolErrorCode）

func TestListModelsFullPreservesSandCatalogParameters(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != modelsURL {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"models":[{
				"name":"claude-opus-5-medium",
				"idAliases":["opus"],
				"legacySlugs":["claude-opus-5-medium"],
				"supportsImages":true,
				"supportsThinking":true,
				"supportsAgent":true,
				"supportsMaxMode":true,
				"parameterDefinitions":[{
					"id":"effort",
					"parameterType":{"enumParameter":{"values":[{"value":"medium"},{"value":"high"}]}}
				},{
					"id":"context",
					"parameterType":{"enumParameter":{"values":[{"value":"1m"}]}}
				}],
				"variants":[{
					"legacySlug":"claude-opus-5-thinking-high",
					"isMaxMode":true,
					"parameterValues":[
						{"id":"thinking","value":"true"},
						{"id":"effort","value":"high"}
					]
				}]
			}]
		}`))
	}))
	t.Cleanup(server.Close)
	ConfigureUpstream(server.URL, "")
	t.Cleanup(func() { ConfigureUpstream("", "") })

	client := NewClient()
	client.shared = server.Client()
	models, err := client.ListModelsFull(&Account{AccessToken: "test-token"})
	if err != nil {
		t.Fatalf("ListModelsFull: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("models=%d, want 1", len(models))
	}
	meta := models[0]
	if len(meta.Aliases) != 2 || meta.Aliases[0] != "opus" ||
		meta.Aliases[1] != "claude-opus-5-medium" {
		t.Fatalf("aliases=%v", meta.Aliases)
	}
	if len(meta.Parameters) != 2 || meta.Parameters[0].ID != "effort" ||
		strings.Join(meta.Parameters[0].Values, ",") != "medium,high" {
		t.Fatalf("parameter definitions=%+v", meta.Parameters)
	}
	if len(meta.Variants) != 1 || meta.Variants[0].Slug != "claude-opus-5-thinking-high" ||
		len(meta.Variants[0].Parameters) != 2 ||
		meta.Variants[0].Parameters[1].Value != "high" {
		t.Fatalf("variants=%+v", meta.Variants)
	}
}

func TestBuildSandInferenceRequestMatchesInferenceServiceSchema(t *testing.T) {
	SetLiveModels(nil)
	t.Cleanup(func() { SetLiveModels(nil) })

	body := buildSandInferenceRequest("sonnet", AgentRequest{
		System:  "system instructions",
		Message: "user request",
		Tools: []ToolDef{{
			Name:        "Read",
			Description: "read a file",
			InputSchema: `{"type":"object","properties":{"path":{"type":"string"}}}`,
		}},
		MaxMode: true,
	}, "conversation-test")
	parts, err := pbParse(body)
	if err != nil {
		t.Fatalf("parse Sand request: %v", err)
	}
	var messages, tools int
	for _, part := range parts {
		switch part.Num {
		case 1:
			messages++
			message, err := pbParse(part.Data)
			if err != nil {
				t.Fatalf("parse Sand message: %v", err)
			}
			role, ok := pbFirst(message, 1)
			if !ok || role.Wire != 0 {
				t.Fatalf("message role missing: %+v", message)
			}
			text, ok := pbFirst(message, 2)
			if !ok {
				t.Fatalf("message text missing: %+v", message)
			}
			switch messages {
			case 1:
				if role.Value != sandRoleSystem || string(text.Data) != "system instructions" {
					t.Fatalf("system message=%+v, want role=%d text=%q", message, sandRoleSystem, "system instructions")
				}
			case 2:
				if role.Value != sandRoleUser || string(text.Data) != "user request" {
					t.Fatalf("user message=%+v, want role=%d text=%q", message, sandRoleUser, "user request")
				}
			}
		case 2:
			tools++
		}
	}
	if messages != 2 || tools != 0 {
		t.Fatalf("Sand request messages=%d tools=%d, want 2/0", messages, tools)
	}
	conversationID, ok := pbFirst(parts, 8)
	if !ok || conversationID.Wire != 2 || string(conversationID.Data) != "conversation-test" {
		t.Fatalf("conversation_id=%+v, want conversation-test", conversationID)
	}
	requested, ok := pbFirst(parts, 7)
	if !ok {
		t.Fatalf("requested_model missing: %+v", parts)
	}
	requestedParts, err := pbParse(requested.Data)
	if err != nil {
		t.Fatalf("parse requested_model: %v", err)
	}
	modelID, ok := pbFirst(requestedParts, 1)
	if !ok || modelID.Wire != 2 || string(modelID.Data) != "claude-sonnet-5" {
		t.Fatalf("requested model_id=%+v, want claude-sonnet-5", modelID)
	}
	maxMode, ok := pbFirst(requestedParts, 2)
	if !ok || maxMode.Wire != 0 || maxMode.Value != 1 {
		t.Fatalf("requested max_mode=%+v, want true", maxMode)
	}
	for _, unsupported := range []int{5, 6} {
		if _, ok := pbFirst(parts, unsupported); ok {
			t.Fatalf("InferenceStreamRequest must not include unsupported field %d: %+v", unsupported, parts)
		}
	}

	defaultBody := buildSandInferenceRequest("default", AgentRequest{Message: "auto"}, "conversation-default")
	defaultParts, err := pbParse(defaultBody)
	if err != nil {
		t.Fatalf("parse default Sand request: %v", err)
	}
	defaultRequested, _ := pbFirst(defaultParts, 7)
	defaultRequestedParts, err := pbParse(defaultRequested.Data)
	if err != nil {
		t.Fatalf("parse default requested_model: %v", err)
	}
	defaultModelID, _ := pbFirst(defaultRequestedParts, 1)
	if string(defaultModelID.Data) != "default" {
		t.Fatalf("default model_id=%q, want default", defaultModelID.Data)
	}
}

func TestSandRequestedModelCarriesCursorParameters(t *testing.T) {
	SetLiveModels(nil)
	t.Cleanup(func() { SetLiveModels(nil) })

	body := buildSandInferenceRequest("fable", AgentRequest{Message: "check"}, "conversation-fable")
	parts, err := pbParse(body)
	if err != nil {
		t.Fatalf("parse fable Sand request: %v", err)
	}
	requested, ok := pbFirst(parts, 7)
	if !ok {
		t.Fatal("fable requested_model missing")
	}
	requestedParts, err := pbParse(requested.Data)
	if err != nil {
		t.Fatalf("parse fable requested_model: %v", err)
	}
	modelID, _ := pbFirst(requestedParts, 1)
	if string(modelID.Data) != "claude-fable-5" {
		t.Fatalf("fable model_id=%q, want claude-fable-5", modelID.Data)
	}
	got := map[string]string{}
	for _, part := range requestedParts {
		if part.Num != 3 || part.Wire != 2 {
			continue
		}
		parameter, parseErr := pbParse(part.Data)
		if parseErr != nil {
			t.Fatalf("parse parameter: %v", parseErr)
		}
		id, idOK := pbFirst(parameter, 1)
		value, valueOK := pbFirst(parameter, 2)
		if idOK && valueOK {
			got[string(id.Data)] = string(value.Data)
		}
	}
	if got["context"] != "1m" {
		t.Fatalf("fable parameters=%v, want context=1m", got)
	}

	thinking := buildSandRequestedModel("claude-fable-5", "claude-fable-5-thinking-max", false)
	thinkingParts, err := pbParse(thinking)
	if err != nil {
		t.Fatalf("parse thinking requested_model: %v", err)
	}
	got = map[string]string{}
	for _, part := range thinkingParts {
		if part.Num != 3 {
			continue
		}
		parameter, parseErr := pbParse(part.Data)
		if parseErr != nil {
			t.Fatalf("parse thinking parameter: %v", parseErr)
		}
		id, _ := pbFirst(parameter, 1)
		value, _ := pbFirst(parameter, 2)
		got[string(id.Data)] = string(value.Data)
	}
	if got["thinking"] != "true" || got["effort"] != "max" || got["context"] != "1m" {
		t.Fatalf("thinking parameters=%v, want thinking=true effort=max context=1m", got)
	}
}

func TestSandRequestedModelAcceptsBracketParametersAndDisablesMaxForOneMillionContext(t *testing.T) {
	SetLiveModels(nil)
	t.Cleanup(func() { SetLiveModels(nil) })

	body := buildSandInferenceRequest(
		"claude-opus-5[effort=high,context=1m]",
		AgentRequest{Message: "check", MaxMode: true},
		"conversation-bracket",
	)
	parts, err := pbParse(body)
	if err != nil {
		t.Fatalf("parse bracket Sand request: %v", err)
	}
	requested, ok := pbFirst(parts, 7)
	if !ok {
		t.Fatal("bracket requested_model missing")
	}
	requestedParts, err := pbParse(requested.Data)
	if err != nil {
		t.Fatalf("parse bracket requested_model: %v", err)
	}
	modelID, ok := pbFirst(requestedParts, 1)
	if !ok || string(modelID.Data) != "claude-opus-5" {
		t.Fatalf("bracket model_id=%q, want claude-opus-5", modelID.Data)
	}
	if _, ok := pbFirst(requestedParts, 2); ok {
		t.Fatal("context=1m request must not also set max_mode")
	}
	got := map[string]string{}
	for _, part := range requestedParts {
		if part.Num != 3 {
			continue
		}
		parameter, parseErr := pbParse(part.Data)
		if parseErr != nil {
			t.Fatalf("parse bracket parameter: %v", parseErr)
		}
		id, idOK := pbFirst(parameter, 1)
		value, valueOK := pbFirst(parameter, 2)
		if idOK && valueOK {
			got[string(id.Data)] = string(value.Data)
		}
	}
	if got["effort"] != "high" || got["context"] != "1m" {
		t.Fatalf("bracket parameters=%v, want effort=high context=1m", got)
	}
}

func TestSandRequestedModelAnyExplicitContextDisablesMaxMode(t *testing.T) {
	for _, contextValue := range []string{"200k", "272k", "300k"} {
		t.Run(contextValue, func(t *testing.T) {
			requested := buildSandRequestedModelWithParameters(
				"gpt-5.6-sol",
				[]sandModelParameter{
					{ID: "effort", Value: "high"},
					{ID: "context", Value: contextValue},
				},
				true,
			)
			parts, err := pbParse(requested)
			if err != nil {
				t.Fatalf("parse requested model: %v", err)
			}
			if _, ok := pbFirst(parts, 2); ok {
				t.Fatalf("context=%s must suppress max_mode", contextValue)
			}
		})
	}
}

func TestSandCatalogVariantNormalizesToParentAndKeepsMaxMode(t *testing.T) {
	SetLiveModels([]ModelMeta{{
		ID:     "gpt-5.6-sol",
		Family: "gpt",
		Variants: []ModelVariant{{
			Slug:    "gpt-5.6-sol-extra-high",
			MaxMode: true,
			Parameters: []ModelParameterValue{
				{ID: "effort", Value: "xhigh"},
			},
		}},
	}})
	t.Cleanup(func() { SetLiveModels(nil) })

	selection := parseSandModelSelection("gpt-5.6-sol-extra-high")
	if selection.ModelID != "gpt-5.6-sol" {
		t.Fatalf("catalog model=%q, want gpt-5.6-sol", selection.ModelID)
	}
	if !selection.MaxMode {
		t.Fatal("max-mode catalog variant must preserve max mode")
	}
	if len(selection.Parameters) != 1 ||
		selection.Parameters[0].ID != "effort" ||
		selection.Parameters[0].Value != "xhigh" {
		t.Fatalf("variant parameters=%+v", selection.Parameters)
	}
}

func TestSandCatalogModelKeepsExplicitGrokVariantID(t *testing.T) {
	if got := sandCatalogModelID("cursor-grok-4.5-medium"); got != "cursor-grok-4.5-medium" {
		t.Fatalf("grok catalog model=%q, want explicit variant ID", got)
	}
}

func TestSandRequestedModelNormalizesCurrentTierSpellings(t *testing.T) {
	tests := []struct {
		model string
		id    string
		value string
	}{
		{"gpt-5.5-extra-high", "effort", "xhigh"},
		{"claude-sonnet-5-nothinking-high", "thinking", "false"},
		{"claude-sonnet-5-nothinking-high", "effort", "high"},
		{"gpt-5.6-sol-300k", "context", "300k"},
	}
	for _, tt := range tests {
		t.Run(tt.model+"/"+tt.id, func(t *testing.T) {
			parameters := sandRequestedModelParameters(tt.model)
			found := false
			for _, parameter := range parameters {
				if parameter.ID == tt.id && parameter.Value == tt.value {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("parameters=%+v, missing %s=%s", parameters, tt.id, tt.value)
			}
		})
	}
}

func TestSandResponseParsesTextMetadataAndToolParts(t *testing.T) {
	first, err := parseSandInferenceResponse(protoMessage(1, protoString(1, "hello ")))
	if err != nil {
		t.Fatalf("parse first Sand response: %v", err)
	}
	secondPayload := protoString(1, "world")
	secondPayload = append(secondPayload, protoString(2, "claude-sonnet-5-medium")...)
	second, err := parseSandInferenceResponse(protoMessage(1, secondPayload))
	if err != nil {
		t.Fatalf("parse second Sand response: %v", err)
	}
	if got := first.Text + second.Text; got != "hello world" {
		t.Fatalf("text=%q, want hello world", got)
	}

	tool := protoString(1, "tool-1\nignored")
	tool = append(tool, protoString(2, "Read")...)
	tool = append(tool, protoString(3, "{")...)
	tool = append(tool, protoBool(4, true)...)
	event, err := parseSandInferenceResponse(protoMessage(2, tool))
	if err != nil {
		t.Fatalf("parse tool Sand response: %v", err)
	}
	if len(event.ToolParts) != 1 || event.ToolParts[0].ID != "tool-1\nignored" ||
		event.ToolParts[0].Name != "Read" || event.ToolParts[0].Arguments != "{" || !event.ToolParts[0].Final {
		t.Fatalf("tool event=%+v", event)
	}

	metadata := protoString(2, "request-model")
	metadataEvent, err := parseSandInferenceResponse(protoMessage(4, metadata))
	if err != nil {
		t.Fatalf("parse metadata Sand response: %v", err)
	}
	if metadataEvent.Model != "request-model" {
		t.Fatalf("metadata model=%q, want request-model", metadataEvent.Model)
	}
	requestMeta, err := parseSandInferenceResponse(protoMessage(7, protoString(1, "request-1")))
	if err != nil {
		t.Fatalf("parse request metadata: %v", err)
	}
	if requestMeta.RequestID != "request-1" {
		t.Fatalf("request id=%q, want request-1", requestMeta.RequestID)
	}
}

func TestMergeSandToolPartsCombinesFragmentsAndPrefersCompleteJSON(t *testing.T) {
	got := mergeSandToolParts([]sandToolPart{
		{ID: "call-1\nextra", Name: "Read", Arguments: "{"},
		{ID: "call-1", Arguments: `"path":"/tmp/a.txt"}`},
		{ID: "call-2", Name: "Bash", Arguments: `{"command":"pwd"}`},
		{ID: "call-2", Name: "Bash", Arguments: `{"command":"ls","recursive":true}`},
	})
	if len(got) != 2 {
		t.Fatalf("merged tools=%+v, want 2", got)
	}
	if got[0].ID != "call-1" || got[0].Name != "Read" || string(got[0].Input) != `{"path":"/tmp/a.txt"}` {
		t.Fatalf("fragmented tool=%+v", got[0])
	}
	if got[1].ID != "call-2" || got[1].Name != "Bash" || string(got[1].Input) != `{"command":"ls","recursive":true}` {
		t.Fatalf("complete tool replacement=%+v", got[1])
	}
}

func TestSandStreamRejectsTrailerErrorAndIncompleteEOF(t *testing.T) {
	t.Run("trailer error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/connect+proto")
			w.WriteHeader(http.StatusOK)
			payload := []byte(`{"error":{"code":"invalid_argument","message":"Sand rejected request"}}`)
			frame := make([]byte, 5+len(payload))
			frame[0] = 0x02
			binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
			copy(frame[5:], payload)
			_, _ = w.Write(frame)
		}))
		t.Cleanup(server.Close)
		ConfigureUpstream(server.URL, "")
		ConfigureSandModels([]string{"cursor-grok-4.5-medium"})
		t.Cleanup(func() {
			ConfigureUpstream("", "")
			ConfigureSandModels(nil)
		})
		client := NewClient()
		client.shared = server.Client()
		client.agentTimeout = time.Second
		client.firstToken = time.Second
		produced, err := client.RunAgentStream(context.Background(), &Account{AccessToken: "test-token"},
			AgentRequest{Model: "cursor-grok-4.5-medium", Message: "error"}, nil, nil, nil)
		if produced || err == nil || !strings.Contains(err.Error(), "Sand rejected request") {
			t.Fatalf("trailer error produced=%v err=%v", produced, err)
		}
	})

	t.Run("incomplete EOF", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/connect+proto")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(wrapFrame(protoMessage(1, protoString(1, "partial"))))
		}))
		t.Cleanup(server.Close)
		ConfigureUpstream(server.URL, "")
		ConfigureSandModels([]string{"cursor-grok-4.5-medium"})
		t.Cleanup(func() {
			ConfigureUpstream("", "")
			ConfigureSandModels(nil)
		})
		client := NewClient()
		client.shared = server.Client()
		client.agentTimeout = time.Second
		client.firstToken = time.Second
		var text strings.Builder
		produced, err := client.RunAgentStream(context.Background(), &Account{AccessToken: "test-token"},
			AgentRequest{Model: "cursor-grok-4.5-medium", Message: "partial"}, func(piece string) {
				_, _ = text.WriteString(piece)
			}, nil, nil)
		if !produced || text.String() != "partial" || err != ErrIncompleteUpstreamStream {
			t.Fatalf("incomplete stream produced=%v text=%q err=%v", produced, text.String(), err)
		}
	})
}

func TestSandModelConfigurationIsExplicitAndNormalized(t *testing.T) {
	ConfigureSandModels([]string{" Cursor/CURSOR-GROK-4.5-MEDIUM ", ""})
	t.Cleanup(func() { ConfigureSandModels(nil) })

	if got := cursorClientTypeForModel("cursor-grok-4.5-medium"); got != "sand" {
		t.Fatalf("configured model surface=%q, want sand", got)
	}
	if got := cursorClientTypeForModel("cursor/cursor-grok-4.5-medium"); got != "sand" {
		t.Fatalf("prefixed configured model surface=%q, want sand", got)
	}
	if got := cursorClientTypeForModel("claude-sonnet-5"); got != "sand" {
		t.Fatalf("claude model surface=%q, want sand", got)
	}
	if !useSandInferenceService("claude-sonnet-5") {
		t.Fatal("claude model must use direct Sand InferenceService")
	}
	if useSandInferenceServiceForRequest("claude-sonnet-5", []ToolDef{{Name: "Read"}}) {
		t.Fatal("claude model with declared tools must use AgentService tool bridge")
	}
	if !useSandInferenceService("cursor-grok-4.5-medium") {
		t.Fatal("explicit configured Sand model must use InferenceService/Stream")
	}
	if got := SandModels(); len(got) != 1 || got[0] != "cursor-grok-4.5-medium" {
		t.Fatalf("sand models=%v, want normalized singleton", got)
	}
}

func TestSandSurfaceUsesInferenceStream(t *testing.T) {
	handlerDone := make(chan error, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		report := func(err error) {
			select {
			case handlerDone <- err:
			default:
			}
		}
		if r.URL.Path != sandStreamURL {
			report(fmt.Errorf("path=%s, want %s", r.URL.Path, sandStreamURL))
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("x-cursor-client-type"); got != "sand" {
			report(fmt.Errorf("client type=%q, want sand", got))
			http.Error(w, "wrong client type", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("Content-Type"); got != "application/connect+proto" {
			report(fmt.Errorf("content-type=%q, want application/connect+proto", got))
			http.Error(w, "wrong content type", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("Accept"); got != "application/connect+proto" {
			report(fmt.Errorf("accept=%q, want application/connect+proto", got))
			http.Error(w, "missing streaming header", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("x-cursor-streaming"); got != "true" {
			report(fmt.Errorf("x-cursor-streaming=%q, want true", got))
			http.Error(w, "missing x-cursor-streaming header", http.StatusBadRequest)
			return
		}

		reader := NewStreamReader(r.Body)
		flag, first, err := reader.ReadFrame()
		if err != nil {
			report(fmt.Errorf("read initial frame: %w", err))
			return
		}
		if flag != 0 {
			report(fmt.Errorf("initial frame flag=%d, want 0", flag))
			return
		}
		initial, err := pbParse(first)
		if err != nil {
			report(fmt.Errorf("parse initial frame: %w", err))
			return
		}
		message, ok := pbFirst(initial, 1)
		if !ok || message.Wire != 2 {
			report(fmt.Errorf("initial InferenceStreamRequest missing messages: %v", initial))
			return
		}
		messageParts, err := pbParse(message.Data)
		if err != nil {
			report(fmt.Errorf("parse message: %w", err))
			return
		}
		if role, ok := pbFirst(messageParts, 1); !ok || role.Wire != 0 || role.Value != sandRoleUser {
			report(fmt.Errorf("user role=%+v, want %d", role, sandRoleUser))
			return
		}
		if text, ok := pbFirst(messageParts, 2); !ok || string(text.Data) != "verify request surface" {
			report(fmt.Errorf("user text=%q, want verify request surface", text.Data))
			return
		}
		conversationID, ok := pbFirst(initial, 8)
		if !ok || conversationID.Wire != 2 || strings.TrimSpace(string(conversationID.Data)) == "" {
			report(fmt.Errorf("missing conversation_id: %v", initial))
			return
		}
		requested, ok := pbFirst(initial, 7)
		if !ok {
			report(fmt.Errorf("missing requested_model: %v", initial))
			return
		}
		requestedParts, parseErr := pbParse(requested.Data)
		if parseErr != nil {
			report(fmt.Errorf("parse requested_model: %w", parseErr))
			return
		}
		modelID, ok := pbFirst(requestedParts, 1)
		if !ok || string(modelID.Data) != "cursor-grok-4.5-medium" {
			report(fmt.Errorf("model_id=%q, want cursor-grok-4.5-medium", modelID.Data))
			return
		}

		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			report(fmt.Errorf("test upstream does not support flushing"))
			return
		}
		// InferenceStreamResponse.field1 is the text event; its nested
		// field1 is the UTF-8 text itself. Do not wrap the nested string as
		// another message, otherwise the parser would receive protobuf wire
		// bytes ("\n\x11...") as user-visible content.
		text := protoString(1, "sand-inference-ok")
		if _, err := w.Write(wrapFrame(protoMessage(1, text))); err != nil {
			report(fmt.Errorf("write text response: %w", err))
			return
		}
		if _, err := w.Write([]byte{0x02, 0, 0, 0, 0}); err != nil {
			report(fmt.Errorf("write final response: %w", err))
			return
		}
		flusher.Flush()
		report(nil)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	ConfigureUpstream(server.URL, "")
	ConfigureSandModels([]string{"cursor-grok-4.5-medium"})
	SetLiveModels([]ModelMeta{{ID: "cursor-grok-4.5-medium", Family: "grok", Vision: true, Tools: true}})
	t.Cleanup(func() {
		ConfigureUpstream("", "")
		ConfigureSandModels(nil)
		SetLiveModels(nil)
	})

	client := NewClient()
	client.shared = server.Client()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var text strings.Builder
	produced, err := client.RunAgentStream(ctx, &Account{AccessToken: "test-token"}, AgentRequest{
		Model:   "cursor-grok-4.5-medium",
		Message: "verify request surface",
	}, func(piece string) {
		_, _ = text.WriteString(piece)
	}, nil, nil)
	if err != nil || !produced {
		t.Fatalf("RunAgentStream produced=%v err=%v", produced, err)
	}
	if got := text.String(); got != "sand-inference-ok" {
		t.Fatalf("text=%q, want sand-inference-ok", got)
	}
	if err := <-handlerDone; err != nil {
		t.Fatal(err)
	}
}
